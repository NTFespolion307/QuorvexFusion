// Package scheduler decides which node each queued task runs on.
//
// It is pure logic with no I/O: the controller builds a snapshot of nodes
// (capacity and current usage) and the queue, calls Schedule, and carries
// out the returned placements. That keeps the placement rules easy to read
// and to unit-test.
//
// Placement rules, in order:
//  1. Hard constraints: enough free CPU, memory and GPUs; required labels;
//     required capabilities (Docker, NVIDIA toolkit); ephemeral/remote
//     opt-outs; node not draining.
//  2. Among the nodes that pass, the highest score wins (see score).
//
// The queue is walked in order (priority, then submission). A task that
// doesn't fit anywhere right now is skipped, so smaller tasks behind it can
// still use free capacity (backfill).
package scheduler

import (
	"fmt"
	"sort"
)

// epsilon absorbs float rounding when adding up fractional CPUs.
const epsilon = 1e-9

type GPU struct {
	Index  int
	UUID   string
	Vendor string
}

// Node is a snapshot of one online node.
type Node struct {
	ID          string
	CPUs        float64 // usable cores
	MemoryBytes uint64
	GPUs        []GPU

	UsedCPUs   float64
	UsedMemory uint64
	UsedGPUs   map[int]bool // by GPU index

	Labels       map[string]string
	Docker       bool
	NvidiaDocker bool
	Ephemeral    bool
	Remote       bool
	Draining     bool
}

// Task is the resource request of one queued task.
type Task struct {
	ID          string
	CPUs        float64
	MemoryBytes uint64 // 0 = not reserved
	GPUs        int

	Requires map[string]string // labels the node must have
	Prefers  map[string]string // labels that make a node more attractive

	NeedsDocker bool
	NeedsNvidia bool // GPU containers

	AllowEphemeral bool
	AllowRemote    bool

	// PreferNodes are nodes that already hold the task's input data
	// (cached files or shared storage).
	PreferNodes map[string]bool
}

type Placement struct {
	TaskID string
	NodeID string
	GPUs   []GPU
}

func (n *Node) FreeCPUs() float64 { return n.CPUs - n.UsedCPUs }

func (n *Node) FreeMemory() uint64 {
	if n.UsedMemory >= n.MemoryBytes {
		return 0
	}
	return n.MemoryBytes - n.UsedMemory
}

func (n *Node) freeGPUs() []GPU {
	var out []GPU
	for _, g := range n.GPUs {
		if !n.UsedGPUs[g.Index] {
			out = append(out, g)
		}
	}
	return out
}

// eligible checks everything except current usage: could this node ever
// run the task? It returns a reason when not.
func eligible(n *Node, t *Task) string {
	switch {
	case n.Draining:
		return "node is draining"
	case t.CPUs > n.CPUs+epsilon:
		return fmt.Sprintf("needs %.4g CPUs, node has %.4g", t.CPUs, n.CPUs)
	case t.MemoryBytes > n.MemoryBytes:
		return "not enough memory"
	case t.GPUs > len(n.GPUs):
		return fmt.Sprintf("needs %d GPUs, node has %d", t.GPUs, len(n.GPUs))
	case t.NeedsDocker && !n.Docker:
		return "no Docker"
	case t.NeedsNvidia && !n.NvidiaDocker:
		return "no NVIDIA container toolkit"
	case n.Ephemeral && !t.AllowEphemeral:
		return "job does not allow ephemeral nodes"
	case n.Remote && !t.AllowRemote:
		return "job does not allow remote nodes"
	}
	for k, v := range t.Requires {
		if n.Labels[k] != v {
			return fmt.Sprintf("missing label %s=%s", k, v)
		}
	}
	return ""
}

// fitsNow additionally checks current free capacity.
func fitsNow(n *Node, t *Task) bool {
	return eligible(n, t) == "" &&
		t.CPUs <= n.FreeCPUs()+epsilon &&
		t.MemoryBytes <= n.FreeMemory() &&
		t.GPUs <= len(n.freeGPUs())
}

// score ranks eligible nodes for a task; higher is better.
func score(n *Node, t *Task) float64 {
	s := 0.0
	if t.PreferNodes[n.ID] {
		s += 100 // input data is already there
	}
	for k, v := range t.Prefers {
		if n.Labels[k] == v {
			s += 10
		}
	}
	if !n.Remote {
		s += 5 // local before remote
	}
	if !n.Ephemeral {
		s += 3 // stable machines before rented ones
	}
	if t.GPUs == 0 && len(n.GPUs) > 0 {
		s -= 2 // keep GPU machines free for GPU work when possible
	}
	// Spread load: prefer the node that stays emptiest (0..1).
	if n.CPUs > 0 {
		s += (n.FreeCPUs() - t.CPUs) / n.CPUs
	}
	return s
}

// Schedule places as many tasks as fit, in queue order. It updates the
// nodes' usage as it goes, so the snapshot reflects the placements.
func Schedule(nodes []*Node, queue []*Task) []Placement {
	// Deterministic order for equal scores.
	sorted := append([]*Node(nil), nodes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	var out []Placement
	for _, t := range queue {
		var best *Node
		bestScore := 0.0
		for _, n := range sorted {
			if !fitsNow(n, t) {
				continue
			}
			if s := score(n, t); best == nil || s > bestScore {
				best, bestScore = n, s
			}
		}
		if best == nil {
			continue
		}
		p := Placement{TaskID: t.ID, NodeID: best.ID}
		if t.GPUs > 0 {
			p.GPUs = best.freeGPUs()[:t.GPUs]
		}
		Reserve(best, t.CPUs, t.MemoryBytes, p.GPUs)
		out = append(out, p)
	}
	return out
}

// Reserve records resources as used on a node.
func Reserve(n *Node, cpus float64, mem uint64, gpus []GPU) {
	n.UsedCPUs += cpus
	n.UsedMemory += mem
	if n.UsedGPUs == nil {
		n.UsedGPUs = map[int]bool{}
	}
	for _, g := range gpus {
		n.UsedGPUs[g.Index] = true
	}
}

// Explain says why a task cannot be placed on any node even when they are
// idle (e.g. "needs 8 GPUs, node has 4"), or returns "" if some node could
// run it once capacity frees up.
func Explain(nodes []*Node, t *Task) string {
	if len(nodes) == 0 {
		return "no nodes online"
	}
	reason := ""
	for _, n := range nodes {
		r := eligible(n, t)
		if r == "" {
			return ""
		}
		reason = r
	}
	if len(nodes) == 1 {
		return reason
	}
	return "no online node qualifies (e.g. " + reason + ")"
}
