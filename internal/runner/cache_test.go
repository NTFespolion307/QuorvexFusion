package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"testing"
)

func TestCacheResumesAndVerifies(t *testing.T) {
	c, err := newCache(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("abcdefgh"), 50000) // 400 KB
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])

	var offsets []int64
	calls := 0
	fetch := func(ctx context.Context, offset int64, w io.Writer) error {
		calls++
		offsets = append(offsets, offset)
		if calls == 1 {
			// The connection drops halfway through.
			_, _ = w.Write(data[:150000])
			return errors.New("connection reset")
		}
		_, err := w.Write(data[offset:])
		return err
	}
	p, err := c.ensure(context.Background(), sha, int64(len(data)), fetch)
	if err != nil {
		t.Fatal(err)
	}
	if len(offsets) != 2 || offsets[1] != 150000 {
		t.Fatalf("fetch offsets %v, want a resume at 150000", offsets)
	}
	got, _ := os.ReadFile(p)
	if !bytes.Equal(got, data) {
		t.Fatal("cached content differs")
	}

	// Cached now: no more fetching.
	if _, err := c.ensure(context.Background(), sha, int64(len(data)), func(context.Context, int64, io.Writer) error {
		t.Fatal("fetched a cached file")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCacheRejectsCorruptData(t *testing.T) {
	c, _ := newCache(t.TempDir(), 0)
	good := []byte("the real content")
	sum := sha256.Sum256(good)
	sha := hex.EncodeToString(sum[:])
	attempts := 0
	_, err := c.ensure(context.Background(), sha, int64(len(good)), func(ctx context.Context, off int64, w io.Writer) error {
		attempts++
		if attempts == 1 {
			_, err := w.Write([]byte("corrupted bytes!")) // same length, wrong content
			return err
		}
		_, err := w.Write(good[off:])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want a fresh retry after the hash mismatch", attempts)
	}
	if _, err := c.ensure(context.Background(), sha, int64(len(good)), func(context.Context, int64, io.Writer) error {
		return errPermanent{errors.New("should not be called")}
	}); err != nil {
		t.Fatal("verified file not reused:", err)
	}
}

func TestCacheEviction(t *testing.T) {
	c, _ := newCache(t.TempDir(), 1000)
	put := func(content string) string {
		sum := sha256.Sum256([]byte(content))
		sha := hex.EncodeToString(sum[:])
		if _, err := c.ensure(context.Background(), sha, int64(len(content)), func(_ context.Context, _ int64, w io.Writer) error {
			_, err := w.Write([]byte(content))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return sha
	}
	first := put(string(bytes.Repeat([]byte("a"), 600)))
	second := put(string(bytes.Repeat([]byte("b"), 600)))
	if _, err := os.Stat(c.path(first)); !os.IsNotExist(err) {
		t.Error("oldest file not evicted when the cache exceeded its limit")
	}
	if _, err := os.Stat(c.path(second)); err != nil {
		t.Error("newest file evicted")
	}
}
