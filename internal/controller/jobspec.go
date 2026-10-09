package controller

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/NTFespolion307/QuorvexFusion/internal/blobstore"
	"github.com/NTFespolion307/QuorvexFusion/internal/files"
)

// JobSpec is what a user submits. It is stored as JSON with the job, and
// every task of the job runs the same spec with {i} replaced by its index.
type JobSpec struct {
	Name    string            `json:"name,omitempty"`
	Command string            `json:"command"` // run with /bin/sh -c
	Env     map[string]string `json:"env,omitempty"`

	// Image runs each task in this Docker image (e.g. "python:3.12" or
	// "ghcr.io/org/tool:1.4"). The command then runs inside the container
	// with /bin/sh -c; with no command, the image's own default runs.
	Image string `json:"image,omitempty"`

	CPUs        float64 `json:"cpus,omitempty"`         // default 1
	MemoryBytes uint64  `json:"memory_bytes,omitempty"` // 0 = no reservation or limit
	GPUs        int     `json:"gpus,omitempty"`

	Retries    int   `json:"retries,omitempty"`         // extra attempts after a failure
	TimeoutSec int64 `json:"timeout_seconds,omitempty"` // per attempt; 0 = none
	Priority   int   `json:"priority,omitempty"`        // higher runs first

	// Array is "", a count ("16" = indices 1..16), "1-500", "0-99:10"
	// (step) or "1,4,9"; one task per index. See ParseArray.
	Array string `json:"array,omitempty"`

	Requires map[string]string `json:"requires,omitempty"` // node labels that must match
	Prefers  map[string]string `json:"prefers,omitempty"`  // node labels that are preferred

	// Nil means allowed. Set to false to keep the job on stable/local nodes.
	AllowEphemeral *bool `json:"allow_ephemeral,omitempty"`
	AllowRemote    *bool `json:"allow_remote,omitempty"`

	// Inputs are placed in each task's working directory before it starts.
	Inputs []InputSpec `json:"inputs,omitempty"`
	// Outputs are globs (relative to the working directory, "**" allowed)
	// uploaded back to the controller after each task.
	Outputs []string `json:"outputs,omitempty"`
}

// InputSpec is one input file: either uploaded content (SHA256, stored on
// the controller and downloaded by workers) or a path in the nodes' shared
// storage (Shared), which is linked without any transfer.
type InputSpec struct {
	Path   string `json:"path"`             // destination, relative to the working directory
	SHA256 string `json:"sha256,omitempty"` // uploaded content
	Size   int64  `json:"size,omitempty"`
	Mode   uint32 `json:"mode,omitempty"`   // permission bits; 0 = 0644
	Shared string `json:"shared,omitempty"` // path relative to the shared storage root
}

const (
	maxInputs  = 100000
	maxOutputs = 100
)

func (s *JobSpec) needsShared() bool {
	for _, in := range s.Inputs {
		if in.Shared != "" {
			return true
		}
	}
	return false
}

// inputBytes is the total size of transferred inputs.
func (s *JobSpec) inputBytes() int64 {
	var n int64
	for _, in := range s.Inputs {
		n += in.Size
	}
	return n
}

const (
	maxArrayTasks = 100000
	maxRetries    = 100
)

// Normalize fills defaults and validates the spec.
func (s *JobSpec) Normalize() error {
	s.Command = strings.TrimSpace(s.Command)
	s.Image = strings.TrimSpace(s.Image)
	if s.Command == "" && s.Image == "" {
		return errors.New("a command (or a Docker image) is required")
	}
	if strings.ContainsAny(s.Image, " \t\n") || strings.HasPrefix(s.Image, "-") {
		return fmt.Errorf("invalid image name %q", s.Image)
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
	if err := s.normalizeFiles(); err != nil {
		return err
	}
	if s.Name == "" && s.Command == "" {
		s.Name = s.Image
	}
	if s.Name == "" {
		s.Name = s.Command
		if len(s.Name) > 60 {
			s.Name = s.Name[:57] + "..."
		}
	}
	return nil
}

// normalizeFiles validates input and output paths. Whether uploaded inputs
// exist is checked by the controller (it owns the blob store).
func (s *JobSpec) normalizeFiles() error {
	if len(s.Inputs) > maxInputs {
		return fmt.Errorf("at most %d input files", maxInputs)
	}
	seen := map[string]bool{}
	for i := range s.Inputs {
		in := &s.Inputs[i]
		p, err := files.CleanRel(in.Path)
		if err != nil {
			return fmt.Errorf("input: %w", err)
		}
		if seen[p] {
			return fmt.Errorf("input %q given twice", p)
		}
		seen[p] = true
		in.Path = p
		switch {
		case in.Shared != "" && in.SHA256 != "":
			return fmt.Errorf("input %q: give either sha256 or shared, not both", p)
		case in.Shared != "":
			if in.Shared, err = files.CleanRel(in.Shared); err != nil {
				return fmt.Errorf("shared input: %w", err)
			}
		case !blobstore.ValidSHA(in.SHA256):
			return fmt.Errorf("input %q: missing or invalid sha256", p)
		}
		if in.Mode == 0 {
			in.Mode = 0o644
		}
		in.Mode &= 0o777
	}
	if len(s.Outputs) > maxOutputs {
		return fmt.Errorf("at most %d output patterns", maxOutputs)
	}
	for i, o := range s.Outputs {
		o = strings.TrimSpace(o)
		if _, err := files.CleanRel(strings.ReplaceAll(o, "**", "x")); err != nil {
			return fmt.Errorf("output: %w", err)
		}
		s.Outputs[i] = o
	}
	return nil
}

func (s *JobSpec) allowEphemeral() bool { return s.AllowEphemeral == nil || *s.AllowEphemeral }
func (s *JobSpec) allowRemote() bool    { return s.AllowRemote == nil || *s.AllowRemote }

// ParseArray expands an array expression into sorted, unique indices:
//
//	""         one task, index 0
//	"16"       16 tasks, indices 1..16 (a bare number is a count)
//	"0-99"     a range
//	"0-99:10"  a range with a step
//	"1,4,9"    a list (items may be ranges; "7," is the single index 7)
func ParseArray(expr string) ([]int64, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return []int64{0}, nil
	}
	if n, err := strconv.ParseInt(expr, 10, 64); err == nil {
		if n < 1 || n > maxArrayTasks {
			return nil, fmt.Errorf("array count must be between 1 and %d", maxArrayTasks)
		}
		out := make([]int64, n)
		for i := range out {
			out[i] = int64(i) + 1
		}
		return out, nil
	}
	seen := map[int64]bool{}
	for _, part := range strings.Split(expr, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
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
	if len(seen) == 0 {
		return nil, fmt.Errorf("array %q has no indices", expr)
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
