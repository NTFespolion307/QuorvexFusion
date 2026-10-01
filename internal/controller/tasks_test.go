package controller

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/NTFespolion307/QuorvexFusion/internal/clusterpb"
	"github.com/NTFespolion307/QuorvexFusion/internal/store"
)

// These tests drive the task lifecycle directly, with fake nodes whose
// "connection" is just the session's send queue.

func newTestController(t *testing.T) (*Controller, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ctl")
	cfg := DefaultConfig(dir)
	if _, err := Init(cfg, "password123"); err != nil {
		t.Fatal(err)
	}
	c, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.store.Close() })
	return c, dir
}

// addNode registers an approved, online node with the given capacity.
func addNode(t *testing.T, c *Controller, id string, cpus float64) *Session {
	t.Helper()
	if _, err := c.store.GetNode(id); err != nil {
		now := time.Now()
		if err := c.store.CreateNode(&store.Node{ID: id, Name: id, Status: store.NodeApproved, PubKeyFP: id,
			CreatedAt: now, ApprovedAt: &now}); err != nil {
			t.Fatal(err)
		}
	}
	return connectNode(c, id, cpus, nil)
}

func connectNode(c *Controller, id string, cpus float64, held []string) *Session {
	hw := &pb.HardwareInfo{Hostname: id, CpuLimit: cpus, LogicalCores: int32(cpus), MemoryBytes: 16 << 30}
	sess := newSession(context.Background(), id, "test", &pb.Hello{Hardware: hw, RunningAttempts: held})
	c.nodeConnected(id, held)
	c.hub.Register(sess)
	return sess
}

func disconnect(c *Controller, s *Session) {
	s.Close(context.Canceled)
	c.hub.Unregister(s)
}

// drain collects the AssignTask (and CancelTask) messages queued for a node.
func drain(s *Session) (assigns []*pb.AssignTask, cancels []string) {
	for {
		select {
		case m := <-s.send:
			switch x := m.Msg.(type) {
			case *pb.ControllerMessage_Assign:
				assigns = append(assigns, x.Assign)
			case *pb.ControllerMessage_Cancel:
				cancels = append(cancels, x.Cancel.AttemptId)
			}
		default:
			return
		}
	}
}

func submit(t *testing.T, c *Controller, spec JobSpec) *store.Job {
	t.Helper()
	j, err := c.SubmitJob(&spec)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func pass(t *testing.T, c *Controller) {
	t.Helper()
	if err := c.schedulePass(); err != nil {
		t.Fatal(err)
	}
}

func exited(attemptID string, code int32) *pb.TaskResult {
	return &pb.TaskResult{AttemptId: attemptID, Outcome: pb.TaskResult_EXITED, ExitCode: code,
		FinishedUnixMs: time.Now().UnixMilli()}
}

func task(t *testing.T, c *Controller, id string) *store.Task {
	t.Helper()
	tk, err := c.store.GetTask(id)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestScheduleRespectsCapacityAndBackfillsOnCompletion(t *testing.T) {
	c, _ := newTestController(t)
	n := addNode(t, c, "n1", 2)
	j := submit(t, c, JobSpec{Command: "echo {i}", Array: "1-3"})

	pass(t, c)
	as, _ := drain(n)
	if len(as) != 2 {
		t.Fatalf("assigned %d tasks to a 2-CPU node, want 2", len(as))
	}
	if as[0].Spec.Command != "echo 1" {
		t.Errorf("command = %q, want {i} substituted", as[0].Spec.Command)
	}
	if u := c.usedResources()["n1"]; u == nil || u.CPUs != 2 {
		t.Errorf("used = %+v", u)
	}

	c.handleTaskStarted("n1", &pb.TaskStarted{AttemptId: as[0].AttemptId, StartedUnixMs: time.Now().UnixMilli()})
	if s := task(t, c, as[0].TaskId).State; s != store.TaskRunning {
		t.Errorf("state after start = %s", s)
	}
	c.handleResult(n, exited(as[0].AttemptId, 0))
	if s := task(t, c, as[0].TaskId).State; s != store.TaskSucceeded {
		t.Errorf("state after exit 0 = %s", s)
	}

	pass(t, c)
	as2, _ := drain(n)
	if len(as2) != 1 || as2[0].TaskId != store.TaskID(j.ID, 3) {
		t.Fatalf("freed CPU not reused: %v", as2)
	}
}

func TestRetriesThenFails(t *testing.T) {
	c, _ := newTestController(t)
	n := addNode(t, c, "n1", 4)
	j := submit(t, c, JobSpec{Command: "false", Retries: 1})
	id := store.TaskID(j.ID, 0)

	pass(t, c)
	as, _ := drain(n)
	c.handleResult(n, exited(as[0].AttemptId, 3))
	tk := task(t, c, id)
	if tk.State != store.TaskQueued || tk.Failures != 1 {
		t.Fatalf("after first failure: state=%s failures=%d, want queued/1", tk.State, tk.Failures)
	}

	pass(t, c)
	as, _ = drain(n)
	if len(as) != 1 || as[0].Attempt != 2 {
		t.Fatalf("retry not assigned: %v", as)
	}
	c.handleResult(n, exited(as[0].AttemptId, 3))
	tk = task(t, c, id)
	if tk.State != store.TaskFailed || tk.ExitCode == nil || *tk.ExitCode != 3 {
		t.Fatalf("after second failure: %+v", tk)
	}
}

func TestLostNodeRequeuesWithoutUsingRetries(t *testing.T) {
	c, _ := newTestController(t)
	n1 := addNode(t, c, "n1", 4)
	j := submit(t, c, JobSpec{Command: "sleep 100"})
	id := store.TaskID(j.ID, 0)

	pass(t, c)
	as, _ := drain(n1)
	disconnect(c, n1)
	c.nodeLost("n1", "test")

	tk := task(t, c, id)
	if tk.State != store.TaskQueued || tk.Failures != 0 || tk.Lost != 1 {
		t.Fatalf("after node loss: state=%s failures=%d lost=%d", tk.State, tk.Failures, tk.Lost)
	}
	if u := c.usedResources()["n1"]; u != nil {
		t.Errorf("lost node still has reservations: %+v", u)
	}

	// Exactly-once: the task moves to n2 and succeeds there. When n1 comes
	// back claiming the old attempt, it is told to kill it, and its late
	// "success" is ignored.
	n2 := addNode(t, c, "n2", 4)
	pass(t, c)
	as2, _ := drain(n2)
	if len(as2) != 1 {
		t.Fatal("task not rescheduled on n2")
	}
	n1 = connectNode(c, "n1", 4, []string{as[0].AttemptId})
	_, cancels := drain(n1)
	c.handleResult(n1, exited(as[0].AttemptId, 0)) // stale
	if s := task(t, c, id).State; s != store.TaskQueued && s != store.TaskAssigned {
		t.Fatalf("stale result changed the task to %s", s)
	}
	c.handleResult(n2, exited(as2[0].AttemptId, 0))
	if s := task(t, c, id).State; s != store.TaskSucceeded {
		t.Fatalf("state = %s", s)
	}
	_ = cancels // CancelTask for the stale attempt is sent before registration (see nodeserver)

	attempts, _ := c.store.ListAttempts(id)
	succeeded := 0
	for _, a := range attempts {
		if a.State == store.AttemptSucceeded {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Errorf("%d succeeded attempts, want exactly 1", succeeded)
	}
}

func TestReconnectWithinGraceAdoptsRunningTask(t *testing.T) {
	c, _ := newTestController(t)
	n := addNode(t, c, "n1", 4)
	j := submit(t, c, JobSpec{Command: "sleep 100"})
	pass(t, c)
	as, _ := drain(n)

	disconnect(c, n)
	c.nodeDisconnected("n1") // grace period starts
	n = connectNode(c, "n1", 4, []string{as[0].AttemptId})

	tk := task(t, c, store.TaskID(j.ID, 0))
	if tk.State != store.TaskAssigned || tk.AttemptID != as[0].AttemptId {
		t.Fatalf("task not adopted: %+v", tk)
	}
	// The grace timer must not fire later and requeue it.
	c.tm.mu.Lock()
	_, pendingTimer := c.tm.offline["n1"]
	c.tm.mu.Unlock()
	if pendingTimer {
		t.Error("grace timer still armed after reconnect")
	}
	c.handleResult(n, exited(as[0].AttemptId, 0))
	if s := task(t, c, tk.ID).State; s != store.TaskSucceeded {
		t.Errorf("state = %s", s)
	}
}

func TestReconnectWithoutAttemptLosesIt(t *testing.T) {
	c, _ := newTestController(t)
	n := addNode(t, c, "n1", 4)
	j := submit(t, c, JobSpec{Command: "sleep 100"})
	pass(t, c)
	drain(n)
	disconnect(c, n)
	connectNode(c, "n1", 4, nil) // worker restarted and has nothing
	if tk := task(t, c, store.TaskID(j.ID, 0)); tk.State != store.TaskQueued || tk.Lost != 1 {
		t.Fatalf("state=%s lost=%d, want queued/1", tk.State, tk.Lost)
	}
}

func TestCancel(t *testing.T) {
	c, _ := newTestController(t)
	n := addNode(t, c, "n1", 1)
	j := submit(t, c, JobSpec{Command: "sleep 100", Array: "0-1"})
	pass(t, c)
	as, _ := drain(n)
	if len(as) != 1 {
		t.Fatalf("assigned %d", len(as))
	}
	if err := c.CancelJob(j.ID); err != nil {
		t.Fatal(err)
	}
	_, cancels := drain(n)
	if len(cancels) != 1 || cancels[0] != as[0].AttemptId {
		t.Fatalf("cancel not sent to worker: %v", cancels)
	}
	for _, i := range []int64{0, 1} {
		if s := task(t, c, store.TaskID(j.ID, i)).State; s != store.TaskCanceled {
			t.Errorf("task %d state = %s", i, s)
		}
	}
	// Resources stay reserved until the worker confirms the process stopped.
	if u := c.usedResources()["n1"]; u == nil {
		t.Error("reservation released before the worker confirmed")
	}
	c.handleResult(n, &pb.TaskResult{AttemptId: as[0].AttemptId, Outcome: pb.TaskResult_CANCELED})
	if u := c.usedResources()["n1"]; u != nil {
		t.Errorf("reservation kept after confirmation: %+v", u)
	}
	if s := task(t, c, as[0].TaskId).State; s != store.TaskCanceled {
		t.Errorf("state = %s", s)
	}
}

func TestLogsOnlyFromOwningNodeAndIdempotent(t *testing.T) {
	c, _ := newTestController(t)
	n := addNode(t, c, "n1", 1)
	addNode(t, c, "n2", 1)
	j := submit(t, c, JobSpec{Command: "echo"})
	pass(t, c)
	as, _ := drain(n)
	id := store.TaskID(j.ID, 0)
	if as[0].TaskId != id {
		t.Fatalf("expected task on n1 (sorted first)")
	}

	c.handleLog("n2", &pb.LogChunk{AttemptId: as[0].AttemptId, Stream: pb.Stream_STDOUT, Data: []byte("evil")})
	c.handleLog("n1", &pb.LogChunk{AttemptId: as[0].AttemptId, Stream: pb.Stream_STDOUT, Offset: 0, Data: []byte("hello ")})
	// A resend after reconnect overlaps what we have.
	c.handleLog("n1", &pb.LogChunk{AttemptId: as[0].AttemptId, Stream: pb.Stream_STDOUT, Offset: 0, Data: []byte("hello world")})
	c.handleLog("n1", &pb.LogChunk{AttemptId: as[0].AttemptId, Stream: pb.Stream_STDERR, Offset: 0, Data: []byte("!")})

	data, _, _, err := c.ReadLog(id, 0, LogStdout, 0, 1<<20)
	if err != nil || string(data) != "hello world" {
		t.Errorf("stdout = %q, %v", data, err)
	}
	data, _, _, _ = c.ReadLog(id, 0, LogCombined, 0, 1<<20)
	if string(data) != "hello world!" {
		t.Errorf("combined = %q", data)
	}
}

func TestRestartRestoresReservations(t *testing.T) {
	c, dir := newTestController(t)
	n := addNode(t, c, "n1", 4)
	j := submit(t, c, JobSpec{Command: "sleep 100", CPUs: 3})
	pass(t, c)
	as, _ := drain(n)
	c.store.Close()

	// A new controller process on the same data dir.
	cfg, _ := LoadConfig(dir)
	c2, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer c2.store.Close()
	if u := c2.usedResources()["n1"]; u == nil || u.CPUs != 3 {
		t.Fatalf("reservation not restored: %+v", u)
	}
	n = connectNode(c2, "n1", 4, []string{as[0].AttemptId})
	c2.handleResult(n, exited(as[0].AttemptId, 0))
	if s := task(t, c2, store.TaskID(j.ID, 0)).State; s != store.TaskSucceeded {
		t.Errorf("state = %s", s)
	}
}

func TestParseArray(t *testing.T) {
	cases := map[string]int{"": 1, "1-500": 500, "0-99:10": 10, "1,4,9": 3, "1-3,2-4": 4}
	for expr, want := range cases {
		got, err := ParseArray(expr)
		if err != nil || len(got) != want {
			t.Errorf("ParseArray(%q) = %d indices, %v; want %d", expr, len(got), err, want)
		}
	}
	for _, bad := range []string{"a", "5-1", "1-10:0", "-3", "0-200000"} {
		if _, err := ParseArray(bad); err == nil {
			t.Errorf("ParseArray(%q) accepted", bad)
		}
	}
}

func TestUnplaceableTaskExplained(t *testing.T) {
	c, _ := newTestController(t)
	addNode(t, c, "n1", 2)
	j := submit(t, c, JobSpec{Command: "x", GPUs: 2})
	pass(t, c)
	if r := c.PendingReason(store.TaskID(j.ID, 0)); r == "" || r == "waiting for free resources" {
		t.Errorf("pending reason = %q, want an explanation about GPUs", r)
	}
}
