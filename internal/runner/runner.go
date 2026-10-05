// Package runner executes tasks on a worker and reliably reports them.
//
// Every attempt gets a directory:
//
//	<tasks dir>/<attempt id>/
//	    assign.json  the AssignTask message (what to run)
//	    work/        the task's working directory
//	    stdout       process output, written directly by the process
//	    stderr
//	    pid          process group ID while running
//	    result.json  outcome, written before it is sent
//
// The process writes its output straight to files, so it never blocks on
// the network. A per-attempt loop ships new output to the controller and,
// once the process has exited and all output is shipped, sends the result.
// Nothing is deleted until the controller acknowledges the result, so
// results survive disconnects and even worker restarts.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	pb "github.com/NTFespolion307/QuorvexFusion/internal/clusterpb"
)

const (
	shipInterval = 200 * time.Millisecond
	maxChunk     = 64 << 10
	killGrace    = 10 * time.Second // SIGTERM, then SIGKILL after this
)

// Sender queues a message to the controller. It returns false if the
// connection is gone.
type Sender func(*pb.WorkerMessage) bool

type Options struct {
	Dir string // where attempt directories live
	// TaskUser runs tasks as this user when the worker is root ("" = same
	// user as the worker).
	TaskUser string
	// UseSystemd wraps tasks in `systemd-run --scope` for cgroup CPU and
	// memory limits.
	UseSystemd bool
	// CacheDir holds downloaded input files, shared by all tasks;
	// CacheMaxBytes bounds its size (least recently used files go first).
	CacheDir      string
	CacheMaxBytes int64
	// SharedStorage is this node's mount of storage shared between nodes;
	// inputs given as shared paths are linked from there.
	SharedStorage string
	Log           *slog.Logger
}

type Runner struct {
	opts Options
	log  *slog.Logger
	user *taskUser

	cache *cache

	mu           sync.Mutex
	send         Sender               // nil while disconnected
	client       pb.NodeServiceClient // for file transfers; nil while disconnected
	gen          int                  // bumped on every (re)connect
	attempts     map[string]*attempt
	shuttingDown bool
}

// attempt is one task attempt held by this worker, running or finished.
type attempt struct {
	id     string
	dir    string
	assign *pb.AssignTask

	// stop is cancelled (with the reason as its cause) to stop the attempt,
	// whether it is still downloading inputs or already running.
	stop   context.Context
	stopFn context.CancelCauseFunc
	exited chan struct{} // closed once the process has ended (result set)

	// Guarded by Runner.mu:
	result  *pb.TaskResult
	started int64          // unix ms, 0 until the process started
	resetTo *pb.LogOffsets // set on reconnect: controller's log offsets
	acked   bool
}

// New prepares a runner and recovers attempts left by a previous run of the
// worker: finished ones are re-reported, interrupted ones are killed and
// reported as lost.
func New(opts Options) (*Runner, error) {
	// 0711: tasks running as another user must be able to pass through to
	// their own working directory, but not list other tasks' directories.
	if err := os.MkdirAll(opts.Dir, 0o711); err != nil {
		return nil, err
	}
	_ = os.Chmod(opts.Dir, 0o711)
	u, err := lookupTaskUser(opts.TaskUser)
	if err != nil {
		return nil, err
	}
	if opts.CacheDir == "" {
		opts.CacheDir = filepath.Join(opts.Dir, "..", "cache")
	}
	c, err := newCache(opts.CacheDir, opts.CacheMaxBytes)
	if err != nil {
		return nil, err
	}
	r := &Runner{opts: opts, log: opts.Log, user: u, cache: c, attempts: map[string]*attempt{}}
	if err := r.recover(); err != nil {
		return nil, err
	}
	return r, nil
}

// --- connection handling (called by the worker session) ---

// Connected installs the sender for a new connection. offsets are the
// controller's log positions for attempts it adopted; shipping resumes
// from there (or from zero for attempts it did not mention).
func (r *Runner) Connected(send Sender, client pb.NodeServiceClient, offsets map[string]*pb.LogOffsets) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.send = send
	r.client = client
	r.gen++
	for id, a := range r.attempts {
		off := offsets[id]
		if off == nil {
			off = &pb.LogOffsets{}
		}
		a.resetTo = off
	}
}

func (r *Runner) Disconnected() {
	r.mu.Lock()
	r.send = nil
	r.client = nil
	r.mu.Unlock()
}

// connection returns the current transfer client and connection
// generation, waiting while disconnected.
func (r *Runner) connection(ctx context.Context) (pb.NodeServiceClient, int, error) {
	for {
		r.mu.Lock()
		client, gen := r.client, r.gen
		r.mu.Unlock()
		if client != nil {
			return client, gen, nil
		}
		select {
		case <-ctx.Done():
			return nil, 0, context.Cause(ctx)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (r *Runner) currentGen() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gen
}

// Held lists every attempt this worker still holds, for the Hello message.
func (r *Runner) Held() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.attempts))
	for id := range r.attempts {
		out = append(out, id)
	}
	return out
}

// sendMsg sends if still on connection gen; never blocks while holding mu.
func (r *Runner) sendMsg(gen int, m *pb.WorkerMessage) bool {
	r.mu.Lock()
	send, cur := r.send, r.gen
	r.mu.Unlock()
	if send == nil || gen != cur {
		return false
	}
	return send(m)
}

// --- controller requests ---

// Start begins running an assigned attempt. Duplicate assignments (e.g.
// resent after a reconnect) are ignored.
func (r *Runner) Start(as *pb.AssignTask) {
	r.mu.Lock()
	if _, ok := r.attempts[as.AttemptId]; ok {
		r.mu.Unlock()
		return
	}
	a := &attempt{
		id: as.AttemptId, dir: filepath.Join(r.opts.Dir, as.AttemptId), assign: as,
		exited: make(chan struct{}), resetTo: &pb.LogOffsets{},
	}
	a.stop, a.stopFn = context.WithCancelCause(context.Background())
	r.attempts[a.id] = a
	r.mu.Unlock()

	r.log.Info("task assigned", "task", as.TaskId, "attempt", as.AttemptId, "command", as.Spec.Command)
	go r.execute(a)
	go r.ship(a)
}

// Cancel stops a running attempt. Unknown attempts are ignored.
func (r *Runner) Cancel(id, reason string) {
	r.mu.Lock()
	a := r.attempts[id]
	r.mu.Unlock()
	if a == nil {
		return
	}
	a.stopFn(errors.New(reason)) // no-op if already stopped
}

// Ack means the controller has the result: the attempt can be forgotten.
func (r *Runner) Ack(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a := r.attempts[id]; a != nil {
		a.acked = true
	}
}

// Shutdown stops all running tasks (they are reported as lost on the next
// connection, possibly after a restart) and waits for them to exit.
func (r *Runner) Shutdown() {
	r.mu.Lock()
	r.shuttingDown = true
	var running []*attempt
	for _, a := range r.attempts {
		running = append(running, a)
	}
	r.mu.Unlock()
	for _, a := range running {
		a.stopFn(errors.New("worker shutting down"))
	}
	deadline := time.After(killGrace + 5*time.Second)
	for _, a := range running {
		select {
		case <-a.exited:
		case <-deadline:
			return
		}
	}
}

// --- execution ---

func (r *Runner) execute(a *attempt) {
	res := r.runProcess(a)
	res.AttemptId = a.id
	res.FinishedUnixMs = time.Now().UnixMilli()
	if err := writeResult(a.dir, res); err != nil {
		r.log.Error("write result", "attempt", a.id, "err", err)
	}
	r.mu.Lock()
	a.result = res
	r.mu.Unlock()
	close(a.exited)
	r.log.Info("task finished", "task", a.assign.TaskId, "attempt", a.id,
		"outcome", res.Outcome, "exit_code", res.ExitCode, "error", res.Error)
}

// runProcess runs the attempt's command to completion and describes how it
// ended. Platform specifics live in proc_*.go.
func (r *Runner) runProcess(a *attempt) *pb.TaskResult {
	work := filepath.Join(a.dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		return failedToStart(err)
	}
	if b, err := protojson.Marshal(a.assign); err == nil {
		_ = os.WriteFile(filepath.Join(a.dir, "assign.json"), b, 0o600)
	}
	if r.user != nil {
		if err := chownTo(work, r.user); err != nil {
			return failedToStart(err)
		}
	}

	// Inputs first: downloading can take a while, and the task can be
	// cancelled meanwhile.
	if err := r.stageInputs(a, work); err != nil {
		if a.stop.Err() != nil {
			return r.stoppedResult(a, 0)
		}
		return failedToStart(fmt.Errorf("preparing inputs: %w", err))
	}

	stdout, err := os.Create(filepath.Join(a.dir, "stdout"))
	if err != nil {
		return failedToStart(err)
	}
	defer stdout.Close()
	stderr, err := os.Create(filepath.Join(a.dir, "stderr"))
	if err != nil {
		return failedToStart(err)
	}
	defer stderr.Close()

	p, err := r.startProcess(a, work, stdout, stderr)
	if err != nil {
		return failedToStart(err)
	}
	started := time.Now().UnixMilli()
	r.mu.Lock()
	a.started = started
	r.mu.Unlock()
	_ = os.WriteFile(filepath.Join(a.dir, "pid"), []byte(fmt.Sprint(p.pid())), 0o600)

	var timeout <-chan time.Time
	if s := a.assign.Spec.TimeoutSeconds; s > 0 {
		t := time.NewTimer(time.Duration(s) * time.Second)
		defer t.Stop()
		timeout = t.C
	}

	waitc := make(chan error, 1)
	go func() { waitc <- p.wait() }()

	stopReason := ""
	var outcome pb.TaskResult_Outcome
	var waitErr error
	select {
	case waitErr = <-waitc:
	case <-timeout:
		stopReason, outcome = fmt.Sprintf("timed out after %ds", a.assign.Spec.TimeoutSeconds), pb.TaskResult_TIMED_OUT
	case <-a.stop.Done():
		res := r.stoppedResult(a, started)
		stopReason, outcome = res.Error, res.Outcome
	}
	if stopReason != "" {
		r.log.Info("stopping task", "attempt", a.id, "reason", stopReason)
		p.terminate()
		select {
		case waitErr = <-waitc:
		case <-time.After(killGrace):
			p.kill()
			waitErr = <-waitc
		}
	}
	p.cleanup()
	_ = os.Remove(filepath.Join(a.dir, "pid"))

	res := &pb.TaskResult{StartedUnixMs: started}
	if stopReason != "" {
		res.Outcome, res.Error = outcome, stopReason
	} else {
		res.Outcome = pb.TaskResult_EXITED
		res.ExitCode, res.Error = exitStatus(waitErr, a.assign.Spec.MemoryBytes > 0)
	}
	// Outputs of finished and timed-out tasks are kept (a timed-out task's
	// partial results are often useful); cancelled or lost ones are not.
	if res.Outcome == pb.TaskResult_EXITED || res.Outcome == pb.TaskResult_TIMED_OUT {
		if err := r.collectOutputs(a, work, res); err != nil {
			res.Error = strings.TrimPrefix(res.Error+"; collecting outputs: "+err.Error(), "; ")
		}
	}
	return res
}

// stoppedResult describes an attempt stopped through a.stop: cancelled by
// the user, or lost because the worker is shutting down (not the task's
// fault, so it is requeued without using up a retry).
func (r *Runner) stoppedResult(a *attempt, started int64) *pb.TaskResult {
	res := &pb.TaskResult{Outcome: pb.TaskResult_CANCELED, StartedUnixMs: started, ExitCode: -1}
	if cause := context.Cause(a.stop); cause != nil {
		res.Error = cause.Error()
	}
	r.mu.Lock()
	if r.shuttingDown {
		res.Outcome = pb.TaskResult_LOST
	}
	r.mu.Unlock()
	return res
}

func failedToStart(err error) *pb.TaskResult {
	return &pb.TaskResult{Outcome: pb.TaskResult_FAILED_TO_START, Error: err.Error(), ExitCode: -1}
}

// --- shipping logs and results ---

// ship streams new output to the controller and finally the result, then
// waits for the acknowledgement and deletes the attempt directory.
func (r *Runner) ship(a *attempt) {
	var offsets [2]int64 // stdout, stderr: bytes the controller has (as far as we know)
	gen := -1
	startedSent, resultSent, uploaded := false, false, false
	logFiles := [2]string{filepath.Join(a.dir, "stdout"), filepath.Join(a.dir, "stderr")}
	streams := [2]pb.Stream{pb.Stream_STDOUT, pb.Stream_STDERR}

	caughtUp := true
	for {
		// Poll gently when idle; go straight on while catching up on output.
		if caughtUp {
			time.Sleep(shipInterval)
		}
		r.mu.Lock()
		connected, curGen := r.send != nil, r.gen
		acked, result, started := a.acked, a.result, a.started
		if curGen != gen && a.resetTo != nil {
			// New connection: resume from what the controller has.
			offsets = [2]int64{a.resetTo.Stdout, a.resetTo.Stderr}
			a.resetTo = nil
			gen = curGen
			// Re-check outputs on a new connection too: if we were away for
			// long, the controller may have cleaned up an unclaimed upload.
			startedSent, resultSent, uploaded = false, false, false
		}
		r.mu.Unlock()

		if acked {
			r.forget(a)
			return
		}
		if !connected || gen != curGen {
			caughtUp = true
			continue
		}
		if started > 0 && !startedSent {
			startedSent = r.sendMsg(gen, &pb.WorkerMessage{Msg: &pb.WorkerMessage_TaskStarted{
				TaskStarted: &pb.TaskStarted{AttemptId: a.id, StartedUnixMs: started}}})
		}

		// Ship whatever was written since last time.
		caughtUp = true
		for i := range logFiles {
			n, done := r.shipFile(gen, a.id, logFiles[i], streams[i], offsets[i])
			offsets[i] += n
			caughtUp = caughtUp && done
		}

		// The result goes last, after all output, on the same ordered
		// stream, so the controller has the full log when it sees it.
		// Output files are uploaded before the result, so the controller
		// has them when it accepts the result.
		if result != nil && caughtUp && !resultSent && !uploaded {
			if err := r.uploadOutputs(a, result, gen); err != nil {
				r.log.Warn("uploading outputs failed; will retry", "attempt", a.id, "err", err)
				time.Sleep(2 * time.Second)
				continue
			}
			uploaded = true
		}
		if result != nil && caughtUp && !resultSent {
			resultSent = r.sendMsg(gen, &pb.WorkerMessage{Msg: &pb.WorkerMessage_Result{Result: result}})
		}
	}
}

// shipFile sends file bytes from offset onward. It returns how many bytes
// were sent and whether it reached the end of the file.
func (r *Runner) shipFile(gen int, attemptID, path string, stream pb.Stream, offset int64) (int64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, true // not created yet (or failed to start): nothing to ship
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, true
	}
	var sent int64
	buf := make([]byte, maxChunk)
	for i := 0; i < 16; i++ { // at most 1 MiB per tick per stream
		n, err := f.Read(buf)
		if n > 0 {
			chunk := &pb.LogChunk{AttemptId: attemptID, Stream: stream, Offset: offset + sent, Data: append([]byte(nil), buf[:n]...)}
			if !r.sendMsg(gen, &pb.WorkerMessage{Msg: &pb.WorkerMessage_Log{Log: chunk}}) {
				return sent, false
			}
			sent += int64(n)
		}
		if err != nil { // io.EOF: caught up
			return sent, true
		}
	}
	return sent, false
}

func (r *Runner) forget(a *attempt) {
	r.mu.Lock()
	delete(r.attempts, a.id)
	r.mu.Unlock()
	if err := os.RemoveAll(a.dir); err != nil {
		r.log.Warn("remove attempt dir", "dir", a.dir, "err", err)
	}
}

// --- persistence and recovery ---

func writeResult(dir string, res *pb.TaskResult) error {
	b, err := protojson.Marshal(res)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "result.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "result.json"))
}

func readResult(dir string) (*pb.TaskResult, error) {
	b, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		return nil, err
	}
	res := &pb.TaskResult{}
	return res, protojson.Unmarshal(b, res)
}

// recover re-adopts attempt directories from a previous worker process.
func (r *Runner) recover() error {
	entries, err := os.ReadDir(r.opts.Dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		dir := filepath.Join(r.opts.Dir, id)
		res, err := readResult(dir)
		if err != nil {
			// No result: the worker died while the task ran. Make sure the
			// process is gone, then report the attempt as lost (it will be
			// requeued without using up a retry).
			if pidData, perr := os.ReadFile(filepath.Join(dir, "pid")); perr == nil {
				killOrphan(id, string(pidData))
			}
			res = &pb.TaskResult{AttemptId: id, Outcome: pb.TaskResult_LOST, ExitCode: -1,
				Error: "worker restarted while the task was running", FinishedUnixMs: time.Now().UnixMilli()}
			if err := writeResult(dir, res); err != nil {
				return err
			}
		}
		assign := &pb.AssignTask{AttemptId: id, Spec: &pb.TaskSpec{}}
		if b, err := os.ReadFile(filepath.Join(dir, "assign.json")); err == nil {
			_ = protojson.Unmarshal(b, assign)
		}
		a := &attempt{id: id, dir: dir, assign: assign,
			exited: make(chan struct{}), result: res, started: res.StartedUnixMs, resetTo: &pb.LogOffsets{}}
		a.stop, a.stopFn = context.WithCancelCause(context.Background())
		close(a.exited)
		r.attempts[id] = a
		go r.ship(a)
		r.log.Info("recovered task attempt from previous run", "attempt", id, "outcome", res.Outcome)
	}
	return nil
}

// errUnsupported is returned on platforms that can't run tasks.
var errUnsupported = errors.New("running tasks is only supported on Linux workers")

// signalError describes a task killed by a signal. The runner only sends
// signals itself on cancel/timeout, which are reported separately, so a
// KILL or TERM here with a memory limit is almost always the OOM killer
// (systemd stops the whole scope with TERM after an OOM kill).
func signalError(sig string, memLimited bool) string {
	msg := "killed by signal " + sig
	if memLimited && (sig == "killed" || sig == "terminated") {
		msg += " (likely out of memory: the task exceeded its --memory limit)"
	}
	return msg
}
