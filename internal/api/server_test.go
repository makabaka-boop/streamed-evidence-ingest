package api_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"uploadsvc/internal/api"
	"uploadsvc/internal/storage"
)

func newTestServer(t *testing.T, faults bool) (*httptest.Server, *storage.Store) {
	t.Helper()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ts := httptest.NewServer(api.NewServer(store, faults, nil).Routes())
	t.Cleanup(ts.Close)
	return ts, store
}

func buildBody(t *testing.T, metaJSON string, fileName string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mh := make(map[string][]string)
	mh["Content-Disposition"] = []string{`form-data; name="meta"`}
	mh["Content-Type"] = []string{"application/json"}
	w, err := mw.CreatePart(mh)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(metaJSON))
	fh := make(map[string][]string)
	fh["Content-Disposition"] = []string{
		`form-data; name="file"; filename="` + fileName + `"`}
	fh["Content-Type"] = []string{"application/octet-stream"}
	w, err = mw.CreatePart(fh)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(content)
	mw.Close()
	return &buf, mw.Boundary()
}

func post(t *testing.T, url string, body io.Reader, boundary string, hdr http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestUploadAndRetrieve(t *testing.T) {
	ts, _ := newTestServer(t, false)
	content := []byte("handler-level content")
	body, boundary := buildBody(t, `{"request_id":"h1","content_type":"text/plain"}`,
		"client-name.txt", content)
	resp := post(t, ts.URL+"/uploads", body, boundary, nil)
	if resp.StatusCode != 201 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	var rec map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&rec)
	resp.Body.Close()
	sum := sha256.Sum256(content)
	if rec["sha256"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("bad hash: %v", rec["sha256"])
	}

	// Idempotent replay -> 200.
	body, boundary = buildBody(t, `{"request_id":"h1"}`, "other.txt", content)
	resp = post(t, ts.URL+"/uploads", body, boundary, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("replay status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	// Different content same id -> 409.
	body, boundary = buildBody(t, `{"request_id":"h1"}`, "x.txt", []byte("DIFFERENT"))
	resp = post(t, ts.URL+"/uploads", body, boundary, nil)
	if resp.StatusCode != 409 {
		t.Fatalf("conflict status=%d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestFaultHeaderIgnoredWhenDisabled(t *testing.T) {
	ts, _ := newTestServer(t, false)
	body, boundary := buildBody(t, `{"request_id":"f1"}`, "x", []byte("hi"))
	resp := post(t, ts.URL+"/uploads", body, boundary,
		http.Header{"X-Upload-Fault": {"after_commit"}})
	if resp.StatusCode != 201 {
		t.Fatalf("fault header must be ignored in production, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestDuplicateAndUnknownParts(t *testing.T) {
	ts, _ := newTestServer(t, false)

	send := func(t *testing.T, parts func(*multipart.Writer)) int {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		parts(mw)
		mw.Close()
		req, _ := http.NewRequest("POST", ts.URL+"/uploads", &buf)
		req.Header.Set("Content-Type", "multipart/form-data; boundary="+mw.Boundary())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}

	status := send(t, func(mw *multipart.Writer) {
		w, _ := mw.CreateFormField("meta")
		_, _ = io.Copy(w, strings.NewReader(`{"request_id":"d1"}`))
		w, _ = mw.CreateFormField("file")
		_, _ = w.Write([]byte("a"))
		w, _ = mw.CreateFormField("file")
		_, _ = w.Write([]byte("b"))
	})
	if status != 400 {
		t.Fatalf("duplicate file: got %d want 400", status)
	}

	status = send(t, func(mw *multipart.Writer) {
		w, _ := mw.CreateFormField("meta")
		_, _ = io.Copy(w, strings.NewReader(`{"request_id":"d2"}`))
		w, _ = mw.CreateFormField("file")
		_, _ = w.Write([]byte("a"))
		w, _ = mw.CreateFormField("rogue")
		_, _ = w.Write([]byte("b"))
	})
	if status != 400 {
		t.Fatalf("unknown field: got %d want 400", status)
	}
}
