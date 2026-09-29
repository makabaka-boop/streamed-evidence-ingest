// Package blobstore streams uploads into a private temp area and only
// publishes a blob by atomically renaming it into its final,
// content-addressed location. Filenames on disk are derived solely from
// the SHA-256 of the bytes, never from client-supplied names.
package blobstore

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

const (
	// tmpPerm is used for the private staging directory and temp files.
	tmpPerm  fs.FileMode = 0o700
	filePerm fs.FileMode = 0o600
)

// BlobStore manages a root directory with two areas:
//
//	root/tmp   – private staging files, never reachable through the API
//	root/blobs – committed, content-addressed files (sharded by hash)
//
// A file becomes externally readable only once it lives under blobs/ AND
// has a row in the metadata database.
type BlobStore struct {
	root  string
	tmp   string
	blobs string
}

// New creates (if needed) the temp and blob directories under root.
func New(root string) (*BlobStore, error) {
	b := &BlobStore{
		root:  root,
		tmp:   filepath.Join(root, "tmp"),
		blobs: filepath.Join(root, "blobs"),
	}
	for _, d := range []string{b.tmp, b.blobs} {
		if err := os.MkdirAll(d, tmpPerm); err != nil {
			return nil, fmt.Errorf("create %s: %w", d, err)
		}
	}
	return b, nil
}

// Session is one in-flight upload writing to a private temp file.
// The zero value is not usable; obtain one via NewSession.
type Session struct {
	b       *BlobStore
	tmpPath string
	file    *os.File
	done    bool
	renamed bool
}

// NewSession creates a fresh temp file with a random, unpredictable name.
func (b *BlobStore) NewSession() (*Session, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return nil, err
	}
	name := ".upload-" + hex.EncodeToString(buf[:]) + ".part"
	path := filepath.Join(b.tmp, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		return nil, fmt.Errorf("create temp file: %w", err)
	}
	return &Session{b: b, tmpPath: path, file: f}, nil
}

// File returns the underlying temp file for streaming writes.
func (s *Session) File() *os.File { return s.file }

// Sync flushes the temp file's data to disk. Call after the last byte has
// been written and before Finalize.
func (s *Session) Sync() error {
	if err := s.file.Sync(); err != nil {
		return fmt.Errorf("fsync temp file: %w", err)
	}
	return nil
}

// Finalize closes the temp file, then atomically renames it into the
// sharded blob directory under a name derived solely from sha256Hex.
// It returns the storage-relative path (e.g. "ab/cdef...").
//
// The rename is the publish point on the filesystem side: it either
// completes or it does not, so no partial file ever appears under blobs/.
func (s *Session) Finalize(sha256Hex string) (string, error) {
	if s.renamed || s.done {
		return "", errors.New("blobstore: session already finished")
	}
	if len(sha256Hex) != 64 {
		return "", errors.New("blobstore: invalid sha256")
	}
	if err := s.file.Close(); err != nil {
		return "", fmt.Errorf("close temp file: %w", err)
	}
	s.done = true

	rel := filepath.ToSlash(filepath.Join(sha256Hex[:2], sha256Hex[2:]))
	final := filepath.Join(s.b.blobs, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(final), tmpPerm); err != nil {
		return "", fmt.Errorf("create shard dir: %w", err)
	}
	// Identical content may already exist; Rename atomically replaces it
	// with byte-identical content, which is harmless.
	if err := os.Rename(s.tmpPath, final); err != nil {
		return "", fmt.Errorf("publish blob: %w", err)
	}
	s.renamed = true
	return rel, nil
}

// Abort abandons the upload. Before Finalize it removes the private temp
// file. After a successful rename it deliberately does NOTHING: deleting
// a file from the committed area during a live error/rollback could remove
// content another request just committed; such an orphan (file without a
// database row) is reaped on the next startup by Recover instead.
func (s *Session) Abort() {
	if !s.done {
		_ = s.file.Close()
	}
	if !s.renamed {
		_ = os.Remove(s.tmpPath)
	}
}

// Path returns the absolute on-disk path of a committed relative name.
func (b *BlobStore) Path(rel string) string {
	return filepath.Join(b.blobs, filepath.FromSlash(rel))
}

// Recover runs at process startup. It removes every leftover in the temp
// area and every file under blobs/ that has no matching database record.
// Committed files (names present in committed) are never touched.
func (b *BlobStore) Recover(committed map[string]struct{}) (tmpRemoved, orphanRemoved int, err error) {
	if tmpRemoved, err = sweepDir(b.tmp); err != nil {
		return 0, 0, err
	}
	orphanRemoved, err = sweepBlobs(b.blobs, committed)
	return tmpRemoved, orphanRemoved, err
}

// sweepDir removes every entry (including subdirectories) inside dir.
func sweepDir(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// sweepBlobs removes files not present in committed, then prunes empty
// shard directories. committed holds slash-separated relative names.
func sweepBlobs(dir string, committed map[string]struct{}) (int, error) {
	type orph struct{ path, rel string }
	var orphans []orph
	var dirs []string

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		if d.IsDir() {
			dirs = append(dirs, path)
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if _, ok := committed[rel]; !ok {
			orphans = append(orphans, orph{path: path, rel: rel})
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}

	removed := 0
	for _, o := range orphans {
		if err := os.Remove(o.path); err != nil {
			return removed, fmt.Errorf("remove orphan %s: %w", o.rel, err)
		}
		removed++
	}

	// Prune now-empty shard directories, deepest first. The blobs root
	// itself is never removed.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		if err := os.Remove(d); err != nil && !errors.Is(err, fs.ErrExist) {
			// A non-empty directory is fine: it holds committed data.
			var pe *fs.PathError
			if !errors.As(err, &pe) {
				return removed, err
			}
		}
	}
	return removed, nil
}
