package runner

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	pb "github.com/NTFespolion307/QuorvexFusion/internal/clusterpb"
)

// Docker tasks run with the docker CLI (no Docker SDK dependency):
//
//	docker run --rm --init --name cluster-task-<attempt> \
//	  --cpus N [--memory B --memory-swap B] [--gpus "device=<uuids>"] \
//	  -v <working dir>:/work -w /work --env-file <file> [--user uid:gid] \
//	  [--entrypoint /bin/sh] IMAGE [-c COMMAND]
//
// The working directory (with inputs already in place) is the same one a
// plain task gets, so inputs, outputs and logs work identically.

func containerName(attemptID string) string { return "cluster-task-" + attemptID }

type dockerRun struct {
	Name      string
	AttemptID string
	Image     string
	Command   string // "" = the image's default command
	Work      string // host path mounted at /work
	EnvFile   string
	User      string // "uid:gid", or "" for the image's default user
	Shared    string // node shared storage, mounted at the same path
	CPUs      float64
	Memory    uint64
	GPUs      []*pb.GPUAssignment
}

// args builds the docker run command line.
func (d dockerRun) args() []string {
	a := []string{"run", "--rm", "--init", "--name", d.Name, "--label", "cluster.attempt=" + d.AttemptID,
		"-v", d.Work + ":/work", "-w", "/work", "--env-file", d.EnvFile}
	if d.CPUs > 0 {
		a = append(a, "--cpus", strconv.FormatFloat(d.CPUs, 'f', -1, 64))
	}
	if d.Memory > 0 {
		// Equal swap limit: no swapping beyond the memory limit.
		a = append(a, "--memory", strconv.FormatUint(d.Memory, 10), "--memory-swap", strconv.FormatUint(d.Memory, 10))
	}
	if d.User != "" {
		a = append(a, "--user", d.User)
	}
	if d.Shared != "" {
		a = append(a, "-v", d.Shared+":"+d.Shared)
	}
	var nvidia []string
	amd := false
	for _, g := range d.GPUs {
		if g.Vendor == "amd" {
			amd = true
		} else if g.Uuid != "" {
			nvidia = append(nvidia, g.Uuid)
		} else {
			nvidia = append(nvidia, strconv.Itoa(int(g.Index)))
		}
	}
	if len(nvidia) > 0 {
		// The quotes are part of the value: docker's syntax for a device
		// list containing commas.
		a = append(a, "--gpus", `"device=`+strings.Join(nvidia, ",")+`"`)
	}
	if amd {
		// ROCm needs the kernel driver devices; which GPUs are visible is
		// set through ROCR_VISIBLE_DEVICES in the environment.
		a = append(a, "--device", "/dev/kfd", "--device", "/dev/dri", "--group-add", "video")
	}
	if d.Command != "" {
		return append(a, "--entrypoint", "/bin/sh", d.Image, "-c", d.Command)
	}
	return append(a, d.Image)
}

// containerEnv is the environment inside a task's container: the task's
// variables only (never the worker's own environment, whose PATH etc.
// would not fit the image). GPU visibility is handled by --gpus, which
// renumbers the container's GPUs from 0, so CUDA_VISIBLE_DEVICES is not set.
func containerEnv(a *attempt) ([]string, error) {
	env := clusterVars(a, "/work")
	env["HOME"] = "/work"
	var amd []string
	for _, g := range a.assign.Spec.Gpus {
		if g.Vendor == "amd" {
			amd = append(amd, strconv.Itoa(int(g.Index)))
		}
	}
	if len(amd) > 0 {
		env["ROCR_VISIBLE_DEVICES"] = strings.Join(amd, ",")
	}
	for k, v := range a.assign.Spec.Env {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		// docker --env-file takes one KEY=VALUE per line, literally.
		if strings.ContainsAny(v, "\r\n") {
			return nil, fmt.Errorf("environment variable %s contains a line break, which containers can't receive", k)
		}
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out, nil
}

// ensureImage pulls the image unless it is already present, writing the
// pull output to log (the task's stderr).
func ensureImage(ctx context.Context, image string, log io.Writer) error {
	if exec.CommandContext(ctx, "docker", "image", "inspect", image).Run() == nil {
		return nil
	}
	fmt.Fprintf(log, "[cluster] pulling image %s\n", image)
	cmd := exec.CommandContext(ctx, "docker", "pull", image)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pulling image %s failed (see the task log)", image)
	}
	return nil
}
