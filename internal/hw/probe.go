// Package hw discovers a worker's hardware and samples its live metrics.
//
// Everything is read from /proc, /sys, and vendor tools (nvidia-smi,
// rocm-smi, docker). Parsers take plain strings so they can be unit-tested
// with captured output. On non-Linux systems most probes just come back
// empty, which keeps the package compilable for development.
package hw

import (
	"bufio"
	"context"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	pb "github.com/NTFespolion307/QuorvexFusion/internal/clusterpb"
)

// procRoot lets tests point the probes at a fake filesystem.
var procRoot = ""

func readFile(path string) string {
	b, err := os.ReadFile(procRoot + path)
	if err != nil {
		return ""
	}
	return string(b)
}

// run executes a command with a timeout and returns its stdout.
func run(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

func hasCommand(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// Probe collects static hardware information. It may take a few seconds
// (vendor tools and `docker info` are queried) so call it once at startup.
func Probe() *pb.HardwareInfo {
	info := &pb.HardwareInfo{
		Os:     osName(),
		Kernel: strings.TrimSpace(readFile("/proc/sys/kernel/osrelease")),
		Arch:   runtime.GOARCH,
	}
	info.Hostname, _ = os.Hostname()

	cpu := parseCPUInfo(readFile("/proc/cpuinfo"))
	info.CpuModel = cpu.model
	if info.CpuModel == "" {
		info.CpuModel = lscpuModel()
	}
	info.LogicalCores = int32(runtime.NumCPU()) // respects CPU affinity/cpusets
	info.PhysicalCores = int32(physicalCores())
	if info.PhysicalCores == 0 || info.PhysicalCores > info.LogicalCores {
		info.PhysicalCores = info.LogicalCores
	}
	info.CpuLimit = float64(info.LogicalCores)
	if q := cgroupCPUQuota(); q > 0 && q < info.CpuLimit {
		info.CpuLimit = q
	}

	memTotal, _ := parseMemInfo(readFile("/proc/meminfo"))
	info.MemoryBytes = memTotal
	if lim := cgroupMemoryLimit(); lim > 0 && (memTotal == 0 || lim < memTotal) {
		info.MemoryBytes = lim
	}

	info.Disks = probeDisks()
	info.Gpus = probeGPUs()
	info.InContainer = inContainer()
	info.Docker, info.NvidiaDocker = probeDocker(len(info.Gpus) > 0)
	info.Systemd = HasSystemd()
	info.Ips = localIPs()
	info.BootTimeUnix = bootTime(readFile("/proc/stat"))
	return info
}

func osName() string {
	for _, line := range strings.Split(readFile("/etc/os-release"), "\n") {
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			return strings.Trim(v, `"'`)
		}
	}
	return runtime.GOOS
}

type cpuInfo struct {
	model string
}

func parseCPUInfo(s string) cpuInfo {
	var c cpuInfo
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		// x86 uses "model name"; some ARM kernels use "Model" or "Hardware".
		if c.model == "" && (k == "model name" || k == "Model" || k == "Hardware") && v != "" {
			c.model = v
		}
	}
	return c
}

func lscpuModel() string {
	out, err := run(3*time.Second, "lscpu")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "Model name:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// physicalCores counts unique (package, core) pairs from sysfs topology.
func physicalCores() int {
	seen := map[string]bool{}
	entries, err := os.ReadDir(procRoot + "/sys/devices/system/cpu")
	if err != nil {
		return 0
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "cpu") {
			continue
		}
		if _, err := strconv.Atoi(name[3:]); err != nil {
			continue
		}
		base := "/sys/devices/system/cpu/" + name + "/topology/"
		pkg := strings.TrimSpace(readFile(base + "physical_package_id"))
		core := strings.TrimSpace(readFile(base + "core_id"))
		if core == "" {
			continue
		}
		seen[pkg+"/"+core] = true
	}
	return len(seen)
}

// cgroupCPUQuota returns the CPU limit in cores imposed on this process's
// cgroup (as in a container with --cpus), or 0 if unlimited.
func cgroupCPUQuota() float64 {
	// cgroup v2: "max 100000" or "<quota> <period>"
	if f := strings.Fields(readFile("/sys/fs/cgroup/cpu.max")); len(f) == 2 && f[0] != "max" {
		q, _ := strconv.ParseFloat(f[0], 64)
		p, _ := strconv.ParseFloat(f[1], 64)
		if q > 0 && p > 0 {
			return q / p
		}
	}
	// cgroup v1
	q, _ := strconv.ParseFloat(strings.TrimSpace(readFile("/sys/fs/cgroup/cpu/cpu.cfs_quota_us")), 64)
	p, _ := strconv.ParseFloat(strings.TrimSpace(readFile("/sys/fs/cgroup/cpu/cpu.cfs_period_us")), 64)
	if q > 0 && p > 0 {
		return q / p
	}
	return 0
}

// cgroupMemoryLimit returns the memory limit of this process's cgroup in
// bytes, or 0 if unlimited.
func cgroupMemoryLimit() uint64 {
	if v := strings.TrimSpace(readFile("/sys/fs/cgroup/memory.max")); v != "" && v != "max" {
		n, _ := strconv.ParseUint(v, 10, 64)
		return n
	}
	n, _ := strconv.ParseUint(strings.TrimSpace(readFile("/sys/fs/cgroup/memory/memory.limit_in_bytes")), 10, 64)
	if n >= 1<<60 { // v1 reports "unlimited" as a huge number
		return 0
	}
	return n
}

// cgroupMemoryUsage returns current memory usage of this cgroup, or 0.
func cgroupMemoryUsage() uint64 {
	if v := strings.TrimSpace(readFile("/sys/fs/cgroup/memory.current")); v != "" {
		n, _ := strconv.ParseUint(v, 10, 64)
		return n
	}
	n, _ := strconv.ParseUint(strings.TrimSpace(readFile("/sys/fs/cgroup/memory/memory.usage_in_bytes")), 10, 64)
	return n
}

// parseMemInfo returns MemTotal and MemAvailable in bytes.
func parseMemInfo(s string) (total, available uint64) {
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		kb, _ := strconv.ParseUint(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			total = kb * 1024
		case "MemAvailable:":
			available = kb * 1024
		}
	}
	return total, available
}

func inContainer() bool {
	if _, err := os.Stat(procRoot + "/.dockerenv"); err == nil {
		return true
	}
	if _, err := os.Stat(procRoot + "/run/.containerenv"); err == nil {
		return true
	}
	if os.Getenv("container") != "" {
		return true
	}
	cg := readFile("/proc/1/cgroup")
	for _, marker := range []string{"docker", "kubepods", "containerd", "lxc"} {
		if strings.Contains(cg, marker) {
			return true
		}
	}
	return false
}

// probeDocker reports whether Docker is usable, and whether GPU containers
// can run (NVIDIA container toolkit installed). wantGPU skips the toolkit
// check on machines without NVIDIA GPUs.
func probeDocker(wantGPU bool) (docker, nvidia bool) {
	if !hasCommand("docker") {
		return false, false
	}
	out, err := run(8*time.Second, "docker", "info", "--format", "{{json .Runtimes}}")
	if err != nil {
		return false, false
	}
	if !wantGPU {
		return true, false
	}
	nvidia = strings.Contains(out, "nvidia") ||
		hasCommand("nvidia-container-runtime-hook") || hasCommand("nvidia-ctk")
	return true, nvidia
}

// HasSystemd reports whether systemd manages this machine and systemd-run exists.
func HasSystemd() bool {
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return false
	}
	return hasCommand("systemd-run")
}

func localIPs() []string {
	var out []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, iface := range ifaces {
		// Skip by flag too: some systems (e.g. WSL) put non-127.x
		// addresses on the loopback interface.
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, ipn.IP.String())
		}
	}
	return out
}

func bootTime(procStat string) int64 {
	sc := bufio.NewScanner(strings.NewReader(procStat))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "btime "); ok {
			n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			return n
		}
	}
	return 0
}
