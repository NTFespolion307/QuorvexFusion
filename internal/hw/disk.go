package hw

import (
	"strings"

	pb "github.com/NTFespolion307/QuorvexFusion/internal/clusterpb"
)

// realFS lists filesystem types that represent actual storage.
var realFS = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "zfs": true,
	"f2fs": true, "ntfs": true, "ntfs3": true, "bcachefs": true,
}

// skipPrefixes are mount points that are never interesting to report.
// /etc catches files Docker bind-mounts into containers (hosts, resolv.conf).
var skipPrefixes = []string{"/proc", "/sys", "/dev", "/run", "/snap", "/boot", "/etc", "/var/lib/docker", "/var/lib/containers"}

type mount struct {
	device, path, fstype string
}

// parseMounts reads /proc/mounts and returns the real storage mounts, one
// per device. Inside containers the root is usually "overlay", which we
// keep only for "/".
func parseMounts(s string) []mount {
	var out []mount
	seenDev := map[string]bool{}
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		m := mount{device: f[0], path: unescapeMount(f[1]), fstype: f[2]}
		if !(realFS[m.fstype] || (m.fstype == "overlay" && m.path == "/")) {
			continue
		}
		skip := false
		for _, p := range skipPrefixes {
			if m.path == p || strings.HasPrefix(m.path, p+"/") {
				skip = true
				break
			}
		}
		// Bind mounts and btrfs subvolumes repeat the same device; keep the
		// first (shortest, as /proc/mounts lists parents first).
		if skip || seenDev[m.device] {
			continue
		}
		seenDev[m.device] = true
		out = append(out, m)
	}
	return out
}

// unescapeMount decodes the octal escapes /proc/mounts uses (e.g. \040 for space).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			var v byte
			ok := true
			for _, c := range s[i+1 : i+4] {
				if c < '0' || c > '7' {
					ok = false
					break
				}
				v = v*8 + byte(c-'0')
			}
			if ok {
				b.WriteByte(v)
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func probeDisks() []*pb.Disk {
	var out []*pb.Disk
	for _, m := range parseMounts(readFile("/proc/mounts")) {
		total, free, err := statfs(m.path)
		if err != nil || total == 0 {
			continue
		}
		out = append(out, &pb.Disk{Mount: m.path, Device: m.device, Fstype: m.fstype, TotalBytes: total, FreeBytes: free})
	}
	return out
}
