package controller

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Features used by the web UI: cluster-wide usage history, draining,
// password changes and rate-limited live metric notifications.

// poolHistoryLen is one hour at the 5 second sampling interval.
const poolHistoryLen = 720

// PoolSample is one point of cluster-wide usage for the dashboard charts.
type PoolSample struct {
	T            int64                     `json:"t"`             // unix ms
	CPUPercent   float64                   `json:"cpu"`           // measured CPU use across online nodes
	MemPercent   float64                   `json:"mem"`           // measured memory use
	GPUPercent   float64                   `json:"gpu"`           // mean GPU utilisation (0 without GPUs)
	ReservedCPUs float64                   `json:"reserved_cpus"` // CPUs reserved by tasks
	Running      int                       `json:"running"`       // active task attempts
	ByLocation   map[string]LocationSample `json:"loc"`
}

type LocationSample struct {
	CPUPercent float64 `json:"cpu"`
	GPUPercent float64 `json:"gpu"`
}

type poolHistory struct {
	mu      sync.Mutex
	samples []PoolSample
}

func (h *poolHistory) add(s PoolSample) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.samples) >= poolHistoryLen {
		copy(h.samples, h.samples[1:])
		h.samples = h.samples[:len(h.samples)-1]
	}
	h.samples = append(h.samples, s)
}

// PoolHistory returns the recent cluster-wide samples, oldest first.
func (c *Controller) PoolHistory() []PoolSample {
	c.history.mu.Lock()
	defer c.history.mu.Unlock()
	return append([]PoolSample(nil), c.history.samples...)
}

// samplePool aggregates the latest metrics of every online node.
func (c *Controller) samplePool() PoolSample {
	type acc struct{ cpuWeighted, cores, gpuSum, gpus float64 }
	all := acc{}
	var memUsed, memTotal float64
	byLoc := map[string]*acc{}

	nodes, _ := c.store.ListNodes()
	location := map[string]string{}
	for _, n := range nodes {
		location[n.ID] = n.Location
	}
	for _, s := range c.hub.All() {
		snap := s.Snapshot()
		m, hw := snap.Metrics, snap.Hello.Hardware
		if m == nil || hw == nil {
			continue
		}
		loc := location[s.NodeID]
		if loc == "" {
			loc = "default"
		}
		l := byLoc[loc]
		if l == nil {
			l = &acc{}
			byLoc[loc] = l
		}
		cores := float64(hw.LogicalCores)
		for _, a := range []*acc{&all, l} {
			a.cpuWeighted += m.CpuPercent * cores
			a.cores += cores
			for _, g := range m.Gpus {
				a.gpuSum += g.UtilizationPercent
				a.gpus++
			}
		}
		memUsed += float64(m.MemUsedBytes)
		memTotal += float64(m.MemTotalBytes)
	}
	pct := func(num, den float64) float64 {
		if den == 0 {
			return 0
		}
		return num / den
	}
	out := PoolSample{
		T:          time.Now().UnixMilli(),
		CPUPercent: pct(all.cpuWeighted, all.cores),
		MemPercent: 100 * pct(memUsed, memTotal),
		GPUPercent: pct(all.gpuSum, all.gpus),
		ByLocation: map[string]LocationSample{},
	}
	for loc, a := range byLoc {
		out.ByLocation[loc] = LocationSample{CPUPercent: pct(a.cpuWeighted, a.cores), GPUPercent: pct(a.gpuSum, a.gpus)}
	}
	c.tm.mu.Lock()
	out.Running = len(c.tm.reservations)
	for _, r := range c.tm.reservations {
		out.ReservedCPUs += r.cpus
	}
	c.tm.mu.Unlock()
	return out
}

func (c *Controller) poolSampler(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.history.add(c.samplePool())
			_ = c.store.PurgeExpiredSessions(time.Now())
		}
	}
}

// publishThrottled notifies UI subscribers about a topic at most once
// every two seconds, however often it changes (metrics arrive from every
// node every few seconds; transfer progress even more often).
func (c *Controller) publishThrottled(topic string) {
	c.throttleMu.Lock()
	now := time.Now()
	if now.Sub(c.lastEvent[topic]) < 2*time.Second {
		c.throttleMu.Unlock()
		return
	}
	c.lastEvent[topic] = now
	c.throttleMu.Unlock()
	c.events.publish(topic)
}

// SetNodeDraining stops (or resumes) scheduling new tasks on a node.
// Running tasks finish normally.
func (c *Controller) SetNodeDraining(id string, draining bool) error {
	if err := c.store.SetNodeDraining(id, draining); err != nil {
		return err
	}
	c.log.Info("node drain changed", "node", id, "draining", draining)
	c.events.publish("nodes")
	c.kickScheduler()
	return nil
}

// ChangePassword replaces the admin password and logs out all browser
// sessions. API tokens keep working.
func (c *Controller) ChangePassword(current, next string) error {
	if !c.CheckAdminPassword(current) {
		return errors.New("current password is wrong")
	}
	if len(next) < 8 {
		return errors.New("new password must be at least 8 characters")
	}
	if err := setAdminPassword(c.store, next); err != nil {
		return err
	}
	return c.store.DeleteAllSessions()
}

// Sessions for the web UI.

const sessionTTL = 7 * 24 * time.Hour

func (c *Controller) CreateSession(ip string) (string, error) {
	return c.store.CreateSession(ip, sessionTTL, time.Now())
}

func (c *Controller) SessionValid(id string) bool { return c.store.SessionValid(id, time.Now()) }

func (c *Controller) DeleteSession(id string) error { return c.store.DeleteSession(id) }

// SessionTTL is how long a browser login lasts.
func SessionTTL() time.Duration { return sessionTTL }

// runningTasks counts active attempts per node.
func (c *Controller) runningTasks() map[string]int {
	c.tm.mu.Lock()
	defer c.tm.mu.Unlock()
	out := map[string]int{}
	for _, r := range c.tm.reservations {
		out[r.nodeID]++
	}
	return out
}
