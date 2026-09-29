// Package store persists blob metadata in SQLite. A row exists only for
// fully validated, committed blobs; readers therefore never observe a
// half-written file.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

// ErrConflict means the client request id already names a blob with
// different content.
var ErrConflict = errors.New("store: request id conflict")

// Record is the public description of one committed blob.
type Record struct {
	RequestID string
	SHA256    string
	Size      int64
	MediaType string
	StoredAs  string
	CreatedAt string
}

// Store wraps the SQLite metadata database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the metadata database at dsn.
func Open(dsn string) (*Store, error) {
	// _txlock makes BEGIN IMMEDIATE the default for implicit transactions,
	// avoiding SQLITE_BUSY deadlocks under concurrent writers.
	db, err := sql.Open("sqlite", dsn+"?_txlock=immediate&_busy_timeout=5000&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS blobs (
    request_id TEXT PRIMARY KEY,
    sha256     TEXT NOT NULL,
    size       INTEGER NOT NULL,
    media_type TEXT NOT NULL,
    -- Content-addressed path (xx/rest-of-hash). Distinct request ids that
    -- upload byte-identical content alias the same on-disk file, hence no
    -- UNIQUE constraint here; startup recovery keeps a file while at least
    -- one row references it.
    stored_as  TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// Lookup returns the record for a request id, or (nil, nil) if absent.
func (s *Store) Lookup(requestID string) (*Record, error) {
	row := s.db.QueryRow(`SELECT request_id, sha256, size, media_type, stored_as, created_at
FROM blobs WHERE request_id = ?`, requestID)
	var r Record
	if err := row.Scan(&r.RequestID, &r.SHA256, &r.Size, &r.MediaType, &r.StoredAs, &r.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

// Commit atomically publishes a blob. It is called only after the file has
// been renamed into its final, content-addressed location.
//
// It returns ErrConflict when requestID already names a blob whose content
// differs. If the same request id and the same content arrive again, the
// existing record is returned with created=false (idempotent success).
func (s *Store) Commit(requestID, sha string, size int64, mediaType, storedAs string) (rec *Record, created bool, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	if existing, err := queryRecord(tx, requestID); err != nil {
		return nil, false, err
	} else if existing != nil {
		if existing.SHA256 != sha || existing.Size != size {
			return nil, false, ErrConflict
		}
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		return existing, false, nil
	}

	if _, err := tx.Exec(
		`INSERT INTO blobs (request_id, sha256, size, media_type, stored_as) VALUES (?, ?, ?, ?, ?)`,
		requestID, sha, size, mediaType, storedAs); err != nil {
		if !isUniqueViolation(err) {
			return nil, false, err
		}
		// A concurrent transaction committed the same request id between our
		// SELECT and INSERT. End this transaction before re-querying, or with
		// the single connection pool the query would wait on our own lock.
		if rbErr := tx.Rollback(); rbErr != nil {
			return nil, false, rbErr
		}
		existing, qerr := s.Lookup(requestID)
		if qerr != nil {
			return nil, false, qerr
		}
		if existing == nil {
			return nil, false, err
		}
		if existing.SHA256 != sha || existing.Size != size {
			return nil, false, ErrConflict
		}
		return existing, false, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	rec, err = s.Lookup(requestID)
	return rec, err == nil, err
}

// queryRow is the minimal subset of *sql.DB/*sql.Tx used by queryRecord.
type queryRow interface {
	QueryRow(query string, args ...any) *sql.Row
}

func queryRecord(q queryRow, requestID string) (*Record, error) {
	row := q.QueryRow(`SELECT request_id, sha256, size, media_type, stored_as, created_at
FROM blobs WHERE request_id = ?`, requestID)
	var r Record
	if err := row.Scan(&r.RequestID, &r.SHA256, &r.Size, &r.MediaType, &r.StoredAs, &r.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

func isUniqueViolation(err error) bool {
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// AllStored returns the stored_as names of every committed blob. Startup
// cleanup uses it to tell committed files apart from abandoned temp files.
func (s *Store) AllStored() (map[string]struct{}, error) {
	rows, err := s.db.Query(`SELECT stored_as FROM blobs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = struct{}{}
	}
	return out, rows.Err()
}
