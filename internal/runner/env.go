package runner

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

// taskEnv builds a task's environment:
//
//  1. the worker's own environment (so e.g. CUDA/conda paths set up inside
//     a container are visible), minus the worker's CLUSTER_* settings,
//     which include the join token, and systemd service internals;
//  2. CLUSTER_* variables describing the task;
//  3. GPU visibility and thread-count defaults;
//  4. the job's own env, which overrides everything.
func (r *Runner) taskEnv(a *attempt, work string) []string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(k, "CLUSTER_"),
			k == "NOTIFY_SOCKET", k == "INVOCATION_ID", k == "JOURNAL_STREAM", k == "LISTEN_FDS", k == "LISTEN_PID":
			continue
		}
		env[k] = v
	}
	if env["PATH"] == "" {
		env["PATH"] = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	if env["LANG"] == "" {
		env["LANG"] = "C.UTF-8"
	}
	env["HOME"] = work
	if r.user != nil {
		if r.user.home != "" {
			env["HOME"] = r.user.home
		}
	}

	as, spec := a.assign, a.assign.Spec
	env["CLUSTER_JOB_ID"] = as.JobId
	env["CLUSTER_TASK_ID"] = as.TaskId
	env["CLUSTER_ATTEMPT_ID"] = as.AttemptId
	env["CLUSTER_ATTEMPT"] = strconv.Itoa(int(as.Attempt))
	env["CLUSTER_ARRAY_INDEX"] = strconv.FormatInt(as.ArrayIndex, 10)
	env["CLUSTER_CPUS"] = strconv.FormatFloat(spec.Cpus, 'f', -1, 64)
	env["CLUSTER_WORKDIR"] = work

	// Libraries like OpenMP/MKL otherwise start one thread per machine
	// core, oversubscribing the CPUs the task was given.
	threads := strconv.Itoa(max(1, int(math.Ceil(spec.Cpus))))
	for _, k := range []string{"OMP_NUM_THREADS", "MKL_NUM_THREADS", "OPENBLAS_NUM_THREADS"} {
		env[k] = threads
	}

	// Each task sees only its own GPUs. NVIDIA devices are given by UUID so
	// the mapping can't be confused by device ordering.
	var nvidia, amd []string
	for _, g := range spec.Gpus {
		if g.Vendor == "amd" {
			amd = append(amd, strconv.Itoa(int(g.Index)))
		} else if g.Uuid != "" {
			nvidia = append(nvidia, g.Uuid)
		} else {
			nvidia = append(nvidia, strconv.Itoa(int(g.Index)))
		}
	}
	env["CUDA_VISIBLE_DEVICES"] = strings.Join(nvidia, ",") // empty: no GPUs for this task
	if len(amd) > 0 {
		env["ROCR_VISIBLE_DEVICES"] = strings.Join(amd, ",")
		env["HIP_VISIBLE_DEVICES"] = strings.Join(amd, ",")
	}

	for k, v := range spec.Env {
		env[k] = v
	}

	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, fmt.Sprintf("%s=%s", k, v))
	}
	sort.Strings(out)
	return out
}
