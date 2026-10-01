//go:build !linux

package runner

import "os"

// Tasks only run on Linux workers. These stubs keep the worker compiling
// on other systems for development; every task fails to start there.

type taskUser struct{ home string }

func lookupTaskUser(name string) (*taskUser, error) { return nil, nil }

type proc struct{}

func (r *Runner) startProcess(a *attempt, work string, stdout, stderr *os.File) (*proc, error) {
	return nil, errUnsupported
}

func (p *proc) pid() int    { return 0 }
func (p *proc) wait() error { return nil }
func (p *proc) terminate()  {}
func (p *proc) kill()       {}
func (p *proc) cleanup()    {}

func killOrphan(attemptID, pidText string) {}

func exitStatus(err error, memLimited bool) (int32, string) {
	if err == nil {
		return 0, ""
	}
	return -1, err.Error()
}
