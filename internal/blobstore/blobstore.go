// Package blobstore stores files on the controller by content: each file
// is named by its SHA-256, so identical files are stored once and every
// transfer can be verified.
//
// Layout under the store directory:
//
//	ab/abcdef...    complete blobs (first two hex digits as a subdirectory)
//	tmp/<sha>.part  uploads in progress; resumed from their current size
//	tmp/new-*       uploads whose hash is computed while receiving
package blobstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var shaRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidSHA reports whether s is a lowercase hex SHA-256.
func ValidSHA(s string) bool { return shaRe.MatchString(s) }

var (
	ErrInvalid      = errors.New("invalid blob id")
	ErrHashMismatch = errors.New("uploaded data does not match its SHA-256 (corrupted transfer)")
)

// OffsetError means a resumed write started at the wrong place; Have is
// how many bytes the store holds, where the client should continue.
type OffsetError struct{ Have int64 }

func (e *OffsetError) Error() string { return fmt.Sprintf("upload must continue at offset %d", e.Have) }

type Store struct {
	dir   string
	mu    sync.Mutex
	locks map[string]*sync.Mutex // per-blob write locks
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "tmp"), 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir, locks: map[string]*sync.Mutex{}}, nil
}

func (s *Store) path(sha string) string { return filepath.Join(s.dir, sha[:2], sha) }
func (s *Store) part(sha string) string { return filepath.Join(s.dir, "tmp", sha+".part") }

func (s *Store) lock(sha string) func() {
	s.mu.Lock()
	l, ok := s.locks[sha]
	if !ok {
		l = &sync.Mutex{}
		s.locks[sha] = l
	}
	s.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// Size returns the size of a complete blob, or ok=false.
func (s *Store) Size(sha string) (size int64, ok bool) {
	if !ValidSHA(sha) {
		return 0, false
	}
	st, err := os.Stat(s.path(sha))
	if err != nil {
		return 0, false
	}
	return st.Size(), true
}

// Status reports whether a blob is complete, else how many bytes of an
// interrupted upload are present.
func (s *Store) Status(sha string) (complete bool, have int64) {
	if size, ok := s.Size(sha); ok {
		return true, size
	}
	if !ValidSHA(sha) {
		return false, 0
	}
	if st, err := os.Stat(s.part(sha)); err == nil {
		return false, st.Size()
	}
	return false, 0
}

// Write appends data for blob sha (of total size) starting at offset,
// which must equal the bytes already held (see Status). When the blob is
// complete its hash is verified and it becomes available; complete
// reports that. A mismatching hash discards the upload.
func (s *Store) Write(sha string, size, offset int64, r io.Reader) (complete bool, err error) {
	if !ValidSHA(sha) || size < 0 {
		return false, ErrInvalid
	}
	unlock := s.lock(sha)
	defer unlock()
	if _, ok := s.Size(sha); ok {
		// Already stored (another upload of the same content): discard.
		_, _ = io.Copy(io.Discard, r)
		return true, nil
	}
	partPath := s.part(sha)
	f, err := os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false, err
	}
	if st.Size() != offset {
		return false, &OffsetError{Have: st.Size()}
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return false, err
	}
	// Never accept more than the declared size.
	n, err := io.Copy(f, io.LimitReader(r, size-offset))
	if err != nil {
		return false, err
	}
	if offset+n < size {
		return false, nil // more to come
	}
	if err := f.Sync(); err != nil {
		return false, err
	}
	f.Close()
	return true, s.finish(partPath, sha)
}

// finish verifies a complete temp file and moves it into place.
func (s *Store) finish(tmp, sha string) error {
	got, err := hashFile(tmp)
	if err != nil {
		return err
	}
	if got != sha {
		os.Remove(tmp)
		return ErrHashMismatch
	}
	if err := os.MkdirAll(filepath.Dir(s.path(sha)), 0o700); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(sha))
}

// Put stores a stream whose hash is not known in advance (browser
// uploads) and returns its SHA-256 and size.
func (s *Store) Put(r io.Reader) (sha string, size int64, err error) {
	tmp, err := os.CreateTemp(filepath.Join(s.dir, "tmp"), "new-*")
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	h := sha256.New()
	size, err = io.Copy(io.MultiWriter(tmp, h), r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, err
	}
	sha = hex.EncodeToString(h.Sum(nil))
	unlock := s.lock(sha)
	defer unlock()
	if _, ok := s.Size(sha); ok {
		return sha, size, nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path(sha)), 0o700); err != nil {
		return "", 0, err
	}
	return sha, size, os.Rename(tmp.Name(), s.path(sha))
}

// Open opens a complete blob for reading.
func (s *Store) Open(sha string) (*os.File, error) {
	if !ValidSHA(sha) {
		return nil, ErrInvalid
	}
	return os.Open(s.path(sha))
}

// GC deletes blobs and stale partial uploads that are not referenced and
// were not touched within grace (so files uploaded for a job that is
// being submitted right now survive). It returns the bytes freed.
func (s *Store) GC(referenced map[string]bool, grace time.Duration) (freed int64, err error) {
	cutoff := time.Now().Add(-grace)
	err = filepath.WalkDir(s.dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil || info.ModTime().After(cutoff) {
			return nil
		}
		name := d.Name()
		sha := strings.TrimSuffix(name, ".part")
		if referenced[sha] {
			return nil
		}
		if ValidSHA(sha) || strings.HasPrefix(name, "new-") {
			if os.Remove(p) == nil {
				freed += info.Size()
			}
		}
		return nil
	})
	return freed, err
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
