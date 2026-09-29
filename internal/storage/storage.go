// Package storage owns durable state: an SQLite table of upload records and
// a directory tree of content-addressed blob files.
//
// Blobs are written first to a staging directory (tmp/), hashed while
// streaming, and only moved under blobs/ immediately before the database row
// is inserted. On startup, every leftover staging file is removed and every
// blob with no referencing row is treated as an uncommitted artifact and
// removed as well; rows only ever point at content that passed validation.
package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// randomSuffix returns an unpredictable hex string used for temp file names.
func randomSuffix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read failing indicates a broken system; fall back to time.
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// ErrNotFound is returned when no record exists for a request id.
var ErrNotFound = errors.New("upload not found")

// ErrConflict is returned when a request id is stored with different
// content.
var ErrConflict = errors.New("request id already used with different content")

// MaxFileSize is the hard limit for a single uploaded file: 16 MiB.
const MaxFileSize = 16 << 20

// Record is one committed upload.
type Record struct {
	RequestID   string
	SHA256      string
	Size        int64
	ContentType string
	CreatedAt   time.Time
}

// Store combines the blob directory with the SQLite metadata table.
type Store struct {
	db  *sql.DB
	dir string // data root
}

// Open creates the data layout (if needed), reconciles files left behind by
// an earlier crash and returns a ready Store.
func Open(dataDir string) (*Store, error) {
	dir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	for _, sub := range []string{"tmp", "blobs"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			return nil, fmt.Errorf("create %s: %w", sub, err)
		}
	}

	dsn := "file:" + filepath.Join(dir, "uploads.db") +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(FULL)" +
		"&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	s := &Store{db: db, dir: dir}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.reconcile(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) init() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS uploads (
    request_id   TEXT PRIMARY KEY,
    sha256       TEXT NOT NULL,
    size         INTEGER NOT NULL,
    content_type TEXT NOT NULL,
    created_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_uploads_sha256 ON uploads(sha256);`)
	return err
}

// reconcile makes the file system match committed database state after a
// crash. It runs before the service starts listening, so no in-flight
// uploads can exist yet.
func (s *Store) reconcile() error {
	// 1. Anything still staged never committed.
	tmpDir := filepath.Join(s.dir, "tmp")
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(tmpDir, e.Name())); err != nil {
			return fmt.Errorf("clean staging file %s: %w", e.Name(), err)
		}
	}

	// 2. A blob existing without a row means the rename happened but the
	//    commit did not: it is unreadable and must disappear too.
	known := map[string]struct{}{}
	rows, err := s.db.Query(`SELECT sha256 FROM uploads`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return err
		}
		known[h] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	blobsRoot := filepath.Join(s.dir, "blobs")
	shards, err := os.ReadDir(blobsRoot)
	if err != nil {
		return err
	}
	var removedBlobs int
	for _, shard := range shards {
		if !shard.IsDir() || len(shard.Name()) != 2 {
			continue
		}
		leaf := filepath.Join(blobsRoot, shard.Name())
		files, err := os.ReadDir(leaf)
		if err != nil {
			return err
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			hexHash := shard.Name() + f.Name()
			if _, ok := known[hexHash]; !ok {
				if err := os.Remove(filepath.Join(leaf, f.Name())); err != nil {
					return fmt.Errorf("remove orphan blob %s: %w", hexHash, err)
				}
				removedBlobs++
			}
		}
		// Remove the shard directory once empty.
		if left, err := os.ReadDir(leaf); err == nil && len(left) == 0 {
			_ = os.Remove(leaf)
		}
	}
	if removedBlobs > 0 {
		fmt.Fprintf(os.Stderr, "reconcile: removed %d staging file(s) and %d uncommitted blob(s)\n",
			len(entries), removedBlobs)
	} else if len(entries) > 0 {
		fmt.Fprintf(os.Stderr, "reconcile: removed %d staging file(s)\n", len(entries))
	}
	return nil
}

// StagedFile is an in-progress upload living under tmp/.
type StagedFile struct {
	*os.File
	hasher    hash.Hash
	tee       io.Writer
	dir       string
	bytesLeft int64 // remaining capacity before MaxFileSize
	overflow  bool
	size      int64
}

// BeginStage creates a fresh staging file whose name is unrelated to any
// client-supplied filename. The returned writer accepts at most MaxFileSize
// bytes; one extra byte sets ErrTooLarge without being persisted.
func (s *Store) BeginStage() (*StagedFile, error) {
	name := fmt.Sprintf("upload-%d-%s.tmp", time.Now().UnixNano(), randomSuffix())
	f, err := os.OpenFile(filepath.Join(s.dir, "tmp", name),
		os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	return &StagedFile{
		File:      f,
		hasher:    h,
		tee:       io.MultiWriter(f, h),
		dir:       s.dir,
		bytesLeft: MaxFileSize,
	}, nil
}

// ErrTooLarge marks an upload that exceeded MaxFileSize.
var ErrTooLarge = errors.New("uploaded file exceeds 16 MiB")

// writeOnly hides the embedded *os.File.ReadFrom so io.Copy cannot bypass the
// hashing/limit logic in Write.
type writeOnly struct{ sf *StagedFile }

func (w writeOnly) Write(p []byte) (int, error) { return w.sf.Write(p) }

// ReadFrom ensures that even io.Copy(staged, src) funnels every byte through
// Write (which updates the hash and enforces the size cap).
func (sf *StagedFile) ReadFrom(r io.Reader) (int64, error) {
	return io.CopyBuffer(writeOnly{sf}, r, make([]byte, 32<<10))
}

// Write streams one chunk to the staging file and the hash. Bytes past the
// limit are rejected so callers never buffer the full object.
func (sf *StagedFile) Write(p []byte) (int, error) {
	if sf.overflow {
		return 0, ErrTooLarge
	}
	if int64(len(p)) > sf.bytesLeft {
		sf.overflow = true
		return 0, ErrTooLarge
	}
	n, err := sf.tee.Write(p)
	sf.size += int64(n)
	sf.bytesLeft -= int64(n)
	return n, err
}

// Size reports how many accepted bytes were written.
func (sf *StagedFile) Size() int64 { return sf.size }

// Hash returns the lowercase hex SHA-256 of everything written so far.
func (sf *StagedFile) Hash() string {
	return hex.EncodeToString(sf.hasher.Sum(nil))
}

// Abort removes the staging file. Safe to call after a successful Commit.
func (sf *StagedFile) Abort() {
	name := sf.File.Name()
	sf.File.Close()
	_ = os.Remove(name)
}

// Commit flushes, closes and moves the staged blob to its content-addressed
// path. It returns the final path. The destination lives on the same
// filesystem so the rename is atomic.
func (sf *StagedFile) Commit() (string, error) {
	if err := sf.File.Sync(); err != nil {
		return "", err
	}
	if err := sf.File.Close(); err != nil {
		return "", err
	}
	hash := sf.Hash()
	dst := blobPath(sf.dir, hash)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return "", err
	}
	if err := os.Rename(sf.File.Name(), dst); err != nil {
		return "", err
	}
	// fsync the shard directory so the rename survives a power loss before
	// the database commit (the startup reconciler treats it as uncommitted).
	if d, derr := os.Open(filepath.Dir(dst)); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return dst, nil
}

func blobPath(dataDir, hexHash string) string {
	return filepath.Join(dataDir, "blobs", hexHash[:2], hexHash[2:])
}

// Get returns the record for requestID.
func (s *Store) Get(ctx context.Context, requestID string) (Record, error) {
	var rec Record
	var createdAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT request_id, sha256, size, content_type, created_at
         FROM uploads WHERE request_id = ?`, requestID).
		Scan(&rec.RequestID, &rec.SHA256, &rec.Size, &rec.ContentType, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, err
	}
	if t, perr := time.Parse(time.RFC3339Nano, createdAt); perr == nil {
		rec.CreatedAt = t
	}
	return rec, nil
}

// ErrAlreadyExists is returned when a row with the same request_id exists
// (a concurrent insert won the race).
var ErrAlreadyExists = errors.New("upload already exists")

// InsertRecord commits a row for an already-renamed blob.
func (s *Store) InsertRecord(rec Record) error {
	res, err := s.db.Exec(
		`INSERT INTO uploads(request_id, sha256, size, content_type, created_at)
         VALUES(?, ?, ?, ?, ?)`,
		rec.RequestID, rec.SHA256, rec.Size, rec.ContentType,
		rec.CreatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		if isUniqueViolation(err) {
			return ErrAlreadyExists
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrAlreadyExists
	}
	return nil
}

// isUniqueViolation reports a SQLite UNIQUE/PRIMARY KEY constraint failure
// without depending on driver-specific error types.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint failed: UNIQUE") ||
		strings.Contains(msg, "PRIMARY KEY must be unique")
}

// OpenBlob opens the committed blob for reading.
func (s *Store) OpenBlob(rec Record) (*os.File, os.FileInfo, error) {
	f, err := os.Open(blobPath(s.dir, rec.SHA256))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, info, nil
}

// BlobPath exposes the on-disk path for tests/inspection.
func (s *Store) BlobPath(hexHash string) string { return blobPath(s.dir, hexHash) }

// DataDir exposes the data root.
func (s *Store) DataDir() string { return s.dir }

// DB exposes the underlying handle (read-only checks in verify).
func (s *Store) DB() *sql.DB { return s.db }
