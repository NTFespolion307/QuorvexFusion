//go:build linux

package worker

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/NTFespolion307/QuorvexFusion/internal/controller"
	"github.com/NTFespolion307/QuorvexFusion/internal/store"
)

const testImage = "alpine:3.20"

func needDocker(t *testing.T) {
	t.Helper()
	if exec.Command("docker", "info").Run() != nil {
		t.Skip("docker is not usable here")
	}
}

func waitTask(t *testing.T, tc *testCluster, id string, within time.Duration) *store.Task {
	t.Helper()
	var tk *store.Task
	waitFor(t, within, "task "+id+" finished", func() bool {
		tk, _ = tc.c.Store().GetTask(id)
		return tk != nil && tk.State.Terminal()
	})
	return tk
}

func TestDockerTasks(t *testing.T) {
	needDocker(t)
	tc := startController(t, 15)
	w := tc.newWorker(t, tc.joinToken, tc.fp)
	stop, _ := runWorker(w)
	defer stop()
	waitFor(t, 20*time.Second, "worker online with docker", func() bool {
		nodes, _ := tc.c.ListNodeViews()
		return len(nodes) == 1 && nodes[0].Status == "online" && nodes[0].Hardware.Docker
	})

	// Inputs in, outputs out, limits applied, runs as the worker's user.
	sha, size, _ := tc.c.Blobs().Put(strings.NewReader("hello container\n"))
	job, err := tc.c.SubmitJob(&controller.JobSpec{
		Image: testImage, CPUs: 0.5, MemoryBytes: 64 << 20,
		Inputs:  []controller.InputSpec{{Path: "in.txt", SHA256: sha, Size: size}},
		Outputs: []string{"out/*"},
		Command: `set -e
echo "cpu.max=$(cat /sys/fs/cgroup/cpu.max)"
echo "memory.max=$(cat /sys/fs/cgroup/memory.max)"
echo "uid=$(id -u) pwd=$PWD home=$HOME idx=$CLUSTER_ARRAY_INDEX"
mkdir -p out
tr a-z A-Z < in.txt > out/upper.txt`,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := store.TaskID(job.ID, 0)
	tk := waitTask(t, tc, id, 120*time.Second) // may include pulling the image
	log, _, _, _ := tc.c.ReadLog(id, 0, controller.LogCombined, 0, 1<<16)
	if tk.State != store.TaskSucceeded {
		t.Fatalf("container task %s: %s\n%s", tk.State, tk.Error, log)
	}
	for _, want := range []string{"cpu.max=50000 100000", "memory.max=67108864",
		fmt.Sprintf("uid=%d pwd=/work home=/work idx=0", os.Getuid())} {
		if !bytes.Contains(log, []byte(want)) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	outs, _ := tc.c.Store().TaskOutputs(id)
	if len(outs) != 1 || readBlob(t, tc, outs[0].SHA256) != "HELLO CONTAINER\n" {
		t.Errorf("outputs = %+v", outs)
	}

	// The image's default command (no command given).
	job2, _ := tc.c.SubmitJob(&controller.JobSpec{Image: testImage, CPUs: 0.5})
	if tk := waitTask(t, tc, store.TaskID(job2.ID, 0), 60*time.Second); tk.State != store.TaskSucceeded {
		t.Errorf("default command: %+v", tk)
	}

	// Exceeding the memory limit is reported as out of memory.
	job3, _ := tc.c.SubmitJob(&controller.JobSpec{Image: testImage, CPUs: 0.5, MemoryBytes: 32 << 20,
		Command: "head -c 200m /dev/zero | tail > /dev/null"})
	if tk := waitTask(t, tc, store.TaskID(job3.ID, 0), 60*time.Second); tk.State != store.TaskFailed ||
		!strings.Contains(tk.Error, "out of memory") {
		t.Errorf("OOM task: state=%s error=%q", tk.State, tk.Error)
	}

	// Cancel stops and removes the container.
	job4, _ := tc.c.SubmitJob(&controller.JobSpec{Image: testImage, CPUs: 0.5, Command: "sleep 300"})
	id4 := store.TaskID(job4.ID, 0)
	waitFor(t, 30*time.Second, "container running", func() bool {
		tk, _ := tc.c.Store().GetTask(id4)
		return tk.State == store.TaskRunning
	})
	tk4, _ := tc.c.Store().GetTask(id4)
	name := "cluster-task-" + tk4.AttemptID
	// The docker client starts a moment before the daemon creates the container.
	waitFor(t, 15*time.Second, "container "+name+" running", func() bool {
		out, _ := exec.Command("docker", "ps", "-q", "-f", "name="+name).Output()
		return len(bytes.TrimSpace(out)) > 0
	})
	if err := tc.c.CancelJob(job4.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "container removed after cancel", func() bool {
		out, _ := exec.Command("docker", "ps", "-aq", "-f", "name="+name).Output()
		return len(bytes.TrimSpace(out)) == 0
	})
	if tk := waitTask(t, tc, id4, 10*time.Second); tk.State != store.TaskCanceled {
		t.Errorf("canceled container task: %+v", tk)
	}

	// A missing image fails clearly instead of hanging.
	job5, _ := tc.c.SubmitJob(&controller.JobSpec{Image: "localhost:1/does-not-exist:nope", CPUs: 0.5, Command: "true"})
	if tk := waitTask(t, tc, store.TaskID(job5.ID, 0), 60*time.Second); tk.State != store.TaskFailed ||
		!strings.Contains(tk.Error, "pulling image") {
		t.Errorf("missing image: state=%s error=%q", tk.State, tk.Error)
	}
}
