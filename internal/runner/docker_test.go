package runner

import (
	"strings"
	"testing"

	pb "github.com/NTFespolion307/QuorvexFusion/internal/clusterpb"
)

func TestDockerRunArgs(t *testing.T) {
	d := dockerRun{
		Name: "cluster-task-a1", AttemptID: "a1", Image: "nvidia/cuda:12.4.1-base-ubuntu22.04",
		Command: "nvidia-smi && echo done", Work: "/var/lib/w/tasks/a1/work", EnvFile: "/var/lib/w/tasks/a1/container.env",
		User: "998:998", CPUs: 2.5, Memory: 4 << 30,
		GPUs: []*pb.GPUAssignment{{Index: 1, Uuid: "GPU-aaa", Vendor: "nvidia"}, {Index: 3, Uuid: "GPU-bbb", Vendor: "nvidia"}},
	}
	got := strings.Join(d.args(), " ")
	for _, want := range []string{
		"run --rm --init --name cluster-task-a1",
		"-v /var/lib/w/tasks/a1/work:/work -w /work",
		"--cpus 2.5",
		"--memory 4294967296 --memory-swap 4294967296",
		"--user 998:998",
		`--gpus "device=GPU-aaa,GPU-bbb"`,
		"--entrypoint /bin/sh nvidia/cuda:12.4.1-base-ubuntu22.04 -c",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// The command is one argument, not split by spaces.
	args := d.args()
	if args[len(args)-1] != "nvidia-smi && echo done" {
		t.Errorf("last arg = %q", args[len(args)-1])
	}

	// No command: the image's default runs, nothing overridden.
	d2 := dockerRun{Name: "n", AttemptID: "a", Image: "alpine", Work: "/w", EnvFile: "/e"}
	a2 := d2.args()
	if a2[len(a2)-1] != "alpine" || strings.Contains(strings.Join(a2, " "), "--entrypoint") {
		t.Errorf("default command args: %v", a2)
	}
	for _, flag := range []string{"--gpus", "--memory", "--user", "--cpus"} {
		if strings.Contains(strings.Join(a2, " "), flag) {
			t.Errorf("unexpected %s in %v", flag, a2)
		}
	}

	// AMD: driver devices, not --gpus.
	d3 := dockerRun{Name: "n", Image: "rocm/pytorch", Work: "/w", EnvFile: "/e",
		GPUs: []*pb.GPUAssignment{{Index: 0, Vendor: "amd"}}}
	if s := strings.Join(d3.args(), " "); !strings.Contains(s, "--device /dev/kfd --device /dev/dri") || strings.Contains(s, "--gpus") {
		t.Errorf("amd args: %s", s)
	}
}

func TestContainerEnv(t *testing.T) {
	t.Setenv("CLUSTER_JOIN_TOKEN", "secret-should-not-leak")
	t.Setenv("PATH", "/host/only/bin")
	a := &attempt{assign: &pb.AssignTask{AttemptId: "a1", TaskId: "j.3", JobId: "j", Attempt: 2, ArrayIndex: 3,
		Spec: &pb.TaskSpec{Cpus: 2, Env: map[string]string{"MODEL": "big"},
			Gpus: []*pb.GPUAssignment{{Index: 0, Uuid: "GPU-x", Vendor: "nvidia"}}}}}
	env, err := containerEnv(a)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	want := map[string]string{"CLUSTER_ARRAY_INDEX": "3", "CLUSTER_WORKDIR": "/work", "HOME": "/work",
		"MODEL": "big", "OMP_NUM_THREADS": "2", "CLUSTER_ATTEMPT": "2"}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %q, want %q", k, m[k], v)
		}
	}
	for _, k := range []string{"CLUSTER_JOIN_TOKEN", "PATH", "CUDA_VISIBLE_DEVICES"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s must not be passed into the container", k)
		}
	}
	a.assign.Spec.Env = map[string]string{"BAD": "two\nlines"}
	if _, err := containerEnv(a); err == nil {
		t.Error("multi-line value accepted")
	}
}
