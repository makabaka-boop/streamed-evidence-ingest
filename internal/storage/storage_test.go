package storage_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"uploadsvc/internal/storage"
)

func newStore(t *testing.T) *storage.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := storage.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStageAndCommitRoundTrip(t *testing.T) {
	s := newStore(t)
	payload := bytes.Repeat([]byte("abcdef0123"), 1000)
	want := sha256.Sum256(payload)

	staged, err := s.BeginStage()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(staged, bytes.NewReader(payload)); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if staged.Size() != int64(len(payload)) {
		t.Fatalf("size = %d, want %d", staged.Size(), len(payload))
	}
	if staged.Hash() != hex.EncodeToString(want[:]) {
		t.Fatalf("hash mismatch")
	}
	if _, err := staged.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	rec := storage.Record{
		RequestID:   "r1",
		SHA256:      hex.EncodeToString(want[:]),
		Size:        int64(len(payload)),
		ContentType: "application/octet-stream",
	}
	if err := s.InsertRecord(rec); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := s.Get(context.Background(), "r1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SHA256 != rec.SHA256 || got.Size != rec.Size {
		t.Fatalf("record mismatch: %+v", got)
	}

	f, _, err := s.OpenBlob(got)
	if err != nil {
		t.Fatalf("open blob: %v", err)
	}
	defer f.Close()
	back, _ := io.ReadAll(f)
	if !bytes.Equal(back, payload) {
		t.Fatal("blob contents differ")
	}
}

func TestOverLimitRejected(t *testing.T) {
	s := newStore(t)
	staged, err := s.BeginStage()
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Abort()

	// Write the maximum in one call: accepted.
	full := make([]byte, storage.MaxFileSize)
	if _, err := staged.Write(full); err != nil {
		t.Fatalf("max write: %v", err)
	}
	if _, err := staged.Write([]byte{1}); !errors.Is(err, storage.ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
}

func TestAbortRemovesStagingFile(t *testing.T) {
	s := newStore(t)
	staged, _ := s.BeginStage()
	name := staged.File.Name()
	staged.Abort()
	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Fatalf("staging file still present: %v", err)
	}
}

func TestReconcileRemovesOnlyUncommitted(t *testing.T) {
	dir := t.TempDir()

	// Commit a real upload with store #1.
	s1, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("durable content")
	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])
	st, _ := s1.BeginStage()
	_, _ = st.Write(payload)
	if _, err := st.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := s1.InsertRecord(storage.Record{
		RequestID: "keep", SHA256: hash, Size: int64(len(payload)),
		ContentType: "text/plain",
	}); err != nil {
		t.Fatal(err)
	}

	// Simulate a rename-without-commit orphan blob.
	orphan := strings.Repeat("0", 64)
	orphanPath := filepath.Join(dir, "blobs", orphan[:2], orphan[2:])
	if err := os.MkdirAll(filepath.Dir(orphanPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphanPath, []byte("orphan"), 0o640); err != nil {
		t.Fatal(err)
	}
	// And a leftover staging file.
	if err := os.WriteFile(filepath.Join(dir, "tmp", "upload-leftover.tmp"),
		[]byte("staged"), 0o640); err != nil {
		t.Fatal(err)
	}
	_ = s1.Close()

	// Reopen: reconciliation must delete the orphan + staging file only.
	s2, err := storage.Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Fatalf("orphan blob survived reconcile: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "tmp")); len(entries) != 0 {
		t.Fatalf("staging files survived: %v", entries)
	}
	if _, err := s2.Get(context.Background(), "keep"); err != nil {
		t.Fatalf("committed upload removed by reconcile: %v", err)
	}
	if _, err := os.Stat(s2.BlobPath(hash)); err != nil {
		t.Fatalf("committed blob removed: %v", err)
	}
}

func TestStagingUsesNoClientName(t *testing.T) {
	s := newStore(t)
	st, _ := s.BeginStage()
	defer st.Abort()
	name := filepath.Base(st.File.Name())
	if !strings.HasPrefix(name, "upload-") || !strings.HasSuffix(name, ".tmp") {
		t.Fatalf("unexpected staging name %q", name)
	}
}

// TestStreamingDoesNotBufferWholeFile asserts the staging path keeps memory
// bounded while ingesting a file much larger than the limit would allow in a
// naive ReadAll implementation. We force GC and compare live heap growth.
func TestStreamingDoesNotBufferWholeFile(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	s := newStore(t)

	const size = 16 << 20
	staged, _ := s.BeginStage()
	defer staged.Abort()

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	// A non-allocating source of zeros: proves the pipeline itself does not
	// buffer the file, independent of the client implementation.
	src := &zeroReader{remaining: size}
	if _, err := io.CopyBuffer(staged, src, make([]byte, 32<<10)); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if staged.Size() != size {
		t.Fatalf("size = %d want %d", staged.Size(), size)
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	grew := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	// Streaming a 16 MiB file must not add more than 4 MiB of live heap.
	if grew > size/4 {
		t.Fatalf("heap grew by %d bytes while streaming %d-byte file", grew, size)
	}
}

type zeroReader struct{ remaining int64 }

func (z *zeroReader) Read(p []byte) (int, error) {
	if z.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > z.remaining {
		p = p[:z.remaining]
	}
	for i := range p {
		p[i] = 0
	}
	z.remaining -= int64(len(p))
	return len(p), nil
}
