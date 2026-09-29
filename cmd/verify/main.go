// Command verify is the one-shot acceptance test for the upload stack.
//
//	-mode=healthcheck   exits 0 when GET /healthz succeeds (container probe)
//	-mode=full          runs the complete contract + crash/recovery suite
//	                    against -base, inspecting the data directory at
//	                    -data, and exits non-zero on the first violation.
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	maxFile = 16 << 20
	// boundary must be unique enough not to appear in random payloads.
	boundary = "----verifyBoundary7f3a9c1e"
)

func main() {
	mode := flag.String("mode", "full", "healthcheck or full")
	base := flag.String("base", "http://app:8080", "service base URL")
	data := flag.String("data", "/data", "shared data directory")
	flag.Parse()

	if *mode == "healthcheck" {
		resp, err := http.Get(strings.TrimRight(*base, "/") + "/healthz")
		if err != nil || resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		os.Exit(0)
	}

	if err := runFull(*base, *data); err != nil {
		log.Fatalf("VERIFY FAILED: %v", err)
	}
	log.Print("VERIFY OK: all checks passed")
}

type v struct {
	base   string
	data   string
	tok    string // unique per run: keeps repeated runs idempotent
	seed   byte   // perturbs large deterministic payloads per run
	client *http.Client
}

// newToken returns a short random, request-id-safe run token.
func newToken() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fall back to time if the system RNG is unavailable.
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// id turns a logical name into a per-run unique request id.
func (t *v) id(name string) string { return "v" + t.tok + "-" + name }

// tagged makes a payload unique to this run while remaining deterministic
// within it, so orphan/committed hash paths cannot collide across runs.
func (t *v) tagged(label string) []byte {
	return []byte(label + "::run" + t.tok)
}

func runFull(base, dataDir string) error {
	tok := newToken()
	tt := &v{
		base: strings.TrimRight(base, "/"),
		data: dataDir,
		tok:  tok,
		seed: tok[0] ^ tok[len(tok)-1],
		client: &http.Client{
			Timeout: 60 * time.Second,
			// Crash failpoints sever the connection; surface that as an
			// error instead of silently retrying the POST.
		},
	}
	if tt.seed == 0 {
		tt.seed = 0x5a
	}
	log.Printf("run token: %s", tt.tok)
	t := tt

	log.Print("[1/6] waiting for service health")
	if err := t.waitHealthy(30 * time.Second); err != nil {
		return err
	}

	log.Print("[2/6] basic upload / download contract")
	if err := t.contract(); err != nil {
		return err
	}

	log.Print("[3/6] strict request rejection")
	if err := t.rejections(); err != nil {
		return err
	}

	log.Print("[4/6] in-process failure injection")
	if err := t.failpointsInProcess(); err != nil {
		return err
	}

	log.Print("[5/6] process-crash failure injection + restart recovery")
	if err := t.failpointsCrash(); err != nil {
		return err
	}

	log.Print("[6/6] on-disk / database invariants")
	return t.invariants()
}

func (t *v) waitHealthy(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := t.client.Get(t.base + "/healthz")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		} else {
			lastErr = err
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("service did not become healthy: %v", lastErr)
}

type uploadJSON struct {
	RequestID string `json:"request_id"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	Download  string `json:"download"`
}

// doUpload posts a small multipart body. filename is the untrusted client
// filename; failpoint optionally sets X-Upload-Failpoint.
func (t *v) doUpload(id, clientFilename string, content []byte, failpoint string) (int, []byte, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.SetBoundary(boundary)

	mh := make(map[string][]string)
	mh["Content-Disposition"] = []string{`form-data; name="metadata"`}
	mh["Content-Type"] = []string{"application/json"}
	w, err := mw.CreatePart(mh)
	if err != nil {
		return 0, nil, err
	}
	w.Write([]byte(fmt.Sprintf(`{"request_id":%q,"media_type":"application/octet-stream"}`, id)))

	fh := make(map[string][]string)
	fh["Content-Disposition"] = []string{fmt.Sprintf(`form-data; name="file"; filename=%q`, clientFilename)}
	fw, err := mw.CreatePart(fh)
	if err != nil {
		return 0, nil, err
	}
	fw.Write(content)
	mw.Close()

	req, err := http.NewRequest(http.MethodPost, t.base+"/uploads", &buf)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	if failpoint != "" {
		req.Header.Set("X-Upload-Failpoint", failpoint)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data, nil
}

func (t *v) get(id string) (int, []byte, error) {
	resp, err := t.client.Get(t.base + "/blobs/" + id)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data, nil
}

func shaOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func (t *v) contract() error {
	content := t.tagged("contract payload " + strings.Repeat("ab", 100))
	wantSHA := shaOf(content)
	idC1 := t.id("c-1")

	// Untrusted client filename, including traversal characters, must not
	// influence the on-disk path.
	status, body, err := t.doUpload(idC1, "../../../../tmp/evil\\name", content, "")
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	if status != http.StatusCreated {
		return fmt.Errorf("upload status=%d body=%s", status, body)
	}
	var rec uploadJSON
	if err := json.Unmarshal(body, &rec); err != nil {
		return err
	}
	if rec.SHA256 != wantSHA {
		return fmt.Errorf("sha mismatch: got %s want %s", rec.SHA256, wantSHA)
	}
	if rec.Size != int64(len(content)) {
		return fmt.Errorf("size mismatch: %d", rec.Size)
	}

	// File lives at a content-addressed path, not the client filename.
	shard := filepath.Join(t.data, "blobs", wantSHA[:2], wantSHA[2:])
	if _, err := os.Stat(shard); err != nil {
		return fmt.Errorf("committed file not at hash path %s: %w", shard, err)
	}
	if _, err := os.Stat(filepath.Join(t.data, "tmp", "evil")); !os.IsNotExist(err) {
		return fmt.Errorf("client filename leaked onto disk")
	}

	// GET returns exact bytes.
	status, got, err := t.get(idC1)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("get status=%d err=%v", status, err)
	}
	if !bytes.Equal(got, content) {
		return fmt.Errorf("downloaded content differs (len %d vs %d)", len(got), len(content))
	}

	// Idempotent replay: same id + same content -> 200 with same digest.
	status2, body2, err := t.doUpload(idC1, "ignored", content, "")
	if err != nil || status2 != http.StatusOK {
		return fmt.Errorf("idempotent replay status=%d err=%v body=%s", status2, err, body2)
	}
	var rec2 uploadJSON
	json.Unmarshal(body2, &rec2)
	if rec2.SHA256 != wantSHA {
		return fmt.Errorf("replay returned different record")
	}

	// Same id + different content -> 409, original stays readable.
	other := t.tagged("totally different bytes")
	status3, _, err := t.doUpload(idC1, "ignored", other, "")
	if err != nil || status3 != http.StatusConflict {
		return fmt.Errorf("conflict status=%d err=%v", status3, err)
	}
	status4, got4, err := t.get(idC1)
	if err != nil || status4 != http.StatusOK || !bytes.Equal(got4, content) {
		return fmt.Errorf("original record mutated after conflict: status=%d err=%v", status4, err)
	}

	// Empty file is a valid 0-byte blob.
	idEmpty := t.id("c-empty")
	status, body, err = t.doUpload(idEmpty, "empty", []byte{}, "")
	if err != nil || status != http.StatusCreated {
		return fmt.Errorf("empty upload status=%d err=%v body=%s", status, err, body)
	}
	status, got, err = t.get(idEmpty)
	if err != nil || status != http.StatusOK || len(got) != 0 {
		return fmt.Errorf("empty download status=%d len=%d err=%v", status, len(got), err)
	}

	// Distinct ids with identical content alias one physical blob.
	alias := t.tagged("alias me please")
	idA1 := t.id("c-alias-1")
	idA2 := t.id("c-alias-2")
	if status, _, err := t.doUpload(idA1, "x", alias, ""); err != nil || status != http.StatusCreated {
		return fmt.Errorf("alias-1 status=%d err=%v", status, err)
	}
	if status, _, err := t.doUpload(idA2, "y", alias, ""); err != nil || status != http.StatusCreated {
		return fmt.Errorf("alias-2 status=%d err=%v (content-dedup broken)", status, err)
	}
	for _, id := range []string{idA1, idA2} {
		if status, got, _ := t.get(id); status != http.StatusOK || !bytes.Equal(got, alias) {
			return fmt.Errorf("alias %s not readable: %d", id, status)
		}
	}
	return nil
}

// rawPost sends a hand-built multipart body so rejection cases can be
// malformed in precise ways.
func (t *v) rawPost(body []byte) (int, error) {
	req, err := http.NewRequest(http.MethodPost, t.base+"/uploads", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	resp, err := t.client.Do(req)
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, nil
}

func partHeader(name, filename string) string {
	disp := "--" + boundary + "\r\nContent-Disposition: form-data; name=\"" + name + "\""
	if filename != "" {
		disp += "; filename=\"" + filename + "\""
	}
	return disp + "\r\n\r\n"
}

func (t *v) rejections() error {
	// Not multipart.
	req, _ := http.NewRequest(http.MethodPost, t.base+"/uploads", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		return fmt.Errorf("non-multipart -> %d, want 400", resp.StatusCode)
	}

	meta := func(jsonBody string) []byte {
		return []byte(partHeader("metadata", "") + jsonBody + "\r\n")
	}

	// Bad metadata variants.
	for _, bad := range []string{
		`{"request_id":"r","unknown_field":1}`,
		`{"request_id":"a","request_id":"b"}`,
		`{"request_id":123}`,
		`{"request_id":""}`,
		`{"request_id":"a/b"}`,
		`this is not json`,
	} {
		body := append(meta(bad), []byte(partHeader("file", "f")+"data\r\n--"+boundary+"--\r\n")...)
		status, err := t.rawPost(body)
		if err != nil || status != http.StatusBadRequest {
			return fmt.Errorf("metadata %q -> status=%d err=%v, want 400", bad, status, err)
		}
	}

	// Duplicate metadata field.
	dup := meta(`{"request_id":"r"}`)
	dup = append(dup, []byte(partHeader("file", "f")+"aaaa\r\n")...)
	dup = append(dup, meta(`{"request_id":"r2"}`)...)
	dup = append(dup, []byte("--"+boundary+"--\r\n")...)
	if status, err := t.rawPost(dup); err != nil || status != http.StatusBadRequest {
		return fmt.Errorf("duplicate metadata -> status=%d err=%v, want 400", status, err)
	}

	// Unknown field after the file.
	unk := meta(`{"request_id":"r"}`)
	unk = append(unk, []byte(partHeader("file", "f")+"aaaa\r\n")...)
	unk = append(unk, []byte(partHeader("mystery", "")+"zz\r\n--"+boundary+"--\r\n")...)
	if status, err := t.rawPost(unk); err != nil || status != http.StatusBadRequest {
		return fmt.Errorf("unknown field -> status=%d err=%v, want 400", status, err)
	}

	// File missing entirely.
	missing := append(meta(`{"request_id":"r"}`), []byte("--"+boundary+"--\r\n")...)
	if status, err := t.rawPost(missing); err != nil || status != http.StatusBadRequest {
		return fmt.Errorf("missing file -> status=%d err=%v, want 400", status, err)
	}

	// Truncated body: file bytes with no closing boundary, chopped short.
	var tr bytes.Buffer
	tr.WriteString(partHeader("metadata", ""))
	tr.WriteString(`{"request_id":"r"}`)
	tr.WriteString("\r\n")
	tr.WriteString(partHeader("file", "f"))
	tr.WriteString(strings.Repeat("x", 8192))
	trunc := tr.Bytes()
	trunc = trunc[:len(trunc)-500]
	if status, err := t.rawPost(trunc); err != nil || status != http.StatusBadRequest {
		return fmt.Errorf("truncated body -> status=%d err=%v, want 400", status, err)
	}

	// Body that ends cleanly on a bare (non-closing) boundary: also
	// truncated, not a valid two-part request.
	var bare bytes.Buffer
	bare.WriteString(partHeader("metadata", ""))
	bare.WriteString(`{"request_id":"r"}`)
	bare.WriteString("\r\n")
	bare.WriteString(partHeader("file", "f"))
	bare.WriteString("data\r\n")
	bare.WriteString("--" + boundary + "\r\n") // no trailing "--"
	if status, err := t.rawPost(bare.Bytes()); err != nil || status != http.StatusBadRequest {
		return fmt.Errorf("bare boundary EOF -> status=%d err=%v, want 400", status, err)
	}

	// Exactly 16 MiB succeeds.
	if err := t.streamUpload(t.id("r-exact"), maxFile, http.StatusCreated, t.seed, false); err != nil {
		return err
	}
	// 16 MiB + 1 is rejected as too large.
	idOver := t.id("r-oversize")
	if err := t.streamUpload(idOver, maxFile+1, http.StatusRequestEntityTooLarge, t.seed, false); err != nil {
		return err
	}
	// Oversized rejection leaves no staged temp file and no readable blob.
	if n := countPartFiles(filepath.Join(t.data, "tmp")); n != 0 {
		return fmt.Errorf("oversized upload left %d temp files", n)
	}
	if status, _, err := t.get(idOver); err != nil || status != http.StatusNotFound {
		return fmt.Errorf("oversized blob readable: status=%d err=%v", status, err)
	}
	return nil
}

// streamUpload streams exactly size bytes over a pipe so the verify client
// itself stays small. seed perturbs the byte pattern so large uploads are
// unique per run even with a per-run unique request id.
func (t *v) streamUpload(id string, size int, wantStatus int, seed byte, crash bool) error {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	mw.SetBoundary(boundary)
	go func() {
		var err error
		defer func() { pw.CloseWithError(err) }()
		mh := make(map[string][]string)
		mh["Content-Disposition"] = []string{`form-data; name="metadata"`}
		mh["Content-Type"] = []string{"application/json"}
		w, e := mw.CreatePart(mh)
		if e != nil {
			err = e
			return
		}
		if _, err = w.Write([]byte(fmt.Sprintf(`{"request_id":%q}`, id))); err != nil {
			return
		}
		fh := make(map[string][]string)
		fh["Content-Disposition"] = []string{`form-data; name="file"; filename="stream.bin"`}
		fw, e := mw.CreatePart(fh)
		if e != nil {
			err = e
			return
		}
		buf := make([]byte, 32*1024)
		for i := range buf {
			buf[i] = byte(i*7+3) ^ seed
		}
		remaining := size
		for remaining > 0 {
			take := len(buf)
			if remaining < take {
				take = remaining
			}
			if _, err = fw.Write(buf[:take]); err != nil {
				return
			}
			remaining -= take
		}
		err = mw.Close()
	}()

	req, err := http.NewRequest(http.MethodPost, t.base+"/uploads", pr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	resp, err := t.client.Do(req)
	if err != nil {
		if crash {
			return nil // connection severed by injected crash is expected
		}
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != wantStatus {
		return fmt.Errorf("stream upload %d bytes -> status=%d, want %d", size, resp.StatusCode, wantStatus)
	}
	return nil
}

func (t *v) failpointsInProcess() error {
	content := t.tagged("write-failpoint payload")
	idWrite := t.id("fp-write")

	// write: 500, nothing readable, temp cleaned, retry succeeds.
	status, _, err := t.doUpload(idWrite, "f", content, "write")
	if err != nil || status != http.StatusInternalServerError {
		return fmt.Errorf("failpoint write status=%d err=%v, want 500", status, err)
	}
	if status, _, _ := t.get(idWrite); status != http.StatusNotFound {
		return fmt.Errorf("blob readable after write failure: %d", status)
	}
	if n := countPartFiles(filepath.Join(t.data, "tmp")); n != 0 {
		return fmt.Errorf("temp files remain after write failure: %d", n)
	}

	// rename: 500, file on disk but no DB row (GET 404); orphan survives
	// until the next process restart.
	renameContent := t.tagged("rename-failpoint payload")
	idRename := t.id("fp-rename")
	status, _, err = t.doUpload(idRename, "f", renameContent, "rename")
	if err != nil || status != http.StatusInternalServerError {
		return fmt.Errorf("failpoint rename status=%d err=%v, want 500", status, err)
	}
	if status, _, _ := t.get(idRename); status != http.StatusNotFound {
		return fmt.Errorf("uncommitted renamed blob readable: %d", status)
	}
	orphan := filepath.Join(t.data, "blobs", shaOf(renameContent)[:2], shaOf(renameContent)[2:])
	if _, err := os.Stat(orphan); err != nil {
		return fmt.Errorf("expected orphan at %s after rename failure: %w", orphan, err)
	}

	// respond: commit is durable, GET 200 even though client saw 500.
	respondContent := t.tagged("respond-failpoint payload")
	idRespond := t.id("fp-respond")
	status, _, err = t.doUpload(idRespond, "f", respondContent, "respond")
	if err != nil || status != http.StatusInternalServerError {
		return fmt.Errorf("failpoint respond status=%d err=%v, want 500", status, err)
	}
	if status, got, _ := t.get(idRespond); status != http.StatusOK || !bytes.Equal(got, respondContent) {
		return fmt.Errorf("committed blob not readable after respond failure: %d", status)
	}
	// Retry without failpoint returns the original record.
	status, _, err = t.doUpload(idRespond, "f", respondContent, "")
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("post-failpoint retry status=%d err=%v, want 200", status, err)
	}

	// The write failure can be retried successfully now.
	status, _, err = t.doUpload(idWrite, "f", content, "")
	if err != nil || status != http.StatusCreated {
		return fmt.Errorf("write-failpoint retry status=%d err=%v, want 201", status, err)
	}
	return nil
}

// expectCrashUpload fires a crash failpoint; the request must fail at the
// transport level because the process exits before replying.
func (t *v) expectCrashUpload(id string, content []byte, fp string) error {
	_, _, err := t.doUpload(id, "f", content, fp)
	if err == nil {
		return fmt.Errorf("%s: request unexpectedly succeeded; process should have crashed", fp)
	}
	return nil
}

func (t *v) failpointsCrash() error {
	// rename-crash: orphan must be reaped on restart, retry then commits.
	renameCrashContent := t.tagged("rename-crash payload")
	idRenameCrash := t.id("fp-rename-crash")
	if err := t.expectCrashUpload(idRenameCrash, renameCrashContent, "rename-crash"); err != nil {
		return err
	}
	if err := t.waitHealthy(30 * time.Second); err != nil {
		return fmt.Errorf("after rename-crash: %w", err)
	}
	if status, _, _ := t.get(idRenameCrash); status != http.StatusNotFound {
		return fmt.Errorf("crashed rename produced readable blob: %d", status)
	}
	// Both the rename-crash orphan and the earlier in-process "rename"
	// orphan must be reaped by the same startup sweep.
	for _, content := range [][]byte{renameCrashContent, t.tagged("rename-failpoint payload")} {
		name := filepath.Join(t.data, "blobs", shaOf(content)[:2], shaOf(content)[2:])
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			return fmt.Errorf("orphan survived restart: %s (err=%v)", name, err)
		}
	}
	if n := countPartFiles(filepath.Join(t.data, "tmp")); n != 0 {
		return fmt.Errorf("temp files survive restart: %d", n)
	}
	if status, _, err := t.doUpload(idRenameCrash, "f", renameCrashContent, ""); err != nil || status != http.StatusCreated {
		return fmt.Errorf("retry after rename-crash status=%d err=%v, want 201", status, err)
	}

	// write-crash: staged temp file must be swept on restart.
	writeCrashContent := t.tagged("write-crash payload")
	idWriteCrash := t.id("fp-write-crash")
	if err := t.expectCrashUpload(idWriteCrash, writeCrashContent, "write-crash"); err != nil {
		return err
	}
	if err := t.waitHealthy(30 * time.Second); err != nil {
		return fmt.Errorf("after write-crash: %w", err)
	}
	if n := countPartFiles(filepath.Join(t.data, "tmp")); n != 0 {
		return fmt.Errorf("staged temp file survives restart: %d", n)
	}
	if status, _, _ := t.get(idWriteCrash); status != http.StatusNotFound {
		return fmt.Errorf("crashed write produced readable blob: %d", status)
	}
	if status, _, err := t.doUpload(idWriteCrash, "f", writeCrashContent, ""); err != nil || status != http.StatusCreated {
		return fmt.Errorf("retry after write-crash status=%d err=%v, want 201", status, err)
	}

	// respond-crash: the commit happened before exit; after restart the
	// blob is readable and a retry returns the original record (200).
	respondCrashContent := t.tagged("respond-crash payload")
	idRespondCrash := t.id("fp-respond-crash")
	if err := t.expectCrashUpload(idRespondCrash, respondCrashContent, "respond-crash"); err != nil {
		return err
	}
	if err := t.waitHealthy(30 * time.Second); err != nil {
		return fmt.Errorf("after respond-crash: %w", err)
	}
	if status, got, _ := t.get(idRespondCrash); status != http.StatusOK || !bytes.Equal(got, respondCrashContent) {
		return fmt.Errorf("committed blob lost across respond-crash restart: status=%d", status)
	}
	if status, _, err := t.doUpload(idRespondCrash, "f", respondCrashContent, ""); err != nil || status != http.StatusOK {
		return fmt.Errorf("retry after respond-crash status=%d err=%v, want 200", status, err)
	}
	return nil
}

func countPartFiles(dir string) int {
	n := 0
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			n++
		}
		return nil
	})
	return n
}

func (t *v) invariants() error {
	// No staging artifacts anywhere.
	if n := countPartFiles(filepath.Join(t.data, "tmp")); n != 0 {
		return fmt.Errorf("tmp area not empty at end: %d files", n)
	}

	// Count regular files under blobs/.
	onDisk := map[string]struct{}{}
	err := filepath.WalkDir(filepath.Join(t.data, "blobs"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			rel, rerr := filepath.Rel(filepath.Join(t.data, "blobs"), path)
			if rerr != nil {
				return rerr
			}
			onDisk[filepath.ToSlash(rel)] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk blobs: %w", err)
	}

	// Open the live database read-only and cross-check every record.
	dsn := "file:" + filepath.Join(t.data, "uploads.db") + "?mode=ro&_pragma=query_only(true)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open db ro: %w", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT request_id, sha256, size, stored_as FROM blobs`)
	if err != nil {
		return fmt.Errorf("query blobs: %w", err)
	}
	defer rows.Close()
	inDB := map[string]struct{}{}
	for rows.Next() {
		var id, sha, stored string
		var size int64
		if err := rows.Scan(&id, &sha, &size, &stored); err != nil {
			return err
		}
		inDB[stored] = struct{}{}
		// stored_as must be the hash-sharded name and the file must exist.
		want := sha[:2] + "/" + sha[2:]
		if stored != want {
			return fmt.Errorf("stored_as %q is not content-addressed (%q)", stored, want)
		}
		path := filepath.Join(t.data, "blobs", filepath.FromSlash(stored))
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("committed record %q missing file: %w", id, err)
		}
		if info.Size() != size {
			return fmt.Errorf("size mismatch for %q: db=%d disk=%d", id, size, info.Size())
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(onDisk) != len(inDB) {
		return fmt.Errorf("blob files on disk (%d) != distinct files referenced by db (%d): half-finished product visible", len(onDisk), len(inDB))
	}
	for k := range inDB {
		if _, ok := onDisk[k]; !ok {
			return fmt.Errorf("db record %q has no file", k)
		}
	}

	// Final sanity: every committed id from this run still downloads.
	for _, id := range []string{
		t.id("c-1"), t.id("c-empty"), t.id("c-alias-1"), t.id("c-alias-2"), t.id("r-exact"),
		t.id("fp-write"), t.id("fp-respond"), t.id("fp-rename-crash"), t.id("fp-write-crash"), t.id("fp-respond-crash"),
	} {
		if status, _, err := t.get(id); err != nil || status != http.StatusOK {
			return fmt.Errorf("final GET %s -> status=%d err=%v", id, status, err)
		}
	}
	return nil
}
