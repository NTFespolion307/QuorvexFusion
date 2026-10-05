//go:build linux

package runner

import (
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	pb "github.com/NTFespolion307/QuorvexFusion/internal/clusterpb"
)

// fakeController collects what the runner sends.
type fakeController struct {
	mu      sync.Mutex
	started map[string]bool
	logs    map[string]map[pb.Stream]string
	results map[string]*pb.TaskResult
}

func newFake() *fakeController {
	return &fakeController{started: map[string]bool{}, logs: map[string]map[pb.Stream]string{}, results: map[string]*pb.TaskResult{}}
}

func (f *fakeController) send(m *pb.WorkerMessage) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch x := m.Msg.(type) {
	case *pb.WorkerMessage_TaskStarted:
		f.started[x.TaskStarted.AttemptId] = true
	case *pb.WorkerMessage_Log:
		l := f.logs[x.Log.AttemptId]
		if l == nil {
			l = map[pb.Stream]string{}
			f.logs[x.Log.AttemptId] = l
		}
		// Emulate the controller's offset handling.
		cur := l[x.Log.Stream]
		if int(x.Log.Offset) <= len(cur) {
			l[x.Log.Stream] = cur[:x.Log.Offset] + string(x.Log.Data)
		}
	case *pb.WorkerMessage_Result:
		f.results[x.Result.AttemptId] = x.Result
	}
	return true
}

func (f *fakeController) result(id string) *pb.TaskResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.results[id]
}

func (f *fakeController) log(id string, s pb.Stream) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logs[id][s]
}

func newRunner(t *testing.T, dir string) (*Runner, *fakeController) {
	t.Helper()
	r, err := New(Options{Dir: dir, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	f := newFake()
	r.Connected(f.send, nil, nil)
	return r, f
}

func assign(id, command string, timeout int64) *pb.AssignTask {
	return &pb.AssignTask{AttemptId: id, TaskId: "j1." + id, JobId: "j1", Attempt: 1, ArrayIndex: 7,
		Spec: &pb.TaskSpec{Command: command, Cpus: 2, TimeoutSeconds: timeout}}
}

func waitResult(t *testing.T, f *fakeController, id string, within time.Duration) *pb.TaskResult {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if r := f.result(id); r != nil {
			return r
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no result for %s within %v", id, within)
	return nil
}

func TestRunCapturesOutputAndExitCode(t *testing.T) {
	dir := t.TempDir()
	r, f := newRunner(t, dir)
	r.Start(assign("a1", `echo out; echo err >&2; echo "idx=$CLUSTER_ARRAY_INDEX omp=$OMP_NUM_THREADS gpus=[$CUDA_VISIBLE_DEVICES]"; exit 3`, 0))
	res := waitResult(t, f, "a1", 10*time.Second)
	if res.Outcome != pb.TaskResult_EXITED || res.ExitCode != 3 {
		t.Fatalf("result = %v", res)
	}
	if got := f.log("a1", pb.Stream_STDOUT); got != "out\nidx=7 omp=2 gpus=[]\n" {
		t.Errorf("stdout = %q", got)
	}
	if got := f.log("a1", pb.Stream_STDERR); got != "err\n" {
		t.Errorf("stderr = %q", got)
	}
	// The attempt is kept until acknowledged, then removed.
	if _, err := os.Stat(filepath.Join(dir, "a1", "result.json")); err != nil {
		t.Fatal("result not persisted before ack")
	}
	r.Ack("a1")
	time.Sleep(3 * shipInterval)
	if _, err := os.Stat(filepath.Join(dir, "a1")); !os.IsNotExist(err) {
		t.Error("attempt dir not removed after ack")
	}
}

func TestTimeoutKillsProcessGroup(t *testing.T) {
	r, f := newRunner(t, t.TempDir())
	// The background child must die too (process group kill).
	r.Start(assign("a2", `sleep 60 & echo $! > child.pid; wait`, 1))
	res := waitResult(t, f, "a2", 15*time.Second)
	if res.Outcome != pb.TaskResult_TIMED_OUT {
		t.Fatalf("outcome = %v", res.Outcome)
	}
	pidData, err := os.ReadFile(filepath.Join(r.opts.Dir, "a2", "work", "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(pidData)))
	time.Sleep(100 * time.Millisecond)
	if syscall.Kill(pid, 0) == nil {
		t.Errorf("child process %d survived the timeout", pid)
	}
}

func TestCancel(t *testing.T) {
	r, f := newRunner(t, t.TempDir())
	r.Start(assign("a3", `sleep 60`, 0))
	time.Sleep(300 * time.Millisecond)
	r.Cancel("a3", "user asked")
	res := waitResult(t, f, "a3", 15*time.Second)
	if res.Outcome != pb.TaskResult_CANCELED || res.Error != "user asked" {
		t.Fatalf("result = %v", res)
	}
}

func TestBuffersWhileDisconnected(t *testing.T) {
	r, f := newRunner(t, t.TempDir())
	r.Disconnected()
	r.Start(assign("a4", `echo hello`, 0))
	time.Sleep(time.Second)
	if f.result("a4") != nil {
		t.Fatal("result sent while disconnected")
	}
	// Reconnect: the controller has none of the log yet.
	f2 := newFake()
	r.Connected(f2.send, nil, map[string]*pb.LogOffsets{})
	res := waitResult(t, f2, "a4", 5*time.Second)
	if res.Outcome != pb.TaskResult_EXITED || f2.log("a4", pb.Stream_STDOUT) != "hello\n" {
		t.Fatalf("after reconnect: result=%v stdout=%q", res, f2.log("a4", pb.Stream_STDOUT))
	}
}

func TestRecoverAfterCrash(t *testing.T) {
	dir := t.TempDir()
	// Simulate a worker that died mid-task: an attempt dir with a pid file
	// for a still-running process group and no result.
	orphan := exec.Command("sleep", "60")
	orphan.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := orphan.Start(); err != nil {
		t.Fatal(err)
	}
	defer orphan.Process.Kill()
	ad := filepath.Join(dir, "a5")
	os.MkdirAll(ad, 0o700)
	os.WriteFile(filepath.Join(ad, "pid"), []byte(strconv.Itoa(orphan.Process.Pid)), 0o600)
	os.WriteFile(filepath.Join(ad, "stdout"), []byte("partial output\n"), 0o600)

	r, f := newRunner(t, dir)
	if held := r.Held(); len(held) != 1 || held[0] != "a5" {
		t.Fatalf("held = %v", held)
	}
	res := waitResult(t, f, "a5", 5*time.Second)
	if res.Outcome != pb.TaskResult_LOST {
		t.Errorf("outcome = %v, want LOST", res.Outcome)
	}
	if f.log("a5", pb.Stream_STDOUT) != "partial output\n" {
		t.Errorf("partial log not shipped")
	}
	done := make(chan struct{})
	go func() { orphan.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("orphaned process was not killed")
	}
}
