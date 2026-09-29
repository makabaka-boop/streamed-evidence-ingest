package server_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"uploadsvc/internal/blobstore"
	"uploadsvc/internal/server"
	"uploadsvc/internal/store"
)

const (
	maxFile = server.MaxFileBytes
	maxMeta = server.MaxMetadataBytes
)

type harness struct {
	t      *testing.T
	dir    string
	blobs  *blobstore.BlobStore
	db     *store.Store
	srv    *httptest.Server
	hooks  *countingHooks
	client *http.Client
}

type countingHooks struct {
	writeN   int
	renameN  int
	respondN int
	// mode: "" = no error, "error" = in-process failure,
	// "crash" is not usable inside tests (would kill the test binary);
	// the crash path is covered by the docker verify service.
	writeErr   bool
	renameErr  bool
	respondErr bool
}

func newHarness(t *testing.T, hooks *countingHooks) *harness {
	return newHarnessWithHooks(t, hooks)
}

func newHarnessWithHooks(t *testing.T, hooks *countingHooks) *harness {
	t.Helper()
	dir := t.TempDir()
	blobs, err := blobstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	logger := log.New(io.Discard, "", 0)
	var h server.Hooks
	if hooks != nil {
		h = server.Hooks{
			AfterWrite: func(r *http.Request, id string) error {
				hooks.writeN++
				if hooks.writeErr {
					return fmt.Errorf("boom write")
				}
				return nil
			},
			AfterRename: func(r *http.Request, id, stored string) error {
				hooks.renameN++
				if hooks.renameErr {
					return fmt.Errorf("boom rename")
				}
				return nil
			},
			AfterRespond: func(r *http.Request, rec *store.Record) error {
				hooks.respondN++
				if hooks.respondErr {
					return fmt.Errorf("boom respond")
				}
				return nil
			},
		}
	}
	s := server.New(blobs, db, logger, server.WithHooks(h))
	ts := httptest.NewServer(s.Routes())
	t.Cleanup(func() {
		ts.Close()
		db.Close()
	})
	return &harness{
		t: t, dir: dir, blobs: blobs, db: db, srv: ts, hooks: hooks,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// multipartBody builds a normal two-part request.
func multipartBody(t *testing.T, requestID, mediaType string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	meta := map[string]string{"request_id": requestID}
	if mediaType != "" {
		meta["media_type"] = mediaType
	}
	raw, _ := json.Marshal(meta)
	mh := textproto.MIMEHeader{}
	mh.Set("Content-Disposition", `form-data; name="metadata"`)
	mh.Set("Content-Type", "application/json")
	w, err := mw.CreatePart(mh)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(raw)
	fh := textproto.MIMEHeader{}
	disp := `form-data; name="file"; filename="totally/untrusted/name.bin"`
	fh.Set("Content-Disposition", disp)
	if mediaType != "" {
		fh.Set("Content-Type", mediaType)
	}
	fw, err := mw.CreatePart(fh)
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(content)
	mw.Close()
	return &buf, mw.FormDataContentType()
}

func (h *harness) upload(requestID string, content []byte, headers map[string]string) (*http.Response, []byte) {
	body, ct := multipartBody(h.t, requestID, "application/octet-stream", content)
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/uploads", body)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", ct)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("http: %v", err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, data
}

func (h *harness) uploadRaw(body []byte, ct string, headers map[string]string) (*http.Response, []byte) {
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/uploads", bytes.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", ct)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("http: %v", err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, data
}

func (h *harness) get(id string) (*http.Response, []byte) {
	resp, err := h.client.Get(h.srv.URL + "/blobs/" + id)
	if err != nil {
		h.t.Fatalf("get: %v", err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, data
}

func countFiles(t *testing.T, root string) int {
	t.Helper()
	n := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && !strings.HasSuffix(path, ".db") &&
			!strings.Contains(path, "-wal") && !strings.Contains(path, "-shm") {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// runRecovery simulates a process restart by reopening the data dir and
// running the same startup recovery the main binary does.
func (h *harness) runRecovery() (*blobstore.BlobStore, *store.Store) {
	h.t.Helper()
	blobs2, err := blobstore.New(h.dir)
	if err != nil {
		h.t.Fatal(err)
	}
	db2, err := store.Open(filepath.Join(h.dir, "test.db"))
	if err != nil {
		h.t.Fatal(err)
	}
	committed, err := db2.AllStored()
	if err != nil {
		h.t.Fatal(err)
	}
	if _, _, err := blobs2.Recover(committed); err != nil {
		h.t.Fatalf("recover: %v", err)
	}
	return blobs2, db2
}

func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
