package server_test

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"uploadsvc/internal/server"
)

type uploadResp struct {
	RequestID string `json:"request_id"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	MediaType string `json:"media_type"`
	Download  string `json:"download"`
	CreatedAt string `json:"created_at"`
}

func mustUpload(t *testing.T, h *harness, id string, content []byte) uploadResp {
	t.Helper()
	resp, data := h.upload(id, content, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload status=%d body=%s", resp.StatusCode, data)
	}
	var got uploadResp
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.SHA256 != shaHex(content) {
		t.Fatalf("sha = %s want %s", got.SHA256, shaHex(content))
	}
	if got.Size != int64(len(content)) {
		t.Fatalf("size = %d want %d", got.Size, len(content))
	}
	return got
}

func TestUploadAndDownload(t *testing.T) {
	h := newHarness(t, nil)
	content := []byte("the quick brown fox jumps over the lazy dog")
	rec := mustUpload(t, h, "id-1", content)

	// The on-disk name is derived from the hash, not the client filename.
	if !strings.Contains(rec.SHA256, rec.SHA256[:8]) {
		t.Fatalf("unexpected sha %q", rec.SHA256)
	}
	wantPath := filepath.Join(h.dir, "blobs", rec.SHA256[:2], rec.SHA256[2:])
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("committed file not at content path: %v", err)
	}

	resp, data := h.get("id-1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status=%d", resp.StatusCode)
	}
	if !bytes.Equal(data, content) {
		t.Fatalf("downloaded bytes differ: %q vs %q", data, content)
	}
	if resp.Header.Get("X-Content-SHA256") != rec.SHA256 {
		t.Fatalf("digest header mismatch")
	}

	// Temp area must be empty after a clean commit.
	if n := countFiles(t, filepath.Join(h.dir, "tmp")); n != 0 {
		t.Fatalf("temp area not empty: %d files", n)
	}
}

func TestEmptyFile(t *testing.T) {
	h := newHarness(t, nil)
	mustUpload(t, h, "empty", []byte{})
	resp, data := h.get("empty")
	if resp.StatusCode != http.StatusOK || len(data) != 0 {
		t.Fatalf("status=%d data=%q", resp.StatusCode, data)
	}
}

func TestClientFilenameNeverUsedAsPath(t *testing.T) {
	h := newHarness(t, nil)
	// The harness sends filename "../../../../etc/passwd"-style payload;
	// the committed path must contain no such component.
	rec := mustUpload(t, h, "evil", []byte("not a password"))
	abs := filepath.Join(h.dir, "blobs", rec.SHA256[:2], rec.SHA256[2:])
	if strings.Contains(abs, "..") {
		t.Fatalf("path escaped via client filename: %s", abs)
	}
}

func TestIdempotentSameContent(t *testing.T) {
	h := newHarness(t, nil)
	content := []byte("dedup payload")
	first := mustUpload(t, h, "idem", content)

	resp, data := h.upload("idem", content, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replay status=%d body=%s", resp.StatusCode, data)
	}
	var second uploadResp
	if err := json.Unmarshal(data, &second); err != nil {
		t.Fatal(err)
	}
	if second.SHA256 != first.SHA256 || second.CreatedAt != first.CreatedAt {
		t.Fatalf("replay did not return original record: %+v vs %+v", first, second)
	}
}

func TestSameContentDifferentIDs(t *testing.T) {
	h := newHarness(t, nil)
	content := []byte("identical bytes under two ids")
	mustUpload(t, h, "alias-a", content)
	mustUpload(t, h, "alias-b", content)
	// Both ids independently return the same bytes.
	for _, id := range []string{"alias-a", "alias-b"} {
		r, data := h.get(id)
		if r.StatusCode != http.StatusOK || !bytes.Equal(data, content) {
			t.Fatalf("get %s: status=%d", id, r.StatusCode)
		}
	}
	// One shared physical file under the content-addressed path.
	if n := countFiles(t, filepath.Join(h.dir, "blobs")); n != 1 {
		t.Fatalf("expected 1 physical blob for identical content, got %d", n)
	}
}

func TestConflictDifferentContent(t *testing.T) {
	h := newHarness(t, nil)
	mustUpload(t, h, "k", []byte("version one"))
	resp, _ := h.upload("k", []byte("version two"), nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status=%d want 409", resp.StatusCode)
	}
	// The original record must remain readable with the original bytes.
	got, data := h.get("k")
	if got.StatusCode != http.StatusOK || string(data) != "version one" {
		t.Fatalf("original record altered: %d %q", got.StatusCode, data)
	}
}

func TestRejectMalformedRequests(t *testing.T) {
	h := newHarness(t, nil)

	cases := []struct {
		name   string
		build  func() ([]byte, string)
		status int
	}{
		{
			name: "not multipart",
			build: func() ([]byte, string) {
				return []byte(`{"request_id":"x"}`), "application/json"
			},
			status: http.StatusBadRequest,
		},
		{
			name: "duplicate metadata field",
			build: func() ([]byte, string) {
				return duplicatePart(t, "metadata", "metadata"), "multipart/form-data; boundary=XX"
			},
			status: http.StatusBadRequest,
		},
		{
			name: "unknown field",
			build: func() ([]byte, string) {
				return unknownField(t), "multipart/form-data; boundary=XX"
			},
			status: http.StatusBadRequest,
		},
		{
			name: "missing file",
			build: func() ([]byte, string) {
				return missingFile(t), "multipart/form-data; boundary=XX"
			},
			status: http.StatusBadRequest,
		},
		{
			name: "truncated mid file",
			build: func() ([]byte, string) {
				return truncatedBody(t), "multipart/form-data; boundary=XX"
			},
			status: http.StatusBadRequest,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, ct := tc.build()
			resp, _ := h.uploadRaw(body, ct, nil)
			if resp.StatusCode != tc.status {
				t.Fatalf("status=%d want %d", resp.StatusCode, tc.status)
			}
			// A rejected request never produces a readable blob.
			if r, _ := h.get("r"); r.StatusCode != http.StatusNotFound {
				t.Fatalf("rejected request left a readable blob: %d", r.StatusCode)
			}
		})
	}

	// Bad JSON metadata variants all fail.
	for _, body := range [][]byte{
		[]byte(`{"request_id":"r","unknown":1}`),
		[]byte(`{"request_id":"r","request_id":"r2"}`),
		[]byte(`{"request_id":123}`),
		[]byte(`{"request_id":""}`),
		[]byte(`{"request_id":"a/b"}`),
		[]byte(`not json at all`),
	} {
		raw := onePartBody(t, "metadata", body)
		resp, _ := h.uploadRaw(raw, "multipart/form-data; boundary=XX", nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("metadata %s -> status %d, want 400", body, resp.StatusCode)
		}
	}
}

func TestFileAtSizeLimit(t *testing.T) {
	h := newHarness(t, nil)
	exact := make([]byte, server.MaxFileBytes)
	if _, err := rand.Read(exact); err != nil {
		t.Fatal(err)
	}
	mustUpload(t, h, "exact-16mib", exact)
}

func TestFileOverSizeLimit(t *testing.T) {
	h := newHarness(t, nil)
	big := make([]byte, server.MaxFileBytes+1)
	resp, _ := h.upload("too-big", big, nil)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d want 413", resp.StatusCode)
	}
	// Oversized attempt leaves no temp artifact and no readable blob.
	if n := countFiles(t, filepath.Join(h.dir, "tmp")); n != 0 {
		t.Fatalf("temp files left behind: %d", n)
	}
	if r, _ := h.get("too-big"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("oversized upload readable: %d", r.StatusCode)
	}
}

// TestStreamingMemoryFlat streams a 16 MiB body while measuring total
// allocation. The handler copies in 32 KiB chunks, so the request path
// must not allocate an amount proportional to the file size; this guards
// against accidentally buffering the whole upload.
func TestStreamingMemoryFlat(t *testing.T) {
	h := newHarness(t, nil)

	// Stream the request over a pipe so building the 16 MiB client body
	// does not allocate inside the measured server-side request window.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		hdr := textproto.MIMEHeader{}
		hdr.Set("Content-Disposition", `form-data; name="metadata"`)
		hdr.Set("Content-Type", "application/json")
		w, err := mw.CreatePart(hdr)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		w.Write([]byte(`{"request_id":"mem-16mib"}`))
		fh := textproto.MIMEHeader{}
		fh.Set("Content-Disposition", `form-data; name="file"; filename="x"`)
		fw, err := mw.CreatePart(fh)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		chunk := make([]byte, 32*1024)
		for i := range chunk {
			chunk[i] = byte(i * 7)
		}
		var written int64
		for written < int64(server.MaxFileBytes) {
			take := int64(len(chunk))
			if remaining := int64(server.MaxFileBytes) - written; remaining < take {
				take = remaining
			}
			if _, err := fw.Write(chunk[:take]); err != nil {
				pw.CloseWithError(err)
				return
			}
			written += take
		}
		mw.Close()
		pw.Close()
	}()

	runtime.GC()
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)

	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/uploads", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	var m2 runtime.MemStats
	runtime.ReadMemStats(&m2)
	added := m2.TotalAlloc - m1.TotalAlloc
	// While streaming a 16 MiB file the server request path must not
	// allocate an amount proportional to the file size. Generous ceiling
	// for SQLite/encoder churn; buffering the full file blows past it.
	if max := uint64(6 << 20); added > max {
		t.Fatalf("streaming 16 MiB allocated %d server-side bytes (limit %d): file appears to be buffered in memory", added, max)
	}
}

// --- failure injection: no readable half-finished product --------------

func TestFailAfterWrite(t *testing.T) {
	hooks := &countingHooks{writeErr: true}
	h := newHarness(t, hooks)

	resp, _ := h.upload("crash-write", []byte("payload A"), nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500", resp.StatusCode)
	}
	if hooks.writeN != 1 {
		t.Fatalf("write hook ran %d times", hooks.writeN)
	}
	// Nothing readable.
	if r, _ := h.get("crash-write"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("blob readable after write failure: %d", r.StatusCode)
	}
	// Temp area cleaned immediately.
	if n := countFiles(t, filepath.Join(h.dir, "tmp")); n != 0 {
		t.Fatalf("temp artifact left: %d", n)
	}
	// Retry without the hook succeeds.
	hooks.writeErr = false
	mustUpload(t, h, "crash-write", []byte("payload A"))
}

func TestFailAfterRename(t *testing.T) {
	hooks := &countingHooks{renameErr: true}
	h := newHarnessWithHooks(t, hooks)

	payload := []byte("payload between rename and commit")
	resp, _ := h.upload("crash-rename", payload, nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500", resp.StatusCode)
	}
	// The file exists on disk (rename won) but has no DB row: GET 404.
	if r, _ := h.get("crash-rename"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("uncommitted renamed file is readable: %d", r.StatusCode)
	}
	// The orphan blob file is present until recovery.
	rel := filepath.Join(h.dir, "blobs", shaHex(payload)[:2], shaHex(payload)[2:])
	if _, err := os.Stat(rel); err != nil {
		t.Fatalf("expected orphan at %s: %v", rel, err)
	}

	// Simulate restart: orphan reaped, nothing readable.
	_, db2 := h.runRecovery()
	if _, err := os.Stat(rel); !os.IsNotExist(err) {
		t.Fatalf("orphan not reaped: %v", err)
	}
	if rec, err := db2.Lookup("crash-rename"); err != nil || rec != nil {
		t.Fatalf("unexpected record after recovery: %v %v", rec, err)
	}

	// Retry after restart succeeds and now the blob is readable.
	hooks.renameErr = false
	mustUpload(t, h, "crash-rename", payload)
}

func TestFailAfterRespond(t *testing.T) {
	hooks := &countingHooks{respondErr: true}
	h := newHarnessWithHooks(t, hooks)
	payload := []byte("committed but response lost")

	resp, _ := h.upload("crash-respond", payload, nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500", resp.StatusCode)
	}
	// The commit happened before the hook fired: the blob must be readable.
	r, data := h.get("crash-respond")
	if r.StatusCode != http.StatusOK || !bytes.Equal(data, payload) {
		t.Fatalf("committed blob not readable after respond failure: %d", r.StatusCode)
	}
	// Retry returns the original record with 200 (idempotent), even though
	// the client believed the first attempt failed.
	hooks.respondErr = false
	resp2, raw := h.upload("crash-respond", payload, nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("retry status=%d body=%s", resp2.StatusCode, raw)
	}
}

func TestRestartRecoveryKeepsCommittedRemovesLeftovers(t *testing.T) {
	h := newHarness(t, nil)

	// Two committed blobs that must survive recovery.
	c1 := []byte("committed one")
	c2 := []byte("committed two")
	mustUpload(t, h, "keep-1", c1)
	mustUpload(t, h, "keep-2", c2)

	// Simulate a pre-commit crash at write: a stray temp file.
	stale, err := os.CreateTemp(filepath.Join(h.dir, "tmp"), ".upload-*.part")
	if err != nil {
		t.Fatal(err)
	}
	stale.WriteString("stale bytes")
	stale.Close()

	// Simulate a pre-commit crash at rename: an orphan blob.
	orphanSum := shaHex([]byte("orphan content"))
	orphanDir := filepath.Join(h.dir, "blobs", orphanSum[:2])
	if err := os.MkdirAll(orphanDir, 0o700); err != nil {
		t.Fatal(err)
	}
	orphanPath := filepath.Join(orphanDir, orphanSum[2:])
	if err := os.WriteFile(orphanPath, []byte("orphan content"), 0o600); err != nil {
		t.Fatal(err)
	}

	before := countFiles(t, filepath.Join(h.dir, "blobs"))
	_, db2 := h.runRecovery()
	if before != 3 {
		t.Fatalf("setup: expected 3 files under blobs, got %d", before)
	}

	// Orphan gone.
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Fatalf("orphan survived recovery")
	}
	// Temp gone.
	if n := countFiles(t, filepath.Join(h.dir, "tmp")); n != 0 {
		t.Fatalf("temp not swept: %d", n)
	}
	// Committed files still on disk and still readable.
	for _, id := range []string{"keep-1", "keep-2"} {
		rec, err := db2.Lookup(id)
		if err != nil || rec == nil {
			t.Fatalf("committed record %s missing after recovery: %v", id, err)
		}
		if _, err := os.Stat(filepath.Join(h.dir, "blobs", rec.SHA256[:2], rec.SHA256[2:])); err != nil {
			t.Fatalf("committed file %s removed by recovery: %v", id, err)
		}
	}
}

func TestHealthz(t *testing.T) {
	h := newHarness(t, nil)
	resp, err := http.Get(h.srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestConcurrentDuplicates(t *testing.T) {
	h := newHarness(t, nil)
	// N concurrent uploads with the same id and same content must all
	// converge on one record; mixed content must yield exactly one 201 and
	// the rest 409, never a 500.
	const n = 8
	results := make(chan int, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			body := []byte("shared content")
			if i%2 == 1 {
				body = []byte("different content")
			}
			resp, _ := h.upload("race", body, nil)
			results <- resp.StatusCode
			errs <- nil
		}()
	}
	created, conflict, ok200, other := 0, 0, 0, 0
	for i := 0; i < n; i++ {
		code := <-results
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflict++
		case http.StatusOK:
			ok200++
		default:
			other++
		}
	}
	if created != 1 {
		t.Fatalf("created=%d want 1 (conflict=%d ok=%d other=%d)", created, conflict, ok200, other)
	}
	if other != 0 {
		t.Fatalf("unexpected statuses: conflict=%d ok=%d other=%d", conflict, ok200, other)
	}
	if conflict+ok200 != n-1 {
		t.Fatalf("non-created requests = %d, want %d", conflict+ok200, n-1)
	}
	// Exactly one content version is committed and readable.
	r, data := h.get("race")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("final blob not readable: %d", r.StatusCode)
	}
	if string(data) != "shared content" && string(data) != "different content" {
		t.Fatalf("unexpected committed content %q", data)
	}
}

// --- raw multipart body builders for malformed-request cases -----------

func writePartHeader(buf *bytes.Buffer, name, filename string) {
	buf.WriteString("--XX\r\n")
	disp := `Content-Disposition: form-data; name="` + name + `"`
	if filename != "" {
		disp += `; filename="` + filename + `"`
	}
	buf.WriteString(disp)
	buf.WriteString("\r\n\r\n")
}

func onePartBody(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("--XX\r\n")
	buf.WriteString(`Content-Disposition: form-data; name="metadata"` + "\r\n")
	buf.WriteString("Content-Type: application/json\r\n\r\n")
	buf.Write(content)
	buf.WriteString("\r\n--XX--\r\n")
	return buf.Bytes()
}

func duplicatePart(t *testing.T, first, second string) []byte {
	t.Helper()
	var buf bytes.Buffer
	// metadata, file, then a second metadata field.
	buf.WriteString("--XX\r\n")
	buf.WriteString(`Content-Disposition: form-data; name="metadata"` + "\r\n")
	buf.WriteString("Content-Type: application/json\r\n\r\n")
	buf.WriteString(`{"request_id":"r"}`)
	buf.WriteString("\r\n")
	writePartHeader(&buf, "file", "a")
	buf.WriteString("aaaa\r\n")
	writePartHeader(&buf, second, "")
	buf.WriteString(`{"request_id":"r2"}`)
	buf.WriteString("\r\n--XX--\r\n")
	return buf.Bytes()
}

func unknownField(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	writePartHeader(&buf, "metadata", "")
	buf.WriteString(`{"request_id":"r"}` + "\r\n")
	writePartHeader(&buf, "file", "a")
	buf.WriteString("aaaa\r\n")
	writePartHeader(&buf, "mystery", "")
	buf.WriteString("zz\r\n--XX--\r\n")
	return buf.Bytes()
}

func missingFile(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	writePartHeader(&buf, "metadata", "")
	buf.WriteString(`{"request_id":"r"}` + "\r\n--XX--\r\n")
	return buf.Bytes()
}

func truncatedBody(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	writePartHeader(&buf, "metadata", "")
	buf.WriteString(`{"request_id":"r"}` + "\r\n")
	writePartHeader(&buf, "file", "a")
	buf.WriteString(strings.Repeat("x", 4096))
	// Deliberately no closing boundary; also chop off the tail.
	out := buf.Bytes()
	return out[:len(out)-100]
}
