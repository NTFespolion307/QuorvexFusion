package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// cache keeps downloaded input files on the worker, named by SHA-256, so
// repeated jobs with the same inputs don't download them again.
//
// Files are made read-only (0444) because tasks get hard links to them:
// a task can delete or replace its link, but cannot modify the shared copy.
type cache struct {
	dir string
	max int64 // bytes; 0 = unlimited

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func newCache(dir string, max int64) (*cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &cache{dir: dir, max: max, locks: map[string]*sync.Mutex{}}, nil
}

func (c *cache) path(sha string) string { return filepath.Join(c.dir, sha[:2], sha) }

func (c *cache) lock(sha string) func() {
	c.mu.Lock()
	l, ok := c.locks[sha]
	if !ok {
		l = &sync.Mutex{}
		c.locks[sha] = l
	}
	c.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// fetchFunc writes a file's bytes from offset onward to w.
type fetchFunc func(ctx context.Context, offset int64, w io.Writer) error

// errPermanent marks fetch errors that retrying will not fix.
type errPermanent struct{ error }

func (e errPermanent) Unwrap() error { return e.error }

// ensure returns the path of a cached, verified copy of sha, downloading
// it with fetch if needed. Interrupted downloads resume from the partial
// file; transient errors are retried until ctx ends.
func (c *cache) ensure(ctx context.Context, sha string, size int64, fetch fetchFunc) (string, error) {
	if len(sha) != 64 {
		return "", fmt.Errorf("invalid sha256 %q", sha)
	}
	unlock := c.lock(sha)
	defer unlock()
	final := c.path(sha)
	if st, err := os.Stat(final); err == nil && st.Size() == size {
		now := time.Now()
		_ = os.Chtimes(final, now, now) // mark as recently used
		return final, nil
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return "", err
	}
	part := final + ".part"
	for attempt := 1; ; attempt++ {
		err := c.download(ctx, part, size, fetch)
		if err == nil {
			got, herr := hashFile(part)
			if herr != nil {
				return "", herr
			}
			if got == sha {
				if err := os.Chmod(part, 0o444); err != nil {
					return "", err
				}
				if err := os.Rename(part, final); err != nil {
					return "", err
				}
				c.evict()
				return final, nil
			}
			os.Remove(part) // corrupted: start over
			err = fmt.Errorf("downloaded data does not match its sha256")
		}
		var perm errPermanent
		if errors.As(err, &perm) || ctx.Err() != nil || attempt >= 20 {
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", context.Cause(ctx)
		case <-time.After(time.Duration(min(attempt, 10)) * time.Second):
		}
	}
}

// download appends to part from its current size until size is reached.
func (c *cache) download(ctx context.Context, part string, size int64, fetch fetchFunc) error {
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	offset := st.Size()
	if offset > size {
		f.Close()
		os.Remove(part)
		return errors.New("partial file larger than expected")
	}
	if offset == size {
		return nil
	}
	return fetch(ctx, offset, f)
}

// evict removes least recently used files until the cache fits its limit.
func (c *cache) evict() {
	if c.max <= 0 {
		return
	}
	type entry struct {
		path string
		size int64
		used time.Time
	}
	var all []entry
	var total int64
	_ = filepath.WalkDir(c.dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) == ".part" {
			return nil
		}
		if info, err := d.Info(); err == nil {
			all = append(all, entry{p, info.Size(), info.ModTime()})
			total += info.Size()
		}
		return nil
	})
	sort.Slice(all, func(i, j int) bool { return all[i].used.Before(all[j].used) })
	for _, e := range all {
		if total <= c.max {
			return
		}
		// Running tasks keep their hard links; only the cached copy goes.
		if os.Remove(e.path) == nil {
			total -= e.size
		}
	}
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
