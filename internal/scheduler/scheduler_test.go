package scheduler

import (
	"testing"
)

const gib = 1 << 30

func node(id string, cpus float64, memGiB uint64, gpus int) *Node {
	n := &Node{ID: id, CPUs: cpus, MemoryBytes: memGiB * gib, Labels: map[string]string{}}
	for i := 0; i < gpus; i++ {
		n.GPUs = append(n.GPUs, GPU{Index: i, UUID: "GPU-" + id + string(rune('a'+i)), Vendor: "nvidia"})
	}
	return n
}

func task(id string, cpus float64) *Task {
	return &Task{ID: id, CPUs: cpus, AllowEphemeral: true, AllowRemote: true}
}

func placedOn(ps []Placement) map[string]string {
	m := map[string]string{}
	for _, p := range ps {
		m[p.TaskID] = p.NodeID
	}
	return m
}

func TestCPUAccountingAndBackfill(t *testing.T) {
	a := node("a", 4, 16, 0)
	// big doesn't fit after the first two take 3 cores, but small backfills.
	q := []*Task{task("t1", 2), task("t2", 1), task("big", 2), task("small", 1)}
	got := placedOn(Schedule([]*Node{a}, q))
	if got["t1"] != "a" || got["t2"] != "a" || got["small"] != "a" {
		t.Errorf("placements = %v", got)
	}
	if _, ok := got["big"]; ok {
		t.Error("big task placed despite only 1 free core")
	}
	if a.UsedCPUs != 4 {
		t.Errorf("used CPUs = %v, want 4", a.UsedCPUs)
	}
}

func TestFractionalCPUs(t *testing.T) {
	a := node("a", 1, 1, 0)
	var q []*Task
	for i := 0; i < 10; i++ {
		q = append(q, task(string(rune('a'+i)), 0.1))
	}
	if n := len(Schedule([]*Node{a}, q)); n != 10 {
		t.Errorf("placed %d of ten 0.1-CPU tasks on one core", n)
	}
}

func TestMemory(t *testing.T) {
	a := node("a", 16, 8, 0)
	t1, t2 := task("t1", 1), task("t2", 1)
	t1.MemoryBytes, t2.MemoryBytes = 6*gib, 6*gib
	if n := len(Schedule([]*Node{a}, []*Task{t1, t2})); n != 1 {
		t.Errorf("placed %d tasks needing 12GiB on an 8GiB node", n)
	}
}

func TestGPUAssignment(t *testing.T) {
	a := node("a", 32, 64, 4)
	a.UsedGPUs = map[int]bool{0: true}
	t1, t2 := task("t1", 1), task("t2", 1)
	t1.GPUs, t2.GPUs = 2, 2
	ps := Schedule([]*Node{a}, []*Task{t1, t2})
	if len(ps) != 1 {
		t.Fatalf("placed %d tasks; only 3 GPUs free", len(ps))
	}
	if g := ps[0].GPUs; len(g) != 2 || g[0].Index != 1 || g[1].Index != 2 {
		t.Errorf("assigned GPUs %+v, want indices 1,2", g)
	}
}

func TestGPUTaskGoesToGPUNode_CPUTaskAvoidsIt(t *testing.T) {
	cpuNode, gpuNode := node("cpu", 8, 16, 0), node("gpu", 8, 16, 1)
	g := task("g", 1)
	g.GPUs = 1
	got := placedOn(Schedule([]*Node{cpuNode, gpuNode}, []*Task{g, task("c", 1)}))
	if got["g"] != "gpu" || got["c"] != "cpu" {
		t.Errorf("placements = %v", got)
	}
}

func TestCapabilitiesAndLabels(t *testing.T) {
	plain := node("plain", 8, 16, 0)
	dock := node("dock", 8, 16, 0)
	dock.Docker = true
	dock.Labels["zone"] = "home"

	d := task("d", 1)
	d.NeedsDocker = true
	l := task("l", 1)
	l.Requires = map[string]string{"zone": "home"}
	never := task("never", 1)
	never.Requires = map[string]string{"zone": "mars"}

	got := placedOn(Schedule([]*Node{plain, dock}, []*Task{d, l, never}))
	if got["d"] != "dock" || got["l"] != "dock" {
		t.Errorf("placements = %v", got)
	}
	if _, ok := got["never"]; ok {
		t.Error("task with unsatisfiable label placed")
	}
	if Explain([]*Node{plain, dock}, never) == "" {
		t.Error("Explain gave no reason for an unsatisfiable task")
	}
	if Explain([]*Node{plain, dock}, d) != "" {
		t.Error("Explain claims a satisfiable task is impossible")
	}
}

func TestEphemeralRemoteDraining(t *testing.T) {
	eph := node("eph", 8, 16, 0)
	eph.Ephemeral = true
	rem := node("rem", 8, 16, 0)
	rem.Remote = true
	drn := node("drn", 64, 64, 0)
	drn.Draining = true

	strict := task("strict", 1)
	strict.AllowEphemeral, strict.AllowRemote = false, false
	if ps := Schedule([]*Node{eph, rem, drn}, []*Task{strict}); len(ps) != 0 {
		t.Errorf("strict task placed on %v", ps)
	}
	// Allowed everywhere: local stable nodes win over remote/ephemeral ones.
	local := node("local", 8, 16, 0)
	got := placedOn(Schedule([]*Node{eph, rem, local}, []*Task{task("any", 1)}))
	if got["any"] != "local" {
		t.Errorf("placed on %q, want local", got["any"])
	}
}

func TestPreferences(t *testing.T) {
	a, b := node("a", 8, 16, 0), node("b", 8, 16, 0)
	b.Labels["gpu"] = "4090"
	p := task("p", 1)
	p.Prefers = map[string]string{"gpu": "4090"}
	if got := placedOn(Schedule([]*Node{a, b}, []*Task{p})); got["p"] != "b" {
		t.Errorf("preferred label ignored: %v", got)
	}

	a2, b2 := node("a", 8, 16, 0), node("b", 8, 16, 0)
	d := task("d", 1)
	d.PreferNodes = map[string]bool{"a": true}
	b2.Labels["x"] = "y"
	if got := placedOn(Schedule([]*Node{a2, b2}, []*Task{d})); got["d"] != "a" {
		t.Errorf("data locality ignored: %v", got)
	}
}

func TestSpreadsLoad(t *testing.T) {
	a, b := node("a", 8, 16, 0), node("b", 8, 16, 0)
	got := placedOn(Schedule([]*Node{a, b}, []*Task{task("1", 1), task("2", 1), task("3", 1), task("4", 1)}))
	count := map[string]int{}
	for _, n := range got {
		count[n]++
	}
	if count["a"] != 2 || count["b"] != 2 {
		t.Errorf("load not spread: %v", count)
	}
}
