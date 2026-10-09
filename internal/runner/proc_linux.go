//go:build linux

package runner

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type taskUser struct {
	name     string
	uid, gid uint32
	groups   []uint32
	home     string
}

// lookupTaskUser resolves the user tasks run as. Switching users requires
// the worker to run as root.
func lookupTaskUser(name string) (*taskUser, error) {
	if name == "" {
		return nil, nil
	}
	u, err := user.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("task user: %w", err)
	}
	if uid, _ := strconv.Atoi(u.Uid); uid == os.Geteuid() {
		return nil, nil // already that user
	}
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("running tasks as %q requires the worker to run as root", name)
	}
	uid, _ := strconv.ParseUint(u.Uid, 10, 32)
	gid, _ := strconv.ParseUint(u.Gid, 10, 32)
	tu := &taskUser{name: name, uid: uint32(uid), gid: uint32(gid), home: u.HomeDir}
	if ids, err := u.GroupIds(); err == nil {
		for _, g := range ids {
			if n, err := strconv.ParseUint(g, 10, 32); err == nil {
				tu.groups = append(tu.groups, uint32(n))
			}
		}
	}
	return tu, nil
}

// proc is a started task process. It runs in its own process group so the
// whole tree can be signalled; with systemd it also has its own scope unit
// (a cgroup), which catches children that escape the process group.
type proc struct {
	cmd       *exec.Cmd
	unit      string // systemd scope name, "" without systemd
	container string // docker container name, for container tasks
}

func scopeName(attemptID string) string { return "cluster-task-" + attemptID + ".scope" }

func (r *Runner) startProcess(a *attempt, work string, stdout, stderr *os.File) (*proc, error) {
	if a.assign.Spec.Image != "" {
		return r.startContainer(a, work, stdout, stderr)
	}
	spec := a.assign.Spec
	argv := []string{"/bin/sh", "-c", spec.Command}
	sys := &syscall.SysProcAttr{Setpgid: true}
	p := &proc{}
	u := r.user

	if r.opts.UseSystemd {
		// systemd-run --scope registers itself in a new cgroup with the
		// given limits and then execs the command, so the PID we get is
		// the task's own shell.
		p.unit = scopeName(a.id)
		scope := []string{"systemd-run", "--scope", "--quiet", "--collect", "--unit", p.unit,
			"-p", fmt.Sprintf("CPUQuota=%d%%", int(math.Ceil(spec.Cpus*100)))}
		if spec.MemoryBytes > 0 {
			scope = append(scope, "-p", fmt.Sprintf("MemoryMax=%d", spec.MemoryBytes), "-p", "MemorySwapMax=0")
		}
		if u != nil {
			// Drop privileges inside the scope (the scope itself must be
			// created as root).
			if _, err := exec.LookPath("setpriv"); err == nil {
				argv = append([]string{"setpriv", "--reuid", u.name, "--regid", strconv.Itoa(int(u.gid)), "--init-groups", "--"}, argv...)
			} else {
				scope = append(scope, "--uid", u.name, "--gid", strconv.Itoa(int(u.gid)))
			}
		}
		argv = append(append(scope, "--"), argv...)
	} else if u != nil {
		sys.Credential = &syscall.Credential{Uid: u.uid, Gid: u.gid, Groups: u.groups}
	}

	if u != nil {
		if err := os.Chown(work, int(u.uid), int(u.gid)); err != nil {
			return nil, err
		}
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = work
	cmd.Env = r.taskEnv(a, work)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = sys
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p.cmd = cmd
	return p, nil
}

// startContainer runs the task in Docker. Docker enforces the CPU, memory
// and GPU limits itself, so no systemd scope is used.
func (r *Runner) startContainer(a *attempt, work string, stdout, stderr *os.File) (*proc, error) {
	spec := a.assign.Spec
	env, err := containerEnv(a)
	if err != nil {
		return nil, err
	}
	envFile := filepath.Join(a.dir, "container.env")
	if err := os.WriteFile(envFile, []byte(strings.Join(env, "\n")+"\n"), 0o600); err != nil {
		return nil, err
	}
	// Run as the task user (or, for an unprivileged worker, as the worker's
	// own user) so files written to /work belong to the right user.
	user := ""
	switch {
	case r.user != nil:
		user = fmt.Sprintf("%d:%d", r.user.uid, r.user.gid)
	case os.Geteuid() != 0:
		user = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	}
	d := dockerRun{
		Name: containerName(a.id), AttemptID: a.id, Image: spec.Image, Command: spec.Command,
		Work: work, EnvFile: envFile, User: user, Shared: r.opts.SharedStorage,
		CPUs: spec.Cpus, Memory: spec.MemoryBytes, GPUs: spec.Gpus,
	}
	// A leftover container with the same name (e.g. after a worker crash)
	// would make docker run fail.
	_ = exec.Command("docker", "rm", "-f", d.Name).Run()
	cmd := exec.Command("docker", d.args()...)
	cmd.Dir = work
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &proc{cmd: cmd, container: d.Name}, nil
}

func (p *proc) pid() int    { return p.cmd.Process.Pid }
func (p *proc) wait() error { return p.cmd.Wait() }
func (p *proc) terminate()  { p.signal(syscall.SIGTERM) }
func (p *proc) kill()       { p.signal(syscall.SIGKILL) }

func (p *proc) signal(sig syscall.Signal) {
	if p.container != "" {
		// Signals to the docker client don't reliably reach the container;
		// ask the daemon. "docker stop" waits, so don't block on it.
		if sig == syscall.SIGKILL {
			_ = exec.Command("docker", "kill", p.container).Run()
		} else {
			_ = exec.Command("docker", "stop", "-t", "10", p.container).Start()
		}
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, sig) // negative PID: the whole process group
	if p.unit != "" {
		systemctl("kill", "--signal="+strconv.Itoa(int(sig)), p.unit)
	}
}

// cleanup kills anything left in the task's scope after the main process
// exited (e.g. background processes it started).
func (p *proc) cleanup() {
	if p.unit != "" {
		systemctl("stop", p.unit)
	}
	if p.container != "" {
		// Normally gone already (--rm). Repeat once: if the task was stopped
		// while "docker run" was still creating the container, the daemon
		// may finish creating it just after the first removal.
		_ = exec.Command("docker", "rm", "-f", p.container).Run()
		time.Sleep(time.Second)
		_ = exec.Command("docker", "rm", "-f", p.container).Run()
	}
}

func systemctl(args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "systemctl", args...).Run()
}

// killOrphan stops a task left running by a previous worker process.
func killOrphan(attemptID, pidText string) {
	if pid, err := strconv.Atoi(strings.TrimSpace(pidText)); err == nil && pid > 1 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		systemctl("stop", scopeName(attemptID))
	}
	if _, err := exec.LookPath("docker"); err == nil {
		_ = exec.Command("docker", "rm", "-f", containerName(attemptID)).Run()
	}
}

// exitStatus turns a Wait error into an exit code and message. Like shells,
// a process killed by signal N reports 128+N.
func exitStatus(err error, memLimited bool) (int32, string) {
	if err == nil {
		return 0, ""
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				return int32(128 + int(ws.Signal())), signalError(ws.Signal().String(), memLimited)
			}
			code := ws.ExitStatus()
			if code == 128+int(syscall.SIGKILL) && memLimited {
				// The shell reports a child killed by SIGKILL as 137.
				return int32(code), "exit code 137 (likely out of memory: the task exceeded its --memory limit)"
			}
			return int32(code), ""
		}
	}
	return -1, err.Error()
}

// chownTo gives a file or directory to the task user.
func chownTo(path string, u *taskUser) error { return os.Lchown(path, int(u.uid), int(u.gid)) }
