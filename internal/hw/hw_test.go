package hw

import (
	"math"
	"testing"
)

func TestParseCPUInfo(t *testing.T) {
	x86 := "processor\t: 0\nvendor_id\t: GenuineIntel\nmodel name\t: Intel(R) Xeon(R) CPU E5-2680 v4 @ 2.40GHz\n"
	if got := parseCPUInfo(x86).model; got != "Intel(R) Xeon(R) CPU E5-2680 v4 @ 2.40GHz" {
		t.Errorf("x86 model = %q", got)
	}
	pi := "processor\t: 0\nBogoMIPS\t: 108.00\n\nHardware\t: BCM2835\nModel\t\t: Raspberry Pi 4 Model B Rev 1.4\n"
	if got := parseCPUInfo(pi).model; got != "BCM2835" && got != "Raspberry Pi 4 Model B Rev 1.4" {
		t.Errorf("arm model = %q", got)
	}
}

func TestParseMemInfo(t *testing.T) {
	total, avail := parseMemInfo("MemTotal:       16303932 kB\nMemFree:  1000 kB\nMemAvailable:   12000000 kB\n")
	if total != 16303932*1024 || avail != 12000000*1024 {
		t.Errorf("total=%d avail=%d", total, avail)
	}
}

func TestParseMounts(t *testing.T) {
	in := `sysfs /sys sysfs rw 0 0
proc /proc proc rw 0 0
/dev/nvme0n1p2 / ext4 rw,relatime 0 0
/dev/nvme0n1p1 /boot/efi vfat rw 0 0
/dev/sda1 /mnt/my\040data xfs rw 0 0
/dev/nvme0n1p2 /var/lib/docker ext4 rw 0 0
/dev/sdb1 /data btrfs rw 0 0
/dev/sdb1 /data/sub btrfs rw 0 0
tmpfs /run tmpfs rw 0 0
`
	got := parseMounts(in)
	want := []string{"/", "/mnt/my data", "/data"}
	if len(got) != len(want) {
		t.Fatalf("got %d mounts: %+v", len(got), got)
	}
	for i, m := range got {
		if m.path != want[i] {
			t.Errorf("mount %d = %q, want %q", i, m.path, want[i])
		}
	}

	container := "overlay / overlay rw,lowerdir=... 0 0\n/dev/sda1 /etc/hosts ext4 rw 0 0\n"
	if got := parseMounts(container); len(got) != 1 || got[0].path != "/" {
		t.Errorf("container mounts = %+v", got)
	}
}

func TestCPUPercent(t *testing.T) {
	a := parseProcStat("cpu  100 0 100 800 0 0 0 0 0 0\ncpu0 50 0 50 400 0 0 0 0 0 0\ncpu1 50 0 50 400 0 0 0 0 0 0\nintr 1 2\n")
	b := parseProcStat("cpu  200 0 200 1000 0 0 0 0 0 0\ncpu0 150 0 150 400 0 0 0 0 0 0\ncpu1 50 0 50 600 0 0 0 0 0 0\n")
	if len(a) != 3 || len(b) != 3 {
		t.Fatalf("parsed %d/%d cpu lines", len(a), len(b))
	}
	// Total: busy +200 of +400 jiffies = 50%. cpu0 fully busy, cpu1 idle.
	for i, want := range []float64{50, 100, 0} {
		if got := cpuPercent(a[i], b[i]); math.Abs(got-want) > 0.01 {
			t.Errorf("cpu line %d: %.2f%%, want %.0f%%", i, got, want)
		}
	}
	if got := cpuPercent(b[0], a[0]); got != 0 {
		t.Errorf("counter going backwards gave %.2f", got)
	}
}

func TestParseNetDev(t *testing.T) {
	in := `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 1000 10 0 0 0 0 0 0 1000 10 0 0 0 0 0 0
  eth0: 5000 50 0 0 0 0 0 0 7000 70 0 0 0 0 0 0
docker0: 9999 1 0 0 0 0 0 0 9999 1 0 0 0 0 0 0
vethab12: 9999 1 0 0 0 0 0 0 9999 1 0 0 0 0 0 0
 wlan0: 100 1 0 0 0 0 0 0 200 2 0 0 0 0 0 0
`
	rx, tx := parseNetDev(in)
	if rx != 5100 || tx != 7200 {
		t.Errorf("rx=%d tx=%d", rx, tx)
	}
}

func TestParseNvidia(t *testing.T) {
	gpus := parseNvidiaGPUs("0, GPU-aaaa, NVIDIA GeForce RTX 4090, 24564, 550.54.14\n1, GPU-bbbb, NVIDIA GeForce RTX 3090, 24576, 550.54.14\n")
	if len(gpus) != 2 || gpus[0].Name != "NVIDIA GeForce RTX 4090" || gpus[1].Uuid != "GPU-bbbb" ||
		gpus[0].MemoryBytes != 24564*mib || gpus[0].Vendor != "nvidia" {
		t.Errorf("gpus = %+v", gpus)
	}
	ms := parseNvidiaMetrics("0, 87, 20000, 24564, 71, 312.45\n1, 0, 1, 24576, 35, [N/A]\n")
	if len(ms) != 2 || ms[0].UtilizationPercent != 87 || ms[0].TemperatureC != 71 || ms[0].PowerWatts != 312.45 || ms[1].PowerWatts != 0 {
		t.Errorf("metrics = %+v", ms)
	}
}

func TestParseRocm(t *testing.T) {
	in := `{"card0": {"Card series": "Radeon RX 7900 XTX", "VRAM Total Memory (B)": "25753026560", "VRAM Total Used Memory (B)": "1000", "GPU use (%)": "12", "Temperature (Sensor edge) (C)": "45.0", "Average Graphics Package Power (W)": "60.0"}}`
	gpus := parseRocmGPUs(in)
	if len(gpus) != 1 || gpus[0].Name != "Radeon RX 7900 XTX" || gpus[0].MemoryBytes != 25753026560 {
		t.Errorf("gpus = %+v", gpus)
	}
}

func TestUnescapeMount(t *testing.T) {
	if got := unescapeMount(`/a\040b\134c`); got != `/a b\c` {
		t.Errorf("got %q", got)
	}
}
