package blobstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/NTFespolion307/QuorvexFusion/internal/secret"
)

// Upload sessions: resumable uploads whose content hash is not known in
// advance (browsers can't hash a large file without reading it all first).
// The client creates a session, sends the file in chunks at increasing
// offsets (asking for the current offset after any interruption), then
// finishes the session, which hashes the data and stores it as a blob.
// Unfinished sessions are deleted by GC after the grace period.

var sessionRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

var ErrNoSession = errors.New("no such upload session (expired or finished)")

func (s *Store) sessionPath(id string) string { return filepath.Join(s.dir, "tmp", "up-"+id) }

// NewSession starts an upload session and returns its ID.
func (s *Store) NewSession() (string, error) {
	id := secret.RandomHex(16)
	f, err := os.OpenFile(s.sessionPath(id), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	return id, f.Close()
}

// SessionSize reports how many bytes a session holds.
func (s *Store) SessionSize(id string) (int64, error) {
	if !sessionRe.MatchString(id) {
		return 0, ErrNoSession
	}
	st, err := os.Stat(s.sessionPath(id))
	if err != nil {
		return 0, ErrNoSession
	}
	return st.Size(), nil
}

// SessionWrite appends a chunk at offset, which must equal the session's
// current size (otherwise an *OffsetError says where to continue).
func (s *Store) SessionWrite(id string, offset int64, r io.Reader) (int64, error) {
	if !sessionRe.MatchString(id) {
		return 0, ErrNoSession
	}
	unlock := s.lock("up-" + id)
	defer unlock()
	f, err := os.OpenFile(s.sessionPath(id), os.O_WRONLY, 0)
	if err != nil {
		return 0, ErrNoSession
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if st.Size() != offset {
		return 0, &OffsetError{Have: st.Size()}
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}
	n, err := io.Copy(f, r)
	return offset + n, err
}

// SessionFinish hashes a completed session, stores it as a blob and
// returns its SHA-256 and size. If size is given (>= 0) it must match.
func (s *Store) SessionFinish(id string, size int64) (string, int64, error) {
	if !sessionRe.MatchString(id) {
		return "", 0, ErrNoSession
	}
	unlock := s.lock("up-" + id)
	defer unlock()
	path := s.sessionPath(id)
	f, err := os.Open(path)
	if err != nil {
		return "", 0, ErrNoSession
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	f.Close()
	if err != nil {
		return "", 0, err
	}
	if size >= 0 && n != size {
		return "", 0, &OffsetError{Have: n}
	}
	sha := hex.EncodeToString(h.Sum(nil))
	unlockBlob := s.lock(sha)
	defer unlockBlob()
	if _, ok := s.Size(sha); ok {
		os.Remove(path) // already stored
		return sha, n, nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path(sha)), 0o700); err != nil {
		return "", 0, err
	}
	return sha, n, os.Rename(path, s.path(sha))
}
