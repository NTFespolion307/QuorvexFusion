package hw

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	pb "github.com/NTFespolion307/QuorvexFusion/internal/clusterpb"
)

const mib = 1024 * 1024

// probeGPUs lists NVIDIA GPUs via nvidia-smi, or AMD GPUs via rocm-smi.
func probeGPUs() []*pb.GPU {
	if hasCommand("nvidia-smi") {
		out, err := run(10*time.Second, "nvidia-smi",
			"--query-gpu=index,uuid,name,memory.total,driver_version", "--format=csv,noheader,nounits")
		if err == nil {
			return parseNvidiaGPUs(out)
		}
	}
	if hasCommand("rocm-smi") {
		out, err := run(10*time.Second, "rocm-smi", "--showproductname", "--showmeminfo", "vram", "--json")
		if err == nil {
			return parseRocmGPUs(out)
		}
	}
	return nil
}

func splitCSV(line string) []string {
	f := strings.Split(line, ",")
	for i := range f {
		f[i] = strings.TrimSpace(f[i])
	}
	return f
}

// num parses a number from vendor tools, treating "[N/A]" etc. as 0.
func num(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v
}

func parseNvidiaGPUs(out string) []*pb.GPU {
	var gpus []*pb.GPU
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := splitCSV(line)
		if len(f) < 5 {
			continue
		}
		idx, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		gpus = append(gpus, &pb.GPU{
			Index: int32(idx), Vendor: "nvidia", Uuid: f[1], Name: f[2],
			MemoryBytes: uint64(num(f[3])) * mib, Driver: f[4],
		})
	}
	return gpus
}

func nvidiaMetrics() []*pb.GPUMetrics {
	out, err := run(5*time.Second, "nvidia-smi",
		"--query-gpu=index,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw",
		"--format=csv,noheader,nounits")
	if err != nil {
		return nil
	}
	return parseNvidiaMetrics(out)
}

func parseNvidiaMetrics(out string) []*pb.GPUMetrics {
	var ms []*pb.GPUMetrics
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := splitCSV(line)
		if len(f) < 6 {
			continue
		}
		idx, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		ms = append(ms, &pb.GPUMetrics{
			Index:              int32(idx),
			UtilizationPercent: num(f[1]),
			MemoryUsedBytes:    uint64(num(f[2])) * mib,
			MemoryTotalBytes:   uint64(num(f[3])) * mib,
			TemperatureC:       num(f[4]),
			PowerWatts:         num(f[5]),
		})
	}
	return ms
}

// rocm-smi --json output is a map of "card0", "card1", ... to string fields
// whose names vary between ROCm versions, so fields are matched loosely.
func parseRocmCards(out string) (map[int]map[string]string, []int) {
	var raw map[string]map[string]string
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, nil
	}
	cards := map[int]map[string]string{}
	var order []int
	for k, v := range raw {
		idx, err := strconv.Atoi(strings.TrimPrefix(k, "card"))
		if err != nil || !strings.HasPrefix(k, "card") {
			continue
		}
		cards[idx] = v
		order = append(order, idx)
	}
	sort.Ints(order)
	return cards, order
}

// field returns the first value whose key contains all the given words.
func field(m map[string]string, words ...string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic choice when several keys match
	for _, k := range keys {
		lk := strings.ToLower(k)
		match := true
		for _, w := range words {
			if !strings.Contains(lk, w) {
				match = false
				break
			}
		}
		if match {
			return m[k]
		}
	}
	return ""
}

func parseRocmGPUs(out string) []*pb.GPU {
	cards, order := parseRocmCards(out)
	var gpus []*pb.GPU
	for _, idx := range order {
		c := cards[idx]
		name := field(c, "card series")
		if name == "" {
			name = field(c, "card model")
		}
		gpus = append(gpus, &pb.GPU{
			Index: int32(idx), Vendor: "amd", Name: name,
			MemoryBytes: uint64(num(field(c, "vram total memory"))),
		})
	}
	return gpus
}

func rocmMetrics() []*pb.GPUMetrics {
	out, err := run(5*time.Second, "rocm-smi", "--showuse", "--showmeminfo", "vram", "--showtemp", "--showpower", "--json")
	if err != nil {
		return nil
	}
	cards, order := parseRocmCards(out)
	var ms []*pb.GPUMetrics
	for _, idx := range order {
		c := cards[idx]
		ms = append(ms, &pb.GPUMetrics{
			Index:              int32(idx),
			UtilizationPercent: num(field(c, "gpu use")),
			MemoryUsedBytes:    uint64(num(field(c, "vram total used"))),
			MemoryTotalBytes:   uint64(num(field(c, "vram total memory"))),
			TemperatureC:       num(field(c, "temperature")),
			PowerWatts:         num(field(c, "power")),
		})
	}
	return ms
}
