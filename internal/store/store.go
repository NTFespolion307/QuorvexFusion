// Package store is the controller's persistent state, kept in one SQLite
// database file (pure-Go driver, no cgo).
//
// All controller state that must survive a restart lives here, behind this
// package's methods. Keeping it in one place is also what would make adding
// a standby controller later a contained change.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// ErrNotFound is returned when a looked-up row does not exist.
var ErrNotFound = errors.New("not found")

type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and applies any
// pending schema migrations.
func Open(path string) (*Store, error) {
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer at a time. A single connection serialises all
	// access, which avoids SQLITE_BUSY errors and is plenty fast at the scale
	// of a cluster controller.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// migrations are applied in order; the index+1 of the last applied one is
// stored in SQLite's user_version. Never edit an existing entry: append.
var migrations = []string{
	// 1: nodes, join tokens, API tokens
	`
CREATE TABLE meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE join_tokens (
	id           TEXT PRIMARY KEY,
	secret_hash  TEXT NOT NULL,
	description  TEXT NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL,
	expires_at   INTEGER,            -- NULL: never expires
	max_uses     INTEGER,            -- NULL: unlimited
	uses         INTEGER NOT NULL DEFAULT 0,
	auto_approve INTEGER NOT NULL,
	location     TEXT NOT NULL DEFAULT '',  -- default location for nodes joining with it
	ephemeral    INTEGER NOT NULL DEFAULT 0, -- nodes joining with it are ephemeral
	revoked_at   INTEGER
);

CREATE TABLE nodes (
	id             TEXT PRIMARY KEY,
	name           TEXT NOT NULL,
	status         TEXT NOT NULL,          -- pending | approved | revoked
	pubkey_fp      TEXT NOT NULL UNIQUE,   -- identifies the node's key across re-joins
	csr            BLOB,                   -- kept while pending, signed on approval
	cert_serial    TEXT NOT NULL DEFAULT '',
	token_id       TEXT NOT NULL DEFAULT '',
	location       TEXT NOT NULL DEFAULT '',
	ephemeral      INTEGER NOT NULL DEFAULT 0,
	worker_labels  TEXT NOT NULL DEFAULT '{}', -- labels the worker reports
	labels         TEXT NOT NULL DEFAULT '{}', -- labels set by the admin (override worker labels)
	hardware       TEXT NOT NULL DEFAULT '',   -- last HardwareInfo, protojson
	addr           TEXT NOT NULL DEFAULT '',   -- remote address of the last connection
	shared_storage TEXT NOT NULL DEFAULT '',
	draining       INTEGER NOT NULL DEFAULT 0,
	created_at     INTEGER NOT NULL,
	approved_at    INTEGER,
	last_seen      INTEGER
);

CREATE TABLE api_tokens (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL,
	secret_hash TEXT NOT NULL,
	created_at  INTEGER NOT NULL,
	last_used   INTEGER
);
`,
	// 2: jobs, tasks, attempts
	`
CREATE TABLE jobs (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL,
	spec        TEXT NOT NULL,              -- JSON job spec as submitted
	priority    INTEGER NOT NULL DEFAULT 0,
	task_count  INTEGER NOT NULL,
	canceled    INTEGER NOT NULL DEFAULT 0,
	created_at  INTEGER NOT NULL
);

-- One row per task (one per array index). A task is retried by creating
-- new attempts; the task row tracks the current one.
CREATE TABLE tasks (
	id           TEXT PRIMARY KEY,          -- "<job id>.<index>"
	job_id       TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
	idx          INTEGER NOT NULL,          -- array index ({i})
	seq          INTEGER NOT NULL,          -- queue order
	state        TEXT NOT NULL,             -- queued | assigned | running | succeeded | failed | canceled
	attempts     INTEGER NOT NULL DEFAULT 0,
	failures     INTEGER NOT NULL DEFAULT 0, -- failed attempts (count against retries)
	lost         INTEGER NOT NULL DEFAULT 0, -- attempts lost with their node (don't count)
	attempt_id   TEXT NOT NULL DEFAULT '',  -- current/last attempt
	node_id      TEXT NOT NULL DEFAULT '',
	exit_code    INTEGER,
	error        TEXT NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL,
	started_at   INTEGER,
	finished_at  INTEGER
);
CREATE INDEX tasks_queue ON tasks(state, seq);
CREATE INDEX tasks_job ON tasks(job_id, idx);

-- One row per placement of a task on a node. Only the task's current
-- attempt may complete it, which is what makes completion exactly-once.
CREATE TABLE attempts (
	id           TEXT PRIMARY KEY,
	task_id      TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	number       INTEGER NOT NULL,
	node_id      TEXT NOT NULL,
	state        TEXT NOT NULL,             -- assigned | running | succeeded | failed | canceled | lost
	cpus         REAL NOT NULL,
	memory_bytes INTEGER NOT NULL,
	gpus         TEXT NOT NULL DEFAULT '[]', -- JSON list of assigned GPUs
	exit_code    INTEGER,
	error        TEXT NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL,
	started_at   INTEGER,
	finished_at  INTEGER
);
CREATE INDEX attempts_task ON attempts(task_id, number);
CREATE INDEX attempts_state ON attempts(state);
`,
	// 3: join codes look tokens up by secret
	`
CREATE INDEX join_tokens_secret ON join_tokens(secret_hash);
`,
}

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		// PRAGMA does not accept bound parameters.
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// --- meta key/value ---

// GetMeta returns the value for key, or "" if unset.
func (s *Store) GetMeta(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO meta(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// --- column conversion helpers ---
// Times are stored as unix seconds; optional values as NULL.

func unix(t time.Time) int64 { return t.Unix() }

func nullTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.Unix()
}

func fromNullTime(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.Unix(v.Int64, 0)
	return &t
}

func nullInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func fromNullInt(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	i := int(v.Int64)
	return &i
}

func encodeLabels(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func decodeLabels(s string) map[string]string {
	m := map[string]string{}
	_ = json.Unmarshal([]byte(s), &m)
	return m
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
