//go:build linux

package worker

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/NTFespolion307/QuorvexFusion/internal/controller"
	"github.com/NTFespolion307/QuorvexFusion/internal/store"
)

// TestArrayJobSurvivesWorkerDeath runs an array job across two workers,
// kills one mid-job, and checks that every task completes exactly once
// with its output.
func TestArrayJobSurvivesWorkerDeath(t *testing.T) {
	tc := startController(t, 3) // short heartbeat so the dead worker is noticed quickly
	w1 := tc.newWorker(t, tc.joinToken, tc.fp)
	w2 := tc.newWorker(t, tc.joinToken, tc.fp)
	w1.opts.Name, w2.opts.Name = "w1", "w2"
	stop1, _ := runWorker(w1)
	stop2, _ := runWorker(w2)
	defer stop2()

	waitFor(t, 15*time.Second, "both workers online", func() bool {
		p, _ := tc.c.Pool()
		return p.NodesOnline == 2
	})

	const n = 12
	// Each task takes a moment so some are mid-run when w1 dies. This
	// machine's real core count limits parallelism; 0.5 CPU per task packs
	// plenty onto each worker.
	job, err := tc.c.SubmitJob(&controller.JobSpec{
		Command: "sleep 1; echo task {i} on $CLUSTER_TASK_ID", Array: fmt.Sprintf("1-%d", n), CPUs: 0.5,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Kill w1 once it is running something.
	st1, _ := w1.id.loadState()
	waitFor(t, 15*time.Second, "w1 running a task", func() bool {
		tasks, _ := tc.c.Store().ListTasks(job.ID, store.TaskRunning, 100, 0)
		for _, tk := range tasks {
			if tk.NodeID == st1.NodeID {
				return true
			}
		}
		return false
	})
	stop1()

	waitFor(t, 60*time.Second, "all tasks finished", func() bool {
		j, _ := tc.c.Store().GetJob(job.ID)
		return j.Counts.Succeeded+j.Counts.Failed+j.Counts.Canceled == n
	})

	j, _ := tc.c.Store().GetJob(job.ID)
	if j.Counts.Succeeded != n {
		t.Fatalf("counts = %+v, want %d succeeded", j.Counts, n)
	}
	requeued := 0
	for i := 1; i <= n; i++ {
		id := store.TaskID(job.ID, int64(i))
		attempts, _ := tc.c.Store().ListAttempts(id)
		ok := 0
		for _, a := range attempts {
			if a.State == store.AttemptSucceeded {
				ok++
			}
			if a.State == store.AttemptLost {
				requeued++
			}
		}
		if ok != 1 {
			t.Errorf("task %s: %d succeeded attempts, want exactly 1", id, ok)
		}
		data, _, _, err := tc.c.ReadLog(id, 0, controller.LogStdout, 0, 1<<20)
		want := fmt.Sprintf("task %d on %s\n", i, id)
		if err != nil || string(data) != want {
			t.Errorf("task %s output = %q, want %q", id, data, want)
		}
	}
	if requeued == 0 {
		t.Error("no attempt was lost: the test did not exercise the worker death")
	}
	t.Logf("%d attempts were requeued after w1 died", requeued)
}

func TestFailureRetryAndTimeout(t *testing.T) {
	tc := startController(t, 15)
	w := tc.newWorker(t, tc.joinToken, tc.fp)
	stop, _ := runWorker(w)
	defer stop()
	waitFor(t, 15*time.Second, "worker online", func() bool {
		p, _ := tc.c.Pool()
		return p.NodesOnline == 1
	})

	// Fails on attempt 1, succeeds on attempt 2.
	retry, _ := tc.c.SubmitJob(&controller.JobSpec{
		Command: `[ "$CLUSTER_ATTEMPT" = 2 ] || { echo boom >&2; exit 7; }; echo ok`, Retries: 2, CPUs: 0.1,
	})
	slow, _ := tc.c.SubmitJob(&controller.JobSpec{Command: "sleep 30", TimeoutSec: 1, CPUs: 0.1})

	waitFor(t, 30*time.Second, "jobs finished", func() bool {
		a, _ := tc.c.Store().GetTask(store.TaskID(retry.ID, 0))
		b, _ := tc.c.Store().GetTask(store.TaskID(slow.ID, 0))
		return a.State.Terminal() && b.State.Terminal()
	})
	a, _ := tc.c.Store().GetTask(store.TaskID(retry.ID, 0))
	if a.State != store.TaskSucceeded || a.Attempts != 2 || a.Failures != 1 {
		t.Errorf("retry task: %+v", a)
	}
	errLog, _, _, _ := tc.c.ReadLog(a.ID, 1, controller.LogStderr, 0, 1024)
	if string(errLog) != "boom\n" {
		t.Errorf("attempt 1 stderr = %q", errLog)
	}
	b, _ := tc.c.Store().GetTask(store.TaskID(slow.ID, 0))
	if b.State != store.TaskFailed || !strings.Contains(b.Error, "timed out") {
		t.Errorf("timeout task: %+v", b)
	}
}
