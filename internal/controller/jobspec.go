package controller

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// JobSpec is what a user submits. It is stored as JSON with the job, and
// every task of the job runs the same spec with {i} replaced by its index.
type JobSpec struct {
	Name    string            `json:"name,omitempty"`
	Command string            `json:"command"` // run with /bin/sh -c
	Env     map[string]string `json:"env,omitempty"`

	CPUs        float64 `json:"cpus,omitempty"`         // default 1
	MemoryBytes uint64  `json:"memory_bytes,omitempty"` // 0 = no reservation or limit
	GPUs        int     `json:"gpus,omitempty"`

	Retries    int   `json:"retries,omitempty"`         // extra attempts after a failure
	TimeoutSec int64 `json:"timeout_seconds,omitempty"` // per attempt; 0 = none
	Priority   int   `json:"priority,omitempty"`        // higher runs first

	// Array is "", "1-500", "0-99:10" (step) or "1,4,9"; one task per index.
	Array string `json:"array,omitempty"`

	Requires map[string]string `json:"requires,omitempty"` // node labels that must match
	Prefers  map[string]string `json:"prefers,omitempty"`  // node labels that are preferred

	// Nil means allowed. Set to false to keep the job on stable/local nodes.
	AllowEphemeral *bool `json:"allow_ephemeral,omitempty"`
	AllowRemote    *bool `json:"allow_remote,omitempty"`
}

const (
	maxArrayTasks = 100000
	maxRetries    = 100
)

// Normalize fills defaults and validates the spec.
func (s *JobSpec) Normalize() error {
	s.Command = strings.TrimSpace(s.Command)
	if s.Command == "" {
		return errors.New("command is required")
	}
	if s.CPUs == 0 {
		s.CPUs = 1
	}
	switch {
	case s.CPUs < 0:
		return errors.New("cpus must be positive")
	case s.GPUs < 0:
		return errors.New("gpus must not be negative")
	case s.Retries < 0 || s.Retries > maxRetries:
		return fmt.Errorf("retries must be between 0 and %d", maxRetries)
	case s.TimeoutSec < 0:
		return errors.New("timeout must not be negative")
	}
	if _, err := ParseArray(s.Array); err != nil {
		return err
	}
	if s.Name == "" {
		s.Name = s.Command
		if len(s.Name) > 60 {
			s.Name = s.Name[:57] + "..."
		}
	}
	return nil
}

func (s *JobSpec) allowEphemeral() bool { return s.AllowEphemeral == nil || *s.AllowEphemeral }
func (s *JobSpec) allowRemote() bool    { return s.AllowRemote == nil || *s.AllowRemote }

// ParseArray expands an array expression into sorted, unique indices.
// "" yields the single index 0.
func ParseArray(expr string) ([]int64, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return []int64{0}, nil
	}
	seen := map[int64]bool{}
	for _, part := range strings.Split(expr, ",") {
		part = strings.TrimSpace(part)
		rng, stepStr, hasStep := strings.Cut(part, ":")
		step := int64(1)
		if hasStep {
			var err error
			if step, err = strconv.ParseInt(stepStr, 10, 64); err != nil || step < 1 {
				return nil, fmt.Errorf("array %q: bad step", part)
			}
		}
		loStr, hiStr, isRange := strings.Cut(rng, "-")
		lo, err := strconv.ParseInt(strings.TrimSpace(loStr), 10, 64)
		if err != nil || lo < 0 {
			return nil, fmt.Errorf("array %q: expected N, N-M or N-M:STEP", part)
		}
		hi := lo
		if isRange {
			if hi, err = strconv.ParseInt(strings.TrimSpace(hiStr), 10, 64); err != nil || hi < lo {
				return nil, fmt.Errorf("array %q: bad range", part)
			}
		}
		if (hi-lo)/step+1+int64(len(seen)) > maxArrayTasks {
			return nil, fmt.Errorf("array has more than %d tasks", maxArrayTasks)
		}
		for i := lo; i <= hi; i += step {
			seen[i] = true
		}
	}
	out := make([]int64, 0, len(seen))
	for i := range seen {
		out = append(out, i)
	}
	sort.Slice(out, func(a, b int) bool { return out[a] < out[b] })
	return out, nil
}

// substitute replaces {i} with the task's array index.
func substitute(s string, index int64) string {
	return strings.ReplaceAll(s, "{i}", strconv.FormatInt(index, 10))
}
