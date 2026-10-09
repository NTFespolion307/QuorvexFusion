package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	pb "github.com/NTFespolion307/QuorvexFusion/internal/clusterpb"
	"github.com/NTFespolion307/QuorvexFusion/internal/scheduler"
	"github.com/NTFespolion307/QuorvexFusion/internal/secret"
	"github.com/NTFespolion307/QuorvexFusion/internal/store"
)

// This file is the task lifecycle on the controller:
//
//	submit ─► queued ─► (scheduler) ─► assigned ─► running ─► succeeded
//	             ▲                         │           │    └► failed (retries used up)
//	             └──────── retry / lost ◄──┴───────────┘
//
// Each placement is an attempt with its own ID. Resources are reserved per
// attempt, from placement until the worker reports the attempt finished
// (or its node is declared lost). Only results from a task's current
// attempt are accepted, so a task completes exactly once even if a worker
// that was presumed dead comes back with an old result.

const (
	// maxLostAttempts caps requeues caused by nodes disappearing, so a task
	// that crashes its machine every time eventually fails.
	maxLostAttempts = 5
	// queueWindow is how many queued tasks one scheduling pass considers.
	queueWindow = 2000
)

// reservation is the resources held by one active attempt.
type reservation struct {
	attemptID string
	taskID    string
	jobID     string
	nodeID    string
	number    int
	cpus      float64
	memory    uint64
	gpus      []scheduler.GPU
}

type taskManager struct {
	mu           sync.Mutex                  // guards everything below and all task state transitions
	reservations map[string]*reservation     // by attempt ID
	specs        map[string]*JobSpec         // job spec cache, by job ID
	offline      map[string]*time.Timer      // disconnected nodes in their grace period
	pending      map[string]string           // task ID -> why it isn't placed yet
	progress     map[string]*pb.TaskProgress // attempt ID -> latest transfer progress
	cached       map[string]map[string]bool  // node ID -> files it has downloaded (for locality)

	kick chan struct{}
	logs *logStore
}

func newTaskManager(logDir string) *taskManager {
	return &taskManager{
		reservations: map[string]*reservation{},
		specs:        map[string]*JobSpec{},
		offline:      map[string]*time.Timer{},
		pending:      map[string]string{},
		progress:     map[string]*pb.TaskProgress{},
		cached:       map[string]map[string]bool{},
		kick:         make(chan struct{}, 1),
		logs:         newLogStore(logDir),
	}
}

// restoreReservations rebuilds in-memory reservations from the database
// after a controller restart. Nodes holding attempts get a grace period to
// reconnect and reclaim them before they are requeued.
func (c *Controller) restoreReservations() error {
	attempts, err := c.store.ActiveAttempts()
	if err != nil {
		return err
	}
	c.tm.mu.Lock()
	defer c.tm.mu.Unlock()
	nodes := map[string]bool{}
	for _, a := range attempts {
		t, err := c.store.GetTask(a.TaskID)
		if err != nil {
			continue
		}
		var gpus []scheduler.GPU
		_ = json.Unmarshal([]byte(a.GPUs), &gpus)
		c.tm.reservations[a.ID] = &reservation{
			attemptID: a.ID, taskID: a.TaskID, jobID: t.JobID, nodeID: a.NodeID, number: a.Number,
			cpus: a.CPUs, memory: a.MemoryBytes, gpus: gpus,
		}
		nodes[a.NodeID] = true
	}
	grace := max(30*time.Second, 2*c.cfg.HeartbeatTimeout())
	for id := range nodes {
		c.startGraceLocked(id, grace)
	}
	if len(attempts) > 0 {
		c.log.Info("restored active attempts", "attempts", len(attempts), "nodes", len(nodes), "grace", grace)
	}
	return nil
}

func (c *Controller) kickScheduler() {
	select {
	case c.tm.kick <- struct{}{}:
	default:
	}
}

// schedulerLoop runs a scheduling pass whenever something changes (a kick)
// and every few seconds as a safety net.
func (c *Controller) schedulerLoop(ctx context.Context) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.tm.kick:
		case <-t.C:
		}
		if err := c.schedulePass(); err != nil {
			c.log.Error("scheduling pass failed", "err", err)
		}
	}
}

// jobSpecLocked returns the decoded spec of a job (cached).
func (c *Controller) jobSpecLocked(jobID string) (*JobSpec, error) {
	if s, ok := c.tm.specs[jobID]; ok {
		return s, nil
	}
	j, err := c.store.GetJob(jobID)
	if err != nil {
		return nil, err
	}
	var s JobSpec
	if err := json.Unmarshal([]byte(j.Spec), &s); err != nil {
		return nil, fmt.Errorf("job %s: bad spec: %w", jobID, err)
	}
	c.tm.specs[jobID] = &s
	return &s, nil
}

// nodeSnapshotLocked describes every online, approved node with its
// current reservations, for the scheduler.
func (c *Controller) nodeSnapshotLocked() (map[string]*scheduler.Node, error) {
	nodes, err := c.store.ListNodes()
	if err != nil {
		return nil, err
	}
	out := map[string]*scheduler.Node{}
	for _, n := range nodes {
		if n.Status != store.NodeApproved {
			continue
		}
		sess := c.hub.Get(n.ID)
		if sess == nil {
			continue
		}
		hw := sess.Snapshot().Hello.Hardware
		sn := &scheduler.Node{
			ID: n.ID, CPUs: hw.CpuLimit, MemoryBytes: hw.MemoryBytes,
			Labels: n.EffectiveLabels(), Docker: hw.Docker, NvidiaDocker: hw.NvidiaDocker,
			Ephemeral: n.Ephemeral, Remote: isRemote(n), Draining: n.Draining, SharedStorage: n.SharedStorage != "",
		}
		for _, g := range hw.Gpus {
			sn.GPUs = append(sn.GPUs, scheduler.GPU{Index: int(g.Index), UUID: g.Uuid, Vendor: g.Vendor})
		}
		out[n.ID] = sn
	}
	for _, r := range c.tm.reservations {
		if sn := out[r.nodeID]; sn != nil {
			scheduler.Reserve(sn, r.cpus, r.memory, r.gpus)
		}
	}
	return out, nil
}

func taskRequest(t *store.Task, spec *JobSpec, preferNodes map[string]bool) *scheduler.Task {
	return &scheduler.Task{
		ID: t.ID, CPUs: spec.CPUs, MemoryBytes: spec.MemoryBytes, GPUs: spec.GPUs,
		Requires: spec.Requires, Prefers: spec.Prefers,
		AllowEphemeral: spec.allowEphemeral(), AllowRemote: spec.allowRemote(),
		NeedsShared: spec.needsShared(), PreferNodes: preferNodes,
		// Containers need Docker; GPUs in containers need the NVIDIA toolkit.
		NeedsDocker: spec.Image != "", NeedsNvidia: spec.Image != "" && spec.GPUs > 0,
	}
}

// schedulePass places as many queued tasks as currently fit.
func (c *Controller) schedulePass() error {
	c.tm.mu.Lock()
	defer c.tm.mu.Unlock()

	queued, err := c.store.QueuedTasks(queueWindow)
	if err != nil {
		return err
	}
	c.tm.pending = map[string]string{}
	if len(queued) == 0 {
		return nil
	}
	snapshot, err := c.nodeSnapshotLocked()
	if err != nil {
		return err
	}
	nodes := make([]*scheduler.Node, 0, len(snapshot))
	for _, n := range snapshot {
		nodes = append(nodes, n)
	}

	byID := map[string]*store.Task{}
	preferred := map[string]map[string]bool{} // per job, computed once per pass
	reqs := make([]*scheduler.Task, 0, len(queued))
	for _, t := range queued {
		spec, err := c.jobSpecLocked(t.JobID)
		if err != nil {
			c.log.Error("skipping task", "task", t.ID, "err", err)
			continue
		}
		byID[t.ID] = t
		prefer, ok := preferred[t.JobID]
		if !ok {
			prefer = c.preferredNodesLocked(spec)
			preferred[t.JobID] = prefer
		}
		reqs = append(reqs, taskRequest(t, spec, prefer))
	}

	placed := map[string]bool{}
	for _, p := range scheduler.Schedule(nodes, reqs) {
		placed[p.TaskID] = true
		c.startAttemptLocked(byID[p.TaskID], p)
	}
	// Remember why the first unplaced tasks are waiting, for the UI/CLI.
	for _, r := range reqs {
		if placed[r.ID] || len(c.tm.pending) >= 200 {
			continue
		}
		reason := scheduler.Explain(nodes, r)
		if reason == "" {
			reason = "waiting for free resources"
		}
		c.tm.pending[r.ID] = reason
	}
	if len(placed) > 0 {
		c.events.publish("jobs")
	}
	return nil
}

// startAttemptLocked records a placement and sends the task to its node.
func (c *Controller) startAttemptLocked(t *store.Task, p scheduler.Placement) {
	spec := c.tm.specs[t.JobID]
	gpuJSON, _ := json.Marshal(p.GPUs)
	a := &store.Attempt{
		ID: "a" + secret.RandomHex(6), TaskID: t.ID, NodeID: p.NodeID,
		CPUs: spec.CPUs, MemoryBytes: spec.MemoryBytes, GPUs: string(gpuJSON), CreatedAt: time.Now(),
	}
	if err := c.store.StartAttempt(a); err != nil {
		c.log.Error("start attempt", "task", t.ID, "err", err)
		return
	}
	r := &reservation{
		attemptID: a.ID, taskID: t.ID, jobID: t.JobID, nodeID: p.NodeID, number: a.Number,
		cpus: spec.CPUs, memory: spec.MemoryBytes, gpus: p.GPUs,
	}
	c.tm.reservations[a.ID] = r

	env := map[string]string{}
	for k, v := range spec.Env {
		env[k] = substitute(v, t.Index)
	}
	ts := &pb.TaskSpec{
		Command: substitute(spec.Command, t.Index), Env: env,
		Cpus: spec.CPUs, MemoryBytes: spec.MemoryBytes, TimeoutSeconds: spec.TimeoutSec,
		Outputs: spec.Outputs, Image: spec.Image,
	}
	for _, in := range spec.Inputs {
		ts.Inputs = append(ts.Inputs, &pb.InputFile{
			Path: in.Path, Sha256: in.SHA256, Size: in.Size, Mode: in.Mode, SharedPath: in.Shared,
		})
	}
	for _, g := range p.GPUs {
		ts.Gpus = append(ts.Gpus, &pb.GPUAssignment{Index: int32(g.Index), Uuid: g.UUID, Vendor: g.Vendor})
	}
	msg := &pb.ControllerMessage{Msg: &pb.ControllerMessage_Assign{Assign: &pb.AssignTask{
		AttemptId: a.ID, TaskId: t.ID, JobId: t.JobID, Attempt: int32(a.Number), ArrayIndex: t.Index, Spec: ts,
	}}}
	// If the send fails the node is going away; the attempt is then
	// handled by the node's grace period like any other lost attempt.
	if sess := c.hub.Get(p.NodeID); sess != nil {
		sess.TrySend(msg)
	}
	c.log.Debug("task placed", "task", t.ID, "attempt", a.ID, "node", p.NodeID)
}

// --- messages from workers ---

func (c *Controller) handleTaskStarted(nodeID string, m *pb.TaskStarted) {
	c.tm.mu.Lock()
	defer c.tm.mu.Unlock()
	r := c.tm.reservations[m.AttemptId]
	if r == nil || r.nodeID != nodeID {
		return
	}
	if err := c.store.MarkAttemptRunning(m.AttemptId, time.UnixMilli(m.StartedUnixMs)); err != nil && !errors.Is(err, store.ErrConflict) {
		c.log.Error("mark running", "attempt", m.AttemptId, "err", err)
	}
	c.events.publish("jobs")
}

func (c *Controller) handleLog(nodeID string, chunk *pb.LogChunk) {
	c.tm.mu.Lock()
	r := c.tm.reservations[chunk.AttemptId]
	c.tm.mu.Unlock()
	// Only the node running an attempt may write its logs.
	if r == nil || r.nodeID != nodeID {
		return
	}
	if err := c.tm.logs.Append(r.taskID, r.number, chunk); err != nil {
		c.log.Error("store log chunk", "task", r.taskID, "err", err)
	}
}

// handleResult finalises an attempt. The result is always acknowledged so
// the worker can drop it, even when it is stale and ignored.
func (c *Controller) handleResult(sess *Session, res *pb.TaskResult) {
	defer sess.TrySend(&pb.ControllerMessage{Msg: &pb.ControllerMessage_ResultAck{
		ResultAck: &pb.ResultAck{AttemptId: res.AttemptId}}})

	c.tm.mu.Lock()
	defer c.tm.mu.Unlock()
	r := c.tm.reservations[res.AttemptId]
	if r == nil || r.nodeID != sess.NodeID {
		return // stale: the attempt was already lost, canceled or finished
	}
	c.finishAttemptLocked(r, res)
}

// finishAttemptLocked applies an attempt's outcome to its task: success,
// retry, failure or requeue; then releases its resources.
func (c *Controller) finishAttemptLocked(r *reservation, res *pb.TaskResult) {
	task, err := c.store.GetTask(r.taskID)
	if err != nil {
		c.log.Error("finish attempt: load task", "task", r.taskID, "err", err)
		delete(c.tm.reservations, r.attemptID)
		return
	}
	spec, err := c.jobSpecLocked(r.jobID)
	if err != nil {
		spec = &JobSpec{}
	}
	finished := time.Now()
	if res.FinishedUnixMs > 0 {
		finished = time.UnixMilli(res.FinishedUnixMs)
	}
	o := store.AttemptOutcome{Finished: finished, Error: res.Error}

	switch {
	case res.Outcome == pb.TaskResult_EXITED && res.ExitCode == 0:
		o.State, o.TaskState = store.AttemptSucceeded, store.TaskSucceeded
		code := 0
		o.ExitCode = &code

	case res.Outcome == pb.TaskResult_CANCELED:
		o.State, o.TaskState = store.AttemptCanceled, store.TaskCanceled
		if o.Error == "" {
			o.Error = "canceled"
		}

	case res.Outcome == pb.TaskResult_LOST:
		o.State, o.CountLost = store.AttemptLost, true
		o.TaskState = store.TaskQueued
		if task.Lost+1 >= maxLostAttempts {
			o.TaskState = store.TaskFailed
			o.Error = fmt.Sprintf("lost %d times: %s", task.Lost+1, res.Error)
		}

	default: // non-zero exit, failed to start, timed out
		o.State, o.CountFailure = store.AttemptFailed, true
		if res.Outcome == pb.TaskResult_EXITED {
			code := int(res.ExitCode)
			o.ExitCode = &code
			if o.Error == "" {
				o.Error = fmt.Sprintf("exit code %d", code)
			}
		}
		if task.Failures+1 <= spec.Retries {
			o.TaskState = store.TaskQueued // retry
		} else {
			o.TaskState = store.TaskFailed
		}
	}

	if len(res.Outputs) > 0 {
		var missing int
		o.Outputs, missing = c.acceptOutputs(res)
		if missing > 0 {
			c.log.Warn("worker reported outputs that never arrived", "task", r.taskID, "missing", missing)
		}
	}

	if err := c.store.FinishAttempt(r.attemptID, o); err != nil && !errors.Is(err, store.ErrConflict) {
		c.log.Error("finish attempt", "attempt", r.attemptID, "err", err)
		return
	}
	delete(c.tm.reservations, r.attemptID)
	delete(c.tm.progress, r.attemptID)
	c.log.Info("attempt finished", "task", r.taskID, "attempt", r.number, "node", r.nodeID,
		"result", o.State, "task_state", o.TaskState, "error", o.Error)
	c.events.publish("jobs")
	c.kickScheduler()
}

// loseAttemptLocked requeues an attempt whose node went away.
func (c *Controller) loseAttemptLocked(r *reservation, reason string) {
	c.finishAttemptLocked(r, &pb.TaskResult{
		AttemptId: r.attemptID, Outcome: pb.TaskResult_LOST, Error: reason,
		FinishedUnixMs: time.Now().UnixMilli(),
	})
}

// --- node connectivity ---

// nodeConnected reconciles a (re)connecting worker's attempts with ours.
// It returns the log offsets to resume from for adopted attempts and the
// stale attempts the worker must kill.
func (c *Controller) nodeConnected(nodeID string, held []string) (offsets map[string]*pb.LogOffsets, stale []string) {
	c.tm.mu.Lock()
	defer c.tm.mu.Unlock()
	if t := c.tm.offline[nodeID]; t != nil {
		t.Stop()
		delete(c.tm.offline, nodeID)
	}
	workerHas := map[string]bool{}
	for _, id := range held {
		workerHas[id] = true
	}
	offsets = map[string]*pb.LogOffsets{}
	for _, r := range c.tm.reservations {
		if r.nodeID != nodeID {
			continue
		}
		if workerHas[r.attemptID] {
			offsets[r.attemptID] = c.tm.logs.Sizes(r.taskID, r.number)
		} else {
			// The worker never got it, or restarted and lost it.
			c.loseAttemptLocked(r, "worker no longer has this task")
		}
	}
	for _, id := range held {
		if r := c.tm.reservations[id]; r == nil || r.nodeID != nodeID {
			stale = append(stale, id)
		}
	}
	c.kickScheduler()
	return offsets, stale
}

// nodeDisconnected starts the grace period: running attempts are kept for
// one heartbeat timeout in case the worker reconnects (e.g. a network blip).
func (c *Controller) nodeDisconnected(nodeID string) {
	c.tm.mu.Lock()
	defer c.tm.mu.Unlock()
	c.startGraceLocked(nodeID, c.cfg.HeartbeatTimeout())
}

func (c *Controller) startGraceLocked(nodeID string, grace time.Duration) {
	has := false
	for _, r := range c.tm.reservations {
		if r.nodeID == nodeID {
			has = true
			break
		}
	}
	if !has {
		return
	}
	if t := c.tm.offline[nodeID]; t != nil {
		t.Stop()
	}
	c.tm.offline[nodeID] = time.AfterFunc(grace, func() { c.nodeLost(nodeID, "node offline") })
}

// nodeLost requeues everything on a node that did not come back in time.
func (c *Controller) nodeLost(nodeID, reason string) {
	c.tm.mu.Lock()
	defer c.tm.mu.Unlock()
	delete(c.tm.offline, nodeID)
	if c.hub.Get(nodeID) != nil {
		return // reconnected meanwhile
	}
	c.loseNodeAttemptsLocked(nodeID, reason)
}

func (c *Controller) loseNodeAttemptsLocked(nodeID, reason string) {
	n := 0
	for _, r := range c.tm.reservations {
		if r.nodeID == nodeID {
			c.loseAttemptLocked(r, reason)
			n++
		}
	}
	if n > 0 {
		c.log.Warn("requeued tasks from lost node", "node", nodeID, "tasks", n, "reason", reason)
	}
}

// --- user actions ---

// SubmitJob validates a spec, creates the job and its tasks, and wakes the
// scheduler.
func (c *Controller) SubmitJob(spec *JobSpec) (*store.Job, error) {
	if err := spec.Normalize(); err != nil {
		return nil, err
	}
	if err := c.checkInputsUploaded(spec); err != nil {
		return nil, err
	}
	indices, err := ParseArray(spec.Array)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(spec)
	j := &store.Job{
		ID: "j" + secret.RandomHex(4), Name: spec.Name, Spec: string(raw),
		Priority: spec.Priority, CreatedAt: time.Now(),
	}
	if err := c.store.CreateJob(j, indices); err != nil {
		return nil, err
	}
	c.log.Info("job submitted", "job", j.ID, "tasks", len(indices), "name", j.Name)
	c.events.publish("jobs")
	c.kickScheduler()
	return c.store.GetJob(j.ID)
}

// CancelJob cancels all unfinished tasks of a job.
func (c *Controller) CancelJob(jobID string) error {
	if err := c.store.SetJobCanceled(jobID); err != nil {
		return err
	}
	if _, err := c.store.CancelQueuedTasks(jobID, "", time.Now()); err != nil {
		return err
	}
	active, err := c.store.ActiveTasks(jobID)
	if err != nil {
		return err
	}
	c.tm.mu.Lock()
	for _, t := range active {
		c.cancelActiveLocked(t)
	}
	c.tm.mu.Unlock()
	c.events.publish("jobs")
	return nil
}

// CancelTask cancels one task.
func (c *Controller) CancelTask(taskID string) error {
	t, err := c.store.GetTask(taskID)
	if err != nil {
		return err
	}
	switch {
	case t.State == store.TaskQueued:
		_, err = c.store.CancelQueuedTasks(t.JobID, t.ID, time.Now())
	case t.State.Active():
		c.tm.mu.Lock()
		c.cancelActiveLocked(t)
		c.tm.mu.Unlock()
	default:
		return fmt.Errorf("task is already %s", t.State)
	}
	c.events.publish("jobs")
	return err
}

// cancelActiveLocked marks a running task canceled and tells its worker to
// stop it. Resources are released when the worker confirms.
func (c *Controller) cancelActiveLocked(t *store.Task) {
	if err := c.store.SetTaskCanceled(t.ID, time.Now()); err != nil && !errors.Is(err, store.ErrNotFound) {
		c.log.Error("cancel task", "task", t.ID, "err", err)
	}
	r := c.tm.reservations[t.AttemptID]
	if r == nil {
		return
	}
	if sess := c.hub.Get(r.nodeID); sess != nil {
		sess.TrySend(&pb.ControllerMessage{Msg: &pb.ControllerMessage_Cancel{Cancel: &pb.CancelTask{
			AttemptId: r.attemptID, Reason: "canceled by user"}}})
	}
}

// DeleteJob removes a finished job with its tasks and logs.
func (c *Controller) DeleteJob(jobID string) error {
	active, err := c.store.ActiveTasks(jobID)
	if err != nil {
		return err
	}
	if len(active) > 0 {
		return errors.New("job still has running tasks; cancel it and wait for them to stop")
	}
	tasks, err := c.store.ListTasks(jobID, "", maxArrayTasks, 0)
	if err != nil {
		return err
	}
	if err := c.store.DeleteJob(jobID); err != nil {
		return err
	}
	for _, t := range tasks {
		_ = c.tm.logs.RemoveTask(t.ID)
	}
	c.tm.mu.Lock()
	delete(c.tm.specs, jobID)
	c.tm.mu.Unlock()
	c.events.publish("jobs")
	go c.collectGarbage() // free the job's inputs and outputs if nothing else uses them
	return nil
}

// --- views ---

// JobView is a job with its decoded spec.
type JobView struct {
	*store.Job
	State string   `json:"state"`
	Spec  *JobSpec `json:"spec"`
}

// jobState summarises task counts as one word.
func jobState(j *store.Job) string {
	cnt := j.Counts
	switch {
	case cnt.Running+cnt.Assigned > 0:
		return "running"
	case cnt.Queued > 0:
		return "queued"
	case cnt.Failed > 0:
		return "failed"
	case cnt.Canceled > 0 && cnt.Succeeded == 0:
		return "canceled"
	case j.Canceled:
		return "canceled"
	default:
		return "succeeded"
	}
}

func (c *Controller) jobView(j *store.Job) *JobView {
	var spec JobSpec
	_ = json.Unmarshal([]byte(j.Spec), &spec)
	return &JobView{Job: j, State: jobState(j), Spec: &spec}
}

func (c *Controller) GetJobView(id string) (*JobView, error) {
	j, err := c.store.GetJob(id)
	if err != nil {
		return nil, err
	}
	return c.jobView(j), nil
}

func (c *Controller) ListJobViews(limit int) ([]*JobView, error) {
	jobs, err := c.store.ListJobs(limit)
	if err != nil {
		return nil, err
	}
	out := make([]*JobView, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, c.jobView(j))
	}
	return out, nil
}

// TaskView is a task with its attempts and, if queued, why it waits.
type TaskView struct {
	*store.Task
	PendingReason string              `json:"pending_reason,omitempty"`
	AttemptList   []*store.Attempt    `json:"attempt_list,omitempty"`
	Progress      *TransferProgress   `json:"progress,omitempty"` // inputs downloading / outputs uploading
	Outputs       []*store.OutputFile `json:"outputs,omitempty"`
}

// TransferProgress is a running task's current file transfer.
type TransferProgress struct {
	Phase      string `json:"phase"` // "download" (inputs) or "upload" (outputs)
	DoneBytes  int64  `json:"done_bytes"`
	TotalBytes int64  `json:"total_bytes"`
}

func (c *Controller) progressFor(t *store.Task) *TransferProgress {
	if !t.State.Active() {
		return nil
	}
	c.tm.mu.Lock()
	defer c.tm.mu.Unlock()
	p := c.tm.progress[t.AttemptID]
	if p == nil || p.DoneBytes >= p.TotalBytes {
		return nil
	}
	return &TransferProgress{Phase: p.Phase, DoneBytes: p.DoneBytes, TotalBytes: p.TotalBytes}
}

func (c *Controller) PendingReason(taskID string) string {
	c.tm.mu.Lock()
	defer c.tm.mu.Unlock()
	return c.tm.pending[taskID]
}

func (c *Controller) GetTaskView(id string) (*TaskView, error) {
	t, err := c.store.GetTask(id)
	if err != nil {
		return nil, err
	}
	attempts, err := c.store.ListAttempts(id)
	if err != nil {
		return nil, err
	}
	v := &TaskView{Task: t, AttemptList: attempts, Progress: c.progressFor(t)}
	if t.State == store.TaskQueued {
		v.PendingReason = c.PendingReason(id)
	}
	if v.Outputs, err = c.store.TaskOutputs(id); err != nil {
		return nil, err
	}
	return v, nil
}

func (c *Controller) ListTaskViews(jobID string, state store.TaskState, limit, offset int) ([]*TaskView, error) {
	tasks, err := c.store.ListTasks(jobID, state, limit, offset)
	if err != nil {
		return nil, err
	}
	c.tm.mu.Lock()
	defer c.tm.mu.Unlock()
	out := make([]*TaskView, 0, len(tasks))
	for _, t := range tasks {
		v := &TaskView{Task: t}
		if t.State == store.TaskQueued {
			v.PendingReason = c.tm.pending[t.ID]
		}
		// (c.tm.mu is held here, so read progress directly, not via progressFor.)
		if p := c.tm.progress[t.AttemptID]; t.State.Active() && p != nil && p.DoneBytes < p.TotalBytes {
			v.Progress = &TransferProgress{Phase: p.Phase, DoneBytes: p.DoneBytes, TotalBytes: p.TotalBytes}
		}
		out = append(out, v)
	}
	return out, nil
}

// ReadLog reads a task's log for an attempt (0 = the latest).
func (c *Controller) ReadLog(taskID string, attempt int, stream LogStream, offset, max int64) (data []byte, size int64, usedAttempt int, err error) {
	t, err := c.store.GetTask(taskID)
	if err != nil {
		return nil, 0, 0, err
	}
	if attempt <= 0 {
		attempt = t.Attempts
	}
	if attempt <= 0 {
		return nil, 0, 0, nil // never started
	}
	data, size, err = c.tm.logs.Read(taskID, attempt, stream, offset, max)
	return data, size, attempt, err
}

// usedResources sums reservations per node.
func (c *Controller) usedResources() map[string]*Resources {
	c.tm.mu.Lock()
	defer c.tm.mu.Unlock()
	out := map[string]*Resources{}
	for _, r := range c.tm.reservations {
		u := out[r.nodeID]
		if u == nil {
			u = &Resources{}
			out[r.nodeID] = u
		}
		u.CPUs += r.cpus
		u.MemoryBytes += r.memory
		u.GPUs += len(r.gpus)
	}
	return out
}
