package blobstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

func shaOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestResumableWrite(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("0123456789"), 10000)
	sha := shaOf(data)

	// First half, then the connection "drops".
	done, err := s.Write(sha, int64(len(data)), 0, bytes.NewReader(data[:40000]))
	if err != nil || done {
		t.Fatalf("first part: done=%v err=%v", done, err)
	}
	if complete, have := s.Status(sha); complete || have != 40000 {
		t.Fatalf("status = %v %d", complete, have)
	}
	// Resuming at the wrong offset is refused with the right one.
	var oe *OffsetError
	if _, err := s.Write(sha, int64(len(data)), 0, bytes.NewReader(data)); !errors.As(err, &oe) || oe.Have != 40000 {
		t.Fatalf("wrong offset: %v", err)
	}
	done, err = s.Write(sha, int64(len(data)), 40000, bytes.NewReader(data[40000:]))
	if err != nil || !done {
		t.Fatalf("second part: done=%v err=%v", done, err)
	}
	f, err := s.Open(sha)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(f)
	f.Close()
	if !bytes.Equal(got, data) {
		t.Fatal("stored data differs")
	}
	// Uploading the same content again is a no-op.
	if done, err := s.Write(sha, int64(len(data)), 0, bytes.NewReader(data)); !done || err != nil {
		t.Fatalf("re-upload: %v %v", done, err)
	}
}

func TestHashMismatchRejected(t *testing.T) {
	s, _ := Open(t.TempDir())
	data := []byte("hello")
	wrong := shaOf([]byte("something else"))
	if _, err := s.Write(wrong, 5, 0, bytes.NewReader(data)); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("err = %v", err)
	}
	if _, ok := s.Size(wrong); ok {
		t.Fatal("corrupt blob stored")
	}
	if complete, have := s.Status(wrong); complete || have != 0 {
		t.Fatal("partial file kept after mismatch")
	}
	if _, err := s.Write("../../etc/passwd", 5, 0, bytes.NewReader(data)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("path-like id accepted: %v", err)
	}
}

func TestPutAndGC(t *testing.T) {
	s, _ := Open(t.TempDir())
	keep, _, err := s.Put(bytes.NewReader([]byte("keep me")))
	if err != nil {
		t.Fatal(err)
	}
	drop, size, _ := s.Put(bytes.NewReader([]byte("drop me")))
	if size != 7 || drop != shaOf([]byte("drop me")) {
		t.Fatalf("Put = %s %d", drop, size)
	}
	// Within the grace period nothing is collected.
	if freed, _ := s.GC(map[string]bool{keep: true}, time.Hour); freed != 0 {
		t.Fatalf("freed %d within grace", freed)
	}
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(s.path(keep), old, old)
	os.Chtimes(s.path(drop), old, old)
	freed, _ := s.GC(map[string]bool{keep: true}, time.Hour)
	if freed != 7 {
		t.Fatalf("freed %d, want 7", freed)
	}
	if _, ok := s.Size(keep); !ok {
		t.Fatal("referenced blob deleted")
	}
	if _, ok := s.Size(drop); ok {
		t.Fatal("unreferenced blob kept")
	}
}

func TestUploadSession(t *testing.T) {
	s, _ := Open(t.TempDir())
	data := bytes.Repeat([]byte("xyz"), 100000)
	id, err := s.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.SessionWrite(id, 0, bytes.NewReader(data[:120000])); err != nil || n != 120000 {
		t.Fatalf("chunk 1: %d %v", n, err)
	}
	// A retried chunk at a stale offset is refused with the right offset.
	var oe *OffsetError
	if _, err := s.SessionWrite(id, 0, bytes.NewReader(data)); !errors.As(err, &oe) || oe.Have != 120000 {
		t.Fatalf("stale offset: %v", err)
	}
	if have, _ := s.SessionSize(id); have != 120000 {
		t.Fatalf("size %d", have)
	}
	if _, err := s.SessionWrite(id, 120000, bytes.NewReader(data[120000:])); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SessionFinish(id, int64(len(data))+1); err == nil {
		t.Fatal("finish accepted a wrong size")
	}
	sha, size, err := s.SessionFinish(id, int64(len(data)))
	if err != nil || sha != shaOf(data) || size != int64(len(data)) {
		t.Fatalf("finish: %s %d %v", sha, size, err)
	}
	if _, ok := s.Size(sha); !ok {
		t.Fatal("blob not stored")
	}
	if _, err := s.SessionSize(id); err == nil {
		t.Fatal("session still exists after finish")
	}
	if _, err := s.SessionWrite("../../x", 0, bytes.NewReader(nil)); err == nil {
		t.Fatal("path-like session id accepted")
	}
}
