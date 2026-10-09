package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// LibraryFile is a file kept in the controller's file library, for use as
// a job input without uploading it again. Folders are just path prefixes.
type LibraryFile struct {
	Path       string    `json:"path"`
	SHA256     string    `json:"sha256"`
	Size       int64     `json:"size"`
	UploadedAt time.Time `json:"uploaded_at"`
}

// PutLibraryFile adds a file, replacing any file at the same path.
func (s *Store) PutLibraryFile(f *LibraryFile) error {
	_, err := s.db.Exec(`INSERT INTO files (path, sha256, size, uploaded_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET sha256 = excluded.sha256, size = excluded.size, uploaded_at = excluded.uploaded_at`,
		f.Path, f.SHA256, f.Size, unix(f.UploadedAt))
	return err
}

// escapeLike makes a string safe inside a LIKE pattern (used with ESCAPE '\').
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// LibraryFiles lists the files at path or below it ("" = everything).
func (s *Store) LibraryFiles(path string) ([]*LibraryFile, error) {
	q := `SELECT path, sha256, size, uploaded_at FROM files`
	var args []any
	if path != "" {
		q += ` WHERE path = ? OR path LIKE ? ESCAPE '\'`
		args = append(args, path, escapeLike(path)+"/%")
	}
	rows, err := s.db.Query(q+` ORDER BY path`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*LibraryFile
	for rows.Next() {
		var f LibraryFile
		var at int64
		if err := rows.Scan(&f.Path, &f.SHA256, &f.Size, &at); err != nil {
			return nil, err
		}
		f.UploadedAt = time.Unix(at, 0)
		out = append(out, &f)
	}
	return out, rows.Err()
}

func (s *Store) LibraryFile(path string) (*LibraryFile, error) {
	var f LibraryFile
	var at int64
	err := s.db.QueryRow(`SELECT path, sha256, size, uploaded_at FROM files WHERE path = ?`, path).
		Scan(&f.Path, &f.SHA256, &f.Size, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	f.UploadedAt = time.Unix(at, 0)
	return &f, err
}

// DeleteLibraryFiles removes a file, or a whole folder, and reports how
// many files were removed.
func (s *Store) DeleteLibraryFiles(path string) (int, error) {
	res, err := s.db.Exec(`DELETE FROM files WHERE path = ? OR path LIKE ? ESCAPE '\'`, path, escapeLike(path)+"/%")
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// LibraryBlobs returns the content hashes the library uses (kept by GC).
func (s *Store) LibraryBlobs(into map[string]bool) error {
	rows, err := s.db.Query(`SELECT DISTINCT sha256 FROM files`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return err
		}
		into[sha] = true
	}
	return rows.Err()
}
