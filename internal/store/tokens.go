package store

import (
	"database/sql"
	"errors"
	"time"

	"github.com/NTFespolion307/QuorvexFusion/internal/secret"
)

// JoinToken lets new machines join the cluster.
type JoinToken struct {
	ID          string     `json:"id"`
	SecretHash  string     `json:"-"`
	Description string     `json:"description"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	MaxUses     *int       `json:"max_uses,omitempty"`
	Uses        int        `json:"uses"`
	AutoApprove bool       `json:"auto_approve"`
	Location    string     `json:"location,omitempty"`
	Ephemeral   bool       `json:"ephemeral"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

// Usable reports why a token can't be used right now, or nil.
func (t *JoinToken) Usable(now time.Time) error {
	switch {
	case t.RevokedAt != nil:
		return errors.New("join token has been revoked")
	case t.ExpiresAt != nil && now.After(*t.ExpiresAt):
		return errors.New("join token has expired")
	case t.MaxUses != nil && t.Uses >= *t.MaxUses:
		return errors.New("join token has no uses left")
	}
	return nil
}

func (s *Store) CreateJoinToken(t *JoinToken) error {
	_, err := s.db.Exec(`INSERT INTO join_tokens
		(id, secret_hash, description, created_at, expires_at, max_uses, uses, auto_approve, location, ephemeral)
		VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, ?)`,
		t.ID, t.SecretHash, t.Description, unix(t.CreatedAt), nullTime(t.ExpiresAt), nullInt(t.MaxUses),
		boolInt(t.AutoApprove), t.Location, boolInt(t.Ephemeral))
	return err
}

const joinTokenCols = `id, secret_hash, description, created_at, expires_at, max_uses, uses,
	auto_approve, location, ephemeral, revoked_at`

func scanJoinToken(row interface{ Scan(...any) error }) (*JoinToken, error) {
	var t JoinToken
	var created int64
	var expires, maxUses, revoked sql.NullInt64
	err := row.Scan(&t.ID, &t.SecretHash, &t.Description, &created, &expires, &maxUses, &t.Uses,
		&t.AutoApprove, &t.Location, &t.Ephemeral, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.CreatedAt = time.Unix(created, 0)
	t.ExpiresAt = fromNullTime(expires)
	t.MaxUses = fromNullInt(maxUses)
	t.RevokedAt = fromNullTime(revoked)
	return &t, nil
}

func (s *Store) GetJoinToken(id string) (*JoinToken, error) {
	return scanJoinToken(s.db.QueryRow(`SELECT `+joinTokenCols+` FROM join_tokens WHERE id = ?`, id))
}

func (s *Store) ListJoinTokens() ([]*JoinToken, error) {
	rows, err := s.db.Query(`SELECT ` + joinTokenCols + ` FROM join_tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*JoinToken
	for rows.Next() {
		t, err := scanJoinToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) RevokeJoinToken(id string, now time.Time) error {
	res, err := s.db.Exec(`UPDATE join_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, unix(now), id)
	return expectRow(res, err)
}

// ErrBadToken is returned for any invalid join token. Callers should not
// reveal more detail than this to unauthenticated clients.
var ErrBadToken = errors.New("invalid join token")

// UseJoinToken validates a full token string and consumes one use of it.
// The returned error is ErrBadToken for unknown/mismatched tokens, or a
// descriptive error for expired/revoked/exhausted ones.
func (s *Store) UseJoinToken(token string, now time.Time) (*JoinToken, error) {
	id, sec, err := secret.Parse(secret.PrefixJoin, token)
	if err != nil {
		return nil, ErrBadToken
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	t, err := scanJoinToken(tx.QueryRow(`SELECT `+joinTokenCols+` FROM join_tokens WHERE id = ?`, id))
	if errors.Is(err, ErrNotFound) {
		return nil, ErrBadToken
	}
	if err != nil {
		return nil, err
	}
	if !secret.Matches(sec, t.SecretHash) {
		return nil, ErrBadToken
	}
	if err := t.Usable(now); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE join_tokens SET uses = uses + 1 WHERE id = ?`, id); err != nil {
		return nil, err
	}
	t.Uses++
	return t, tx.Commit()
}

// --- API tokens ---

type APIToken struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	SecretHash string     `json:"-"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsed   *time.Time `json:"last_used,omitempty"`
}

func (s *Store) CreateAPIToken(t *APIToken) error {
	_, err := s.db.Exec(`INSERT INTO api_tokens (id, name, secret_hash, created_at) VALUES (?, ?, ?, ?)`,
		t.ID, t.Name, t.SecretHash, unix(t.CreatedAt))
	return err
}

func (s *Store) ListAPITokens() ([]*APIToken, error) {
	rows, err := s.db.Query(`SELECT id, name, created_at, last_used FROM api_tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*APIToken
	for rows.Next() {
		var t APIToken
		var created int64
		var last sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Name, &created, &last); err != nil {
			return nil, err
		}
		t.CreatedAt = time.Unix(created, 0)
		t.LastUsed = fromNullTime(last)
		out = append(out, &t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteAPIToken(id string) error {
	res, err := s.db.Exec(`DELETE FROM api_tokens WHERE id = ?`, id)
	return expectRow(res, err)
}

// CheckAPIToken validates a full API token string and records its use.
func (s *Store) CheckAPIToken(token string, now time.Time) (*APIToken, error) {
	id, sec, err := secret.Parse(secret.PrefixAPI, token)
	if err != nil {
		return nil, ErrNotFound
	}
	var t APIToken
	var created int64
	err = s.db.QueryRow(`SELECT id, name, secret_hash, created_at FROM api_tokens WHERE id = ?`, id).
		Scan(&t.ID, &t.Name, &t.SecretHash, &created)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !secret.Matches(sec, t.SecretHash)) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.CreatedAt = time.Unix(created, 0)
	// Only write last_used once a minute to avoid a write on every request.
	_, _ = s.db.Exec(`UPDATE api_tokens SET last_used = ? WHERE id = ? AND (last_used IS NULL OR last_used < ?)`,
		unix(now), id, unix(now)-60)
	return &t, nil
}

// expectRow turns "no rows affected" into ErrNotFound.
func expectRow(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
