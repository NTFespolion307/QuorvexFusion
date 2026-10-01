package hw

import (
	"strconv"
	"strings"
	"time"

	pb "github.com/NTFespolion307/QuorvexFusion/internal/clusterpb"
)

// Sampler produces Metrics snapshots. CPU and network figures are rates, so
// the Sampler remembers the previous counters; the first sample after
// creation reports zero for them.
type Sampler struct {
	gpuVendor string // "nvidia", "amd", or ""
	mounts    []string

	prevCPU  []cpuTimes
	prevRx   uint64
	prevTx   uint64
	prevTime time.Time
}

// NewSampler prepares a sampler for the hardware found by Probe.
func NewSampler(info *pb.HardwareInfo) *Sampler {
	s := &Sampler{}
	if len(info.Gpus) > 0 {
		s.gpuVendor = info.Gpus[0].Vendor
	}
	for _, d := range info.Disks {
		s.mounts = append(s.mounts, d.Mount)
	}
	return s
}

// cpuTimes holds the busy and total jiffies of one /proc/stat cpu line.
type cpuTimes struct {
	busy, total uint64
}

// parseProcStat returns the aggregate "cpu" line first, then each "cpuN".
func parseProcStat(s string) []cpuTimes {
	var out []cpuTimes
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(line, "cpu") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		var t cpuTimes
		var idle uint64
		for i, v := range f[1:] {
			n, _ := strconv.ParseUint(v, 10, 64)
			// Fields 8 and 9 (guest, guest_nice) are already counted in
			// user/nice; adding them would double count.
			if i >= 8 {
				break
			}
			t.total += n
			if i == 3 || i == 4 { // idle, iowait
				idle += n
			}
		}
		t.busy = t.total - idle
		out = append(out, t)
	}
	return out
}

func cpuPercent(prev, cur cpuTimes) float64 {
	dt := float64(cur.total - prev.total)
	// iowait can go backwards on some kernels, so guard both counters.
	if cur.total <= prev.total || cur.busy < prev.busy || dt == 0 {
		return 0
	}
	return 100 * float64(cur.busy-prev.busy) / dt
}

// parseNetDev sums received/sent bytes over physical-looking interfaces.
func parseNetDev(s string) (rx, tx uint64) {
	for _, line := range strings.Split(s, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		// Skip loopback and virtual interfaces whose traffic is also
		// counted on a physical interface.
		if name == "lo" || strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "docker") ||
			strings.HasPrefix(name, "br-") || strings.HasPrefix(name, "virbr") {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		r, _ := strconv.ParseUint(f[0], 10, 64)
		t, _ := strconv.ParseUint(f[8], 10, 64)
		rx += r
		tx += t
	}
	return rx, tx
}

func parseLoadAvg(s string) (l1, l5, l15 float64) {
	f := strings.Fields(s)
	if len(f) < 3 {
		return
	}
	return num(f[0]), num(f[1]), num(f[2])
}

// Sample takes one metrics snapshot.
func (s *Sampler) Sample() *pb.Metrics {
	now := time.Now()
	m := &pb.Metrics{TimeUnixMs: now.UnixMilli()}

	cpus := parseProcStat(readFile("/proc/stat"))
	if len(cpus) > 0 && len(s.prevCPU) == len(cpus) {
		m.CpuPercent = cpuPercent(s.prevCPU[0], cpus[0])
		for i := 1; i < len(cpus); i++ {
			m.CpuPerCore = append(m.CpuPerCore, cpuPercent(s.prevCPU[i], cpus[i]))
		}
	}
	s.prevCPU = cpus

	total, avail := parseMemInfo(readFile("/proc/meminfo"))
	m.MemTotalBytes, m.MemUsedBytes = total, total-avail
	// Inside a memory-limited container, report against the cgroup limit.
	if lim := cgroupMemoryLimit(); lim > 0 && lim < total {
		m.MemTotalBytes, m.MemUsedBytes = lim, cgroupMemoryUsage()
	}

	m.Load1, m.Load5, m.Load15 = parseLoadAvg(readFile("/proc/loadavg"))

	for _, mount := range s.mounts {
		t, free, err := statfs(mount)
		if err == nil {
			m.Disks = append(m.Disks, &pb.DiskUsage{Mount: mount, TotalBytes: t, UsedBytes: t - free})
		}
	}

	rx, tx := parseNetDev(readFile("/proc/net/dev"))
	if !s.prevTime.IsZero() && rx >= s.prevRx && tx >= s.prevTx {
		secs := now.Sub(s.prevTime).Seconds()
		if secs > 0 {
			m.NetRxBytesPerSec = uint64(float64(rx-s.prevRx) / secs)
			m.NetTxBytesPerSec = uint64(float64(tx-s.prevTx) / secs)
		}
	}
	s.prevRx, s.prevTx, s.prevTime = rx, tx, now

	switch s.gpuVendor {
	case "nvidia":
		m.Gpus = nvidiaMetrics()
	case "amd":
		m.Gpus = rocmMetrics()
	}

	if f := strings.Fields(readFile("/proc/uptime")); len(f) > 0 {
		m.UptimeSeconds = uint64(num(f[0]))
	}
	return m
}
