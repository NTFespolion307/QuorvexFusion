package store

import (
	"database/sql"
	"errors"
	"time"

	"github.com/NTFespolion307/QuorvexFusion/internal/secret"
)

// Web UI sessions. The browser holds a random session ID in a cookie; only
// its hash is stored, like API tokens.

// CreateSession stores a new session and returns the cookie value.
func (s *Store) CreateSession(ip string, ttl time.Duration, now time.Time) (string, error) {
	id := secret.RandomHex(32)
	_, err := s.db.Exec(`INSERT INTO sessions (id_hash, created_at, expires_at, ip) VALUES (?, ?, ?, ?)`,
		secret.Hash(id), unix(now), unix(now.Add(ttl)), ip)
	return id, err
}

// SessionValid reports whether a cookie value is a live session.
func (s *Store) SessionValid(id string, now time.Time) bool {
	if id == "" {
		return false
	}
	var expires int64
	err := s.db.QueryRow(`SELECT expires_at FROM sessions WHERE id_hash = ?`, secret.Hash(id)).Scan(&expires)
	if errors.Is(err, sql.ErrNoRows) || err != nil {
		return false
	}
	return now.Unix() < expires
}

func (s *Store) DeleteSession(id string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE id_hash = ?`, secret.Hash(id))
	return err
}

// DeleteAllSessions logs everyone out (used when the password changes).
func (s *Store) DeleteAllSessions() error {
	_, err := s.db.Exec(`DELETE FROM sessions`)
	return err
}

func (s *Store) PurgeExpiredSessions(now time.Time) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, unix(now))
	return err
}
