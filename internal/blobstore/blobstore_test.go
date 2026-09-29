package blobstore

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func sha256Hex(p []byte) string {
	sum := sha256.Sum256(p)
	return hex.EncodeToString(sum[:])
}

func TestSessionRoundTripAndAbort(t *testing.T) {
	root := t.TempDir()
	b, err := New(root)
	if err != nil {
		t.Fatal(err)
	}

	sess, err := b.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.File().Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	// Abort before finalize removes the private temp file.
	sess.Abort()
	if _, err := os.Stat(sess.tmpPath); !os.IsNotExist(err) {
		t.Fatalf("temp file not removed: %v", err)
	}
	if entries, _ := os.ReadDir(b.tmp); len(entries) != 0 {
		t.Fatalf("temp dir not empty")
	}
}

func TestFinalizeIsContentAddressed(t *testing.T) {
	root := t.TempDir()
	b, _ := New(root)
	sess, _ := b.NewSession()
	sess.File().Write([]byte("hello"))
	if err := sess.Sync(); err != nil {
		t.Fatal(err)
	}
	const sha = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	rel, err := sess.Finalize(sha)
	if err != nil {
		t.Fatal(err)
	}
	if rel != sha[:2]+"/"+sha[2:] {
		t.Fatalf("rel = %q", rel)
	}
	if _, err := os.Stat(b.Path(rel)); err != nil {
		t.Fatalf("final file missing: %v", err)
	}
	if entries, _ := os.ReadDir(b.tmp); len(entries) != 0 {
		t.Fatalf("temp not drained: %v", entries)
	}

	// Abort after a successful rename must not delete the published file.
	sess.Abort()
	if _, err := os.Stat(b.Path(rel)); err != nil {
		t.Fatalf("post-rename abort removed committed file: %v", err)
	}
}

func TestRecoverSweepsTempAndOrphansKeepsCommitted(t *testing.T) {
	root := t.TempDir()
	b, _ := New(root)

	// Committed blob: a file whose name is present in the committed set.
	sess, _ := b.NewSession()
	if _, err := sess.File().Write([]byte("committed bytes")); err != nil {
		t.Fatal(err)
	}
	if err := sess.Sync(); err != nil {
		t.Fatal(err)
	}
	rel, err := sess.Finalize(sha256Hex([]byte("committed bytes")))
	if err != nil {
		t.Fatal(err)
	}

	// Stray temp file.
	stale, _ := os.CreateTemp(b.tmp, ".upload-*.part")
	stale.WriteString("stale")
	stale.Close()

	// Orphan blob written directly to the sharded area.
	const orphan = "ab0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	od := filepath.Join(b.blobs, orphan[:2])
	os.MkdirAll(od, 0o700)
	opath := filepath.Join(od, orphan[2:])
	os.WriteFile(opath, []byte("orphan"), 0o600)

	committed := map[string]struct{}{rel: {}}
	tmpN, orphanN, err := b.Recover(committed)
	if err != nil {
		t.Fatal(err)
	}
	if tmpN != 1 {
		t.Fatalf("tmp removed = %d, want 1", tmpN)
	}
	if orphanN != 1 {
		t.Fatalf("orphan removed = %d, want 1", orphanN)
	}
	if _, err := os.Stat(opath); !os.IsNotExist(err) {
		t.Fatalf("orphan survived: %v", err)
	}
	if entries, _ := os.ReadDir(b.tmp); len(entries) != 0 {
		t.Fatalf("temp not swept")
	}
	if _, err := os.Stat(b.Path(rel)); err != nil {
		t.Fatalf("committed file removed: %v", err)
	}

	// Recovery is idempotent.
	tmp2, orphan2, err := b.Recover(committed)
	if err != nil || tmp2 != 0 || orphan2 != 0 {
		t.Fatalf("second recovery: tmp=%d orphan=%d err=%v", tmp2, orphan2, err)
	}
}
