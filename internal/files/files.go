// Package files has the file-handling rules shared by the worker and the
// CLI: safe relative paths, output globs and hashing.
package files

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// CleanRel validates a relative, slash-separated path that must stay
// inside a task's working directory, and returns it cleaned.
func CleanRel(p string) (string, error) {
	p = strings.ReplaceAll(p, "\\", "/")
	if p == "" || strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("path %q must be relative", p)
	}
	c := path.Clean(p)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("path %q leaves the working directory", p)
	}
	return c, nil
}

// Join places a validated relative path under root.
func Join(root, rel string) (string, error) {
	c, err := CleanRel(rel)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, filepath.FromSlash(c)), nil
}

// Match reports whether a slash-separated relative path matches a glob.
// Besides the usual * ? [..] (within one path segment), "**" matches any
// number of segments, e.g. "out/**/*.png" or "**/result.json".
func Match(pattern, name string) bool {
	return matchSegs(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegs(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(name); i++ {
				if matchSegs(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], name[0]); !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// ErrTooMany is returned when outputs match more files than allowed.
var ErrTooMany = errors.New("too many output files")

// CollectOutputs lists regular files under root that match any pattern. A
// pattern naming a directory selects everything below it. Symbolic links
// are never followed or returned, so a task can't make the worker upload
// files from outside its working directory.
func CollectOutputs(root string, patterns []string, maxFiles int) ([]string, error) {
	var clean []string
	for _, p := range patterns {
		p = strings.TrimSuffix(strings.TrimSpace(strings.ReplaceAll(p, "\\", "/")), "/")
		if p == "" {
			continue
		}
		if _, err := CleanRel(strings.ReplaceAll(p, "**", "x")); err != nil {
			return nil, fmt.Errorf("output pattern %q: %w", p, err)
		}
		clean = append(clean, p)
	}
	found := map[string]bool{}
	err := filepath.WalkDir(root, func(full string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are skipped
		}
		rel, _ := filepath.Rel(root, full)
		rel = filepath.ToSlash(rel)
		if rel == "." || !d.Type().IsRegular() {
			return nil // directories are walked, links and devices skipped
		}
		for _, p := range clean {
			if Match(p, rel) || Match(p+"/**", rel) {
				found[rel] = true
				if len(found) > maxFiles {
					return ErrTooMany
				}
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(found))
	for f := range found {
		out = append(out, f)
	}
	sort.Strings(out)
	return out, nil
}

// HashFile returns a file's SHA-256 (hex) and size.
func HashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
