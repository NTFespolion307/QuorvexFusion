package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Job, task and attempt times are stored as unix milliseconds (unlike the
// second-resolution times elsewhere) because short tasks are common.

func ms(t time.Time) int64 { return t.UnixMilli() }

func nullMs(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

func fromNullMs(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMilli(v.Int64)
	return &t
}

type TaskState string

const (
	TaskQueued    TaskState = "queued"
	TaskAssigned  TaskState = "assigned" // sent to a worker, not yet started
	TaskRunning   TaskState = "running"
	TaskSucceeded TaskState = "succeeded"
	TaskFailed    TaskState = "failed"
	TaskCanceled  TaskState = "canceled"
)

// Active reports whether the task currently occupies a node.
func (s TaskState) Active() bool { return s == TaskAssigned || s == TaskRunning }

// Terminal reports whether the task is finished for good.
func (s TaskState) Terminal() bool {
	return s == TaskSucceeded || s == TaskFailed || s == TaskCanceled
}

type AttemptState string

const (
	AttemptAssigned  AttemptState = "assigned"
	AttemptRunning   AttemptState = "running"
	AttemptSucceeded AttemptState = "succeeded"
	AttemptFailed    AttemptState = "failed"
	AttemptCanceled  AttemptState = "canceled"
	AttemptLost      AttemptState = "lost" // node went away
)

var ErrConflict = errors.New("state changed concurrently")

type TaskCounts struct {
	Queued    int `json:"queued"`
	Assigned  int `json:"assigned"`
	Running   int `json:"running"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Canceled  int `json:"canceled"`
}

func (c TaskCounts) Total() int {
	return c.Queued + c.Assigned + c.Running + c.Succeeded + c.Failed + c.Canceled
}

type Job struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Spec      string     `json:"-"` // JSON; decoded by the controller
	Priority  int        `json:"priority"`
	TaskCount int        `json:"task_count"`
	Canceled  bool       `json:"canceled"`
	CreatedAt time.Time  `json:"created_at"`
	Counts    TaskCounts `json:"counts"`
}

type Task struct {
	ID         string     `json:"id"`
	JobID      string     `json:"job_id"`
	Index      int64      `json:"index"`
	Seq        int64      `json:"-"`
	State      TaskState  `json:"state"`
	Attempts   int        `json:"attempts"`
	Failures   int        `json:"failures"`
	Lost       int        `json:"lost"`
	AttemptID  string     `json:"attempt_id,omitempty"`
	NodeID     string     `json:"node_id,omitempty"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Error      string     `json:"error,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

type Attempt struct {
	ID          string       `json:"id"`
	TaskID      string       `json:"task_id"`
	Number      int          `json:"number"`
	NodeID      string       `json:"node_id"`
	State       AttemptState `json:"state"`
	CPUs        float64      `json:"cpus"`
	MemoryBytes uint64       `json:"memory_bytes"`
	GPUs        string       `json:"-"` // JSON list, decoded by the controller
	ExitCode    *int         `json:"exit_code,omitempty"`
	Error       string       `json:"error,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
	StartedAt   *time.Time   `json:"started_at,omitempty"`
	FinishedAt  *time.Time   `json:"finished_at,omitempty"`
}

// --- jobs ---

// CreateJob inserts a job and one queued task per array index.
func (s *Store) CreateJob(j *Job, indices []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO jobs (id, name, spec, priority, task_count, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		j.ID, j.Name, j.Spec, j.Priority, len(indices), ms(j.CreatedAt)); err != nil {
		return err
	}
	var seq int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM tasks`).Scan(&seq); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO tasks (id, job_id, idx, seq, state, created_at) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, i := range indices {
		seq++
		if _, err := stmt.Exec(TaskID(j.ID, i), j.ID, i, seq, TaskQueued, ms(j.CreatedAt)); err != nil {
			return err
		}
	}
	j.TaskCount = len(indices)
	return tx.Commit()
}

// TaskID builds a task ID from its job ID and array index.
func TaskID(jobID string, index int64) string { return fmt.Sprintf("%s.%d", jobID, index) }

const jobCols = `id, name, spec, priority, task_count, canceled, created_at`

func scanJob(row interface{ Scan(...any) error }) (*Job, error) {
	var j Job
	var created int64
	err := row.Scan(&j.ID, &j.Name, &j.Spec, &j.Priority, &j.TaskCount, &j.Canceled, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	j.CreatedAt = time.UnixMilli(created)
	return &j, nil
}

func (s *Store) GetJob(id string) (*Job, error) {
	j, err := scanJob(s.db.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	counts, err := s.taskCounts([]string{j.ID})
	if err != nil {
		return nil, err
	}
	j.Counts = counts[j.ID]
	return j, nil
}

// ListJobs returns the newest jobs first, with task counts.
func (s *Store) ListJobs(limit int) ([]*Job, error) {
	rows, err := s.db.Query(`SELECT `+jobCols+` FROM jobs ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	var jobs []*Job
	var ids []string
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		jobs = append(jobs, j)
		ids = append(ids, j.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	counts, err := s.taskCounts(ids)
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		j.Counts = counts[j.ID]
	}
	return jobs, nil
}

func (s *Store) taskCounts(jobIDs []string) (map[string]TaskCounts, error) {
	out := map[string]TaskCounts{}
	if len(jobIDs) == 0 {
		return out, nil
	}
	args := make([]any, len(jobIDs))
	for i, id := range jobIDs {
		args[i] = id
	}
	rows, err := s.db.Query(`SELECT job_id, state, COUNT(*) FROM tasks WHERE job_id IN (?`+
		strings.Repeat(",?", len(jobIDs)-1)+`) GROUP BY job_id, state`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var st TaskState
		var n int
		if err := rows.Scan(&id, &st, &n); err != nil {
			return nil, err
		}
		c := out[id]
		switch st {
		case TaskQueued:
			c.Queued = n
		case TaskAssigned:
			c.Assigned = n
		case TaskRunning:
			c.Running = n
		case TaskSucceeded:
			c.Succeeded = n
		case TaskFailed:
			c.Failed = n
		case TaskCanceled:
			c.Canceled = n
		}
		out[id] = c
	}
	return out, rows.Err()
}

// GlobalTaskCounts counts tasks in each state across all jobs.
func (s *Store) GlobalTaskCounts() (TaskCounts, error) {
	var c TaskCounts
	rows, err := s.db.Query(`SELECT state, COUNT(*) FROM tasks WHERE state IN (?, ?, ?) GROUP BY state`,
		TaskQueued, TaskAssigned, TaskRunning)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var st TaskState
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return c, err
		}
		switch st {
		case TaskQueued:
			c.Queued = n
		case TaskAssigned:
			c.Assigned = n
		case TaskRunning:
			c.Running = n
		}
	}
	return c, rows.Err()
}

func (s *Store) SetJobCanceled(id string) error {
	res, err := s.db.Exec(`UPDATE jobs SET canceled = 1 WHERE id = ?`, id)
	return expectRow(res, err)
}

func (s *Store) DeleteJob(id string) error {
	res, err := s.db.Exec(`DELETE FROM jobs WHERE id = ?`, id)
	return expectRow(res, err)
}

// --- tasks ---

const taskCols = `id, job_id, idx, seq, state, attempts, failures, lost, attempt_id, node_id,
	exit_code, error, created_at, started_at, finished_at`

func scanTask(row interface{ Scan(...any) error }) (*Task, error) {
	var t Task
	var exit, started, finished sql.NullInt64
	var created int64
	err := row.Scan(&t.ID, &t.JobID, &t.Index, &t.Seq, &t.State, &t.Attempts, &t.Failures, &t.Lost,
		&t.AttemptID, &t.NodeID, &exit, &t.Error, &created, &started, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.ExitCode = fromNullInt(exit)
	t.CreatedAt = time.UnixMilli(created)
	t.StartedAt = fromNullMs(started)
	t.FinishedAt = fromNullMs(finished)
	return &t, nil
}

func (s *Store) queryTasks(query string, args ...any) ([]*Task, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) GetTask(id string) (*Task, error) {
	return scanTask(s.db.QueryRow(`SELECT `+taskCols+` FROM tasks WHERE id = ?`, id))
}

// ListTasks lists a job's tasks by index, optionally filtered by state.
func (s *Store) ListTasks(jobID string, state TaskState, limit, offset int) ([]*Task, error) {
	q := `SELECT ` + taskCols + ` FROM tasks WHERE job_id = ?`
	args := []any{jobID}
	if state != "" {
		q += ` AND state = ?`
		args = append(args, state)
	}
	q += ` ORDER BY idx LIMIT ? OFFSET ?`
	return s.queryTasks(q, append(args, limit, offset)...)
}

// QueuedTasks returns up to limit queued tasks of non-canceled jobs, in
// scheduling order: higher job priority first, then submission order.
func (s *Store) QueuedTasks(limit int) ([]*Task, error) {
	return s.queryTasks(`SELECT t.`+strings.ReplaceAll(taskCols, ", ", ", t.")+`
		FROM tasks t JOIN jobs j ON j.id = t.job_id
		WHERE t.state = ? ORDER BY j.priority DESC, t.seq LIMIT ?`, TaskQueued, limit)
}

// ActiveTaskIDs returns the IDs of a job's tasks that occupy a node.
func (s *Store) ActiveTasks(jobID string) ([]*Task, error) {
	return s.queryTasks(`SELECT `+taskCols+` FROM tasks WHERE job_id = ? AND state IN (?, ?)`,
		jobID, TaskAssigned, TaskRunning)
}

// CancelQueuedTasks cancels a job's queued tasks (taskID "" = all of them)
// and returns how many were canceled.
func (s *Store) CancelQueuedTasks(jobID, taskID string, now time.Time) (int, error) {
	q := `UPDATE tasks SET state = ?, finished_at = ? WHERE job_id = ? AND state = ?`
	args := []any{TaskCanceled, ms(now), jobID, TaskQueued}
	if taskID != "" {
		q += ` AND id = ?`
		args = append(args, taskID)
	}
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// SetTaskCanceled marks an active task canceled. Its attempt stays active
// until the worker confirms it stopped (so resources stay reserved).
func (s *Store) SetTaskCanceled(taskID string, now time.Time) error {
	res, err := s.db.Exec(`UPDATE tasks SET state = ?, finished_at = ?, error = 'canceled' WHERE id = ? AND state IN (?, ?)`,
		TaskCanceled, ms(now), taskID, TaskAssigned, TaskRunning)
	return expectRow(res, err)
}

// --- attempts ---

const attemptCols = `id, task_id, number, node_id, state, cpus, memory_bytes, gpus, exit_code, error,
	created_at, started_at, finished_at`

func scanAttempt(row interface{ Scan(...any) error }) (*Attempt, error) {
	var a Attempt
	var exit, started, finished sql.NullInt64
	var created int64
	err := row.Scan(&a.ID, &a.TaskID, &a.Number, &a.NodeID, &a.State, &a.CPUs, &a.MemoryBytes, &a.GPUs,
		&exit, &a.Error, &created, &started, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.ExitCode = fromNullInt(exit)
	a.CreatedAt = time.UnixMilli(created)
	a.StartedAt = fromNullMs(started)
	a.FinishedAt = fromNullMs(finished)
	return &a, nil
}

func (s *Store) queryAttempts(query string, args ...any) ([]*Attempt, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Attempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GetAttempt(id string) (*Attempt, error) {
	return scanAttempt(s.db.QueryRow(`SELECT `+attemptCols+` FROM attempts WHERE id = ?`, id))
}

func (s *Store) ListAttempts(taskID string) ([]*Attempt, error) {
	return s.queryAttempts(`SELECT `+attemptCols+` FROM attempts WHERE task_id = ? ORDER BY number`, taskID)
}

// ActiveAttempts returns all attempts that occupy a node (used to rebuild
// resource reservations when the controller starts).
func (s *Store) ActiveAttempts() ([]*Attempt, error) {
	return s.queryAttempts(`SELECT `+attemptCols+` FROM attempts WHERE state IN (?, ?)`, AttemptAssigned, AttemptRunning)
}

// StartAttempt records a new placement and moves the queued task to
// "assigned". It fails with ErrConflict if the task is no longer queued.
func (s *Store) StartAttempt(a *Attempt) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE tasks SET state = ?, attempts = attempts + 1, attempt_id = ?, node_id = ?, error = ''
		WHERE id = ? AND state = ?`, TaskAssigned, a.ID, a.NodeID, a.TaskID, TaskQueued)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrConflict
	}
	if err := tx.QueryRow(`SELECT attempts FROM tasks WHERE id = ?`, a.TaskID).Scan(&a.Number); err != nil {
		return err
	}
	a.State = AttemptAssigned
	if _, err := tx.Exec(`INSERT INTO attempts (id, task_id, number, node_id, state, cpus, memory_bytes, gpus, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.TaskID, a.Number, a.NodeID, a.State, a.CPUs, a.MemoryBytes, a.GPUs, ms(a.CreatedAt)); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkAttemptRunning records that the worker started the process.
func (s *Store) MarkAttemptRunning(attemptID string, started time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var taskID string
	err = tx.QueryRow(`UPDATE attempts SET state = ?, started_at = ? WHERE id = ? AND state = ? RETURNING task_id`,
		AttemptRunning, ms(started), attemptID, AttemptAssigned).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE tasks SET state = ?, started_at = COALESCE(started_at, ?)
		WHERE id = ? AND attempt_id = ? AND state = ?`,
		TaskRunning, ms(started), taskID, attemptID, TaskAssigned); err != nil {
		return err
	}
	return tx.Commit()
}

// AttemptOutcome describes how an attempt ended and what happens to its task.
type AttemptOutcome struct {
	State    AttemptState
	ExitCode *int
	Error    string
	Finished time.Time

	// TaskState is the task's next state: TaskQueued to retry, or a
	// terminal state. It is only applied if the attempt is still the task's
	// current one and the task is active (a canceled task stays canceled).
	TaskState    TaskState
	CountFailure bool
	CountLost    bool
	// Outputs replace the task's stored outputs if the outcome is applied.
	Outputs []OutputFile
}

// FinishAttempt closes an active attempt and updates its task, atomically.
// It returns ErrConflict if the attempt was already finished.
func (s *Store) FinishAttempt(attemptID string, o AttemptOutcome) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var taskID string
	err = tx.QueryRow(`UPDATE attempts SET state = ?, exit_code = ?, error = ?, finished_at = ?
		WHERE id = ? AND state IN (?, ?) RETURNING task_id`,
		o.State, nullInt(o.ExitCode), o.Error, ms(o.Finished), attemptID, AttemptAssigned, AttemptRunning).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	var finished any
	if o.TaskState.Terminal() {
		finished = ms(o.Finished)
	}
	res, err := tx.Exec(`UPDATE tasks SET state = ?, exit_code = ?, error = ?, finished_at = ?,
			failures = failures + ?, lost = lost + ?
		WHERE id = ? AND attempt_id = ? AND state IN (?, ?)`,
		o.TaskState, nullInt(o.ExitCode), o.Error, finished, boolInt(o.CountFailure), boolInt(o.CountLost),
		taskID, attemptID, TaskAssigned, TaskRunning)
	if err != nil {
		return err
	}
	// Outputs belong to the task only if this attempt's outcome was applied
	// (the same exactly-once rule as the result itself).
	if n, _ := res.RowsAffected(); n > 0 && len(o.Outputs) > 0 {
		if _, err := tx.Exec(`DELETE FROM task_outputs WHERE task_id = ?`, taskID); err != nil {
			return err
		}
		for _, f := range o.Outputs {
			if _, err := tx.Exec(`INSERT INTO task_outputs (task_id, path, sha256, size, mode) VALUES (?, ?, ?, ?, ?)`,
				taskID, f.Path, f.SHA256, f.Size, f.Mode); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// OutputFile is one stored output of a task.
type OutputFile struct {
	TaskID string `json:"task_id"`
	Index  int64  `json:"index"` // the task's array index
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
}

func (s *Store) queryOutputs(where string, arg any) ([]*OutputFile, error) {
	rows, err := s.db.Query(`SELECT o.task_id, t.idx, o.path, o.sha256, o.size, o.mode
		FROM task_outputs o JOIN tasks t ON t.id = o.task_id WHERE `+where+` ORDER BY t.idx, o.path`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*OutputFile
	for rows.Next() {
		var f OutputFile
		if err := rows.Scan(&f.TaskID, &f.Index, &f.Path, &f.SHA256, &f.Size, &f.Mode); err != nil {
			return nil, err
		}
		out = append(out, &f)
	}
	return out, rows.Err()
}

func (s *Store) TaskOutputs(taskID string) ([]*OutputFile, error) {
	return s.queryOutputs(`o.task_id = ?`, taskID)
}

func (s *Store) JobOutputs(jobID string) ([]*OutputFile, error) {
	return s.queryOutputs(`t.job_id = ?`, jobID)
}

// ReferencedBlobs returns the content hashes still in use: every job's
// spec (for its inputs) and every stored output.
func (s *Store) ReferencedBlobs() (specs []string, outputs map[string]bool, err error) {
	rows, err := s.db.Query(`SELECT spec FROM jobs`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var spec string
		if err := rows.Scan(&spec); err != nil {
			rows.Close()
			return nil, nil, err
		}
		specs = append(specs, spec)
	}
	rows.Close()
	outputs = map[string]bool{}
	rows, err = s.db.Query(`SELECT DISTINCT sha256 FROM task_outputs`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, nil, err
		}
		outputs[sha] = true
	}
	return specs, outputs, rows.Err()
}
