package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCommitNewThenReplayThenConflict(t *testing.T) {
	s := openTest(t)
	rec, created, err := s.Commit("id1", "aaa", 10, "text/plain", "aa/a")
	if err != nil || !created {
		t.Fatalf("first commit created=%v err=%v", created, err)
	}
	if rec.RequestID != "id1" {
		t.Fatalf("rec = %+v", rec)
	}

	// Same id, same content -> replay, not created.
	rec2, created, err := s.Commit("id1", "aaa", 10, "text/plain", "aa/a")
	if err != nil || created {
		t.Fatalf("replay created=%v err=%v", created, err)
	}
	if rec2.SHA256 != "aaa" {
		t.Fatalf("replay rec = %+v", rec2)
	}

	// Same id, different content -> conflict.
	_, _, err = s.Commit("id1", "bbb", 10, "text/plain", "bb/b")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}

	// Same id, same hash but different size still conflicts (content differs).
	_, _, err = s.Commit("id1", "aaa", 11, "text/plain", "aa/a")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("size-diff err = %v, want conflict", err)
	}
}

func TestSameContentDifferentIDsAliasOneFile(t *testing.T) {
	s := openTest(t)
	// Two distinct request ids uploading byte-identical content must both
	// succeed and point at the same content-addressed path.
	if _, created, err := s.Commit("id-a", "h1", 5, "a/b", "h1/rest"); err != nil || !created {
		t.Fatalf("id-a: created=%v err=%v", created, err)
	}
	if _, created, err := s.Commit("id-b", "h1", 5, "a/b", "h1/rest"); err != nil || !created {
		t.Fatalf("id-b alias: created=%v err=%v", created, err)
	}
	a, _ := s.Lookup("id-a")
	b, _ := s.Lookup("id-b")
	if a == nil || b == nil || a.StoredAs != b.StoredAs {
		t.Fatalf("aliasing broken: %+v %+v", a, b)
	}
}

func TestAllStoredDedups(t *testing.T) {
	s := openTest(t)
	_, _, _ = s.Commit("a", "h1", 1, "x", "h1/rest")
	_, _, _ = s.Commit("b", "h1", 1, "x", "h1/rest")
	got, err := s.AllStored()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("AllStored = %v, want single shared path", got)
	}
}
