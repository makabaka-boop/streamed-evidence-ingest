// Command verify is the one-shot acceptance test for the upload service.
//
// It runs in two phases:
//
//	A. Black-box HTTP tests against BASE_URL (the compose "server" service):
//	   happy path, strict multipart/JSON validation, the 16 MiB boundary,
//	   truncation, idempotency, conflict and content retrieval.
//	B. Crash-injection tests against a server subprocess it owns:
//	   X-Upload-Fault crashes the process after the write, rename and
//	   database-commit stages. verify restarts the server, inspects the
//	   data directory directly and proves no half-finished upload is ever
//	   readable, while previously committed uploads survive.
//
// Exit code is 0 only when every check passes.
package main

import (
	"bytes"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

type verifier struct {
	baseURL string
	failed  int
	passed  int
	runID   string
}

func main() {
	baseURL := flag.String("base-url", envOr("BASE_URL", "http://server:8080"),
		"base URL of the upload service")
	serverBin := flag.String("server-bin", envOr("SERVER_BIN", "/usr/local/bin/server"),
		"server binary used for crash/restart tests")
	faultData := flag.String("fault-data", envOr("FAULT_DATA", "/faultdata"),
		"data directory for the crash-test server")
	faultAddr := flag.String("fault-addr", envOr("FAULT_ADDR", "127.0.0.1:18080"),
		"listen address for the crash-test server")
	flag.Parse()

	// Unique suffix keeps phase A re-runnable against a persistent volume:
	// fixed IDs would otherwise return 200 instead of 201 on the second run.
	rb := make([]byte, 4)
	if _, err := crand.Read(rb); err != nil {
		rb = []byte(fmt.Sprintf("%08x", time.Now().UnixNano()))
	}
	runID := fmt.Sprintf("%d-%x", time.Now().UnixNano(), rb)
	v := &verifier{baseURL: strings.TrimRight(*baseURL, "/"), runID: runID}

	fmt.Println("== phase A: black-box HTTP tests against", v.baseURL)
	v.waitReady(v.baseURL, 30*time.Second)
	v.phaseA()

	fmt.Println("== phase B: crash/restart tests")
	v.phaseB(*serverBin, *faultData, *faultAddr)

	fmt.Printf("\nverify: %d passed, %d failed\n", v.passed, v.failed)
	if v.failed > 0 {
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// Tiny assertion framework
// ---------------------------------------------------------------------------

func (v *verifier) ok(cond bool, format string, args ...any) {
	if cond {
		v.passed++
		fmt.Println("  PASS:", fmt.Sprintf(format, args...))
		return
	}
	v.failed++
	fmt.Println("  FAIL:", fmt.Sprintf(format, args...))
}

func (v *verifier) must(cond bool, format string, args ...any) {
	v.ok(cond, format, args...)
	if !cond {
		// Fatal for the current phase: panic is recovered per case group.
		panic(fatalCheck{fmt.Sprintf(format, args...)})
	}
}

type fatalCheck struct{ msg string }

func (v *verifier) run(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			if f, ok := r.(fatalCheck); ok {
				fmt.Printf("  FAIL(fatal): %s: %s\n", name, f.msg)
				v.failed++
				return
			}
			panic(r)
		}
	}()
	fn()
}

// uniq returns a per-run-unique request id. phase A runs against a persistent
// volume, so every case that expects a fresh 201 needs a fresh id.
func (v *verifier) uniq(prefix string) string {
	return prefix + "-" + v.runID
}

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

func (v *verifier) waitReady(base string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/healthz")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	v.must(false, "service at %s never became ready", base)
}

func doJSON(method, url string, body io.Reader, hdr http.Header) (int, map[string]any, []byte) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		panic(err)
	}
	for k, vs := range hdr {
		for _, x := range vs {
			req.Header.Add(k, x)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return -1, nil, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	return resp.StatusCode, parsed, raw
}

// multipartBody builds a complete, small multipart body.
func multipartBody(parts []formPart, extraTrailer []byte) ([]byte, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		h := make(map[string][]string)
		if p.filename != "" {
			h["Content-Disposition"] = []string{
				fmt.Sprintf(`form-data; name=%q; filename=%q`, p.name, p.filename)}
		} else {
			h["Content-Disposition"] = []string{
				fmt.Sprintf(`form-data; name=%q`, p.name)}
		}
		if p.contentType != "" {
			h["Content-Type"] = []string{p.contentType}
		}
		w, err := mw.CreatePart(h)
		if err != nil {
			panic(err)
		}
		_, _ = w.Write(p.value)
	}
	mw.Close()
	body := buf.Bytes()
	if extraTrailer != nil {
		body = append(body, extraTrailer...)
	}
	return body, mw.Boundary()
}

type formPart struct {
	name        string
	filename    string
	contentType string
	value       []byte
}

func metaJSON(id, contentType string) []byte {
	if contentType == "" {
		return []byte(fmt.Sprintf(`{"request_id":%q}`, id))
	}
	return []byte(fmt.Sprintf(`{"request_id":%q,"content_type":%q}`, id, contentType))
}

func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// streamingRequest builds a request whose file content is generated lazily so
// verify itself never materialises the (up to 16 MiB+) payload in memory.
func streamingRequest(id string, fileSize int64, contentType string) (*http.Request, int64) {
	boundary := "----verifyboundary42"
	meta := metaJSON(id, contentType)
	prefix := fmt.Sprintf(
		"--%s\r\nContent-Disposition: form-data; name=\"meta\"\r\n"+
			"Content-Type: application/json\r\n\r\n%s\r\n"+
			"--%s\r\nContent-Disposition: form-data; name=\"file\"; "+
			"filename=\"do-not-use-this-name.bin\"\r\nContent-Type: application/octet-stream\r\n\r\n",
		boundary, meta, boundary)
	suffix := fmt.Sprintf("\r\n--%s--\r\n", boundary)
	total := int64(len(prefix)) + fileSize + int64(len(suffix))
	body := io.MultiReader(
		strings.NewReader(prefix),
		io.LimitReader(zeroReader{}, fileSize),
		strings.NewReader(suffix),
	)
	req, err := http.NewRequest(http.MethodPost, "/uploads", body)
	if err != nil {
		panic(err)
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	req.ContentLength = total
	return req, total
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// postStreaming sends a generated-size upload to the service base URL.
func (v *verifier) postStreaming(id string, size int64, contentType string, hdr http.Header) (int, map[string]any) {
	req, _ := streamingRequest(id, size, contentType)
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(strings.TrimPrefix(v.baseURL, "http://"), "https://")
	for k, vs := range hdr {
		for _, x := range vs {
			req.Header.Add(k, x)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return -1, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	return resp.StatusCode, parsed
}

// ---------------------------------------------------------------------------
// Phase A
// ---------------------------------------------------------------------------

func (v *verifier) phaseA() {
	content := []byte("hello upload world\n")
	hash := shaHex(content)
	id := v.uniq("case-happy")

	v.run("happy path: 201 + correct record", func() {
		body, boundary := multipartBody([]formPart{
			{name: "meta", contentType: "application/json", value: metaJSON(id, "text/plain")},
			{name: "file", filename: "ignored.txt", contentType: "text/plain", value: content},
		}, nil)
		status, rec, _ := doJSON("POST", v.baseURL+"/uploads", bytes.NewReader(body),
			http.Header{"Content-Type": {"multipart/form-data; boundary=" + boundary}})
		v.must(status == 201, "expected 201, got %d", status)
		v.ok(rec["request_id"] == id, "request_id echoed")
		v.ok(rec["sha256"] == hash, "sha256 matches content")
		v.ok(int64(rec["size"].(float64)) == int64(len(content)), "size matches")
		v.ok(rec["content_type"] == "text/plain", "content_type stored")
	})

	v.run("metadata endpoint returns the committed record", func() {
		status, rec, _ := doJSON("GET", v.baseURL+"/uploads/"+id, nil, nil)
		v.must(status == 200, "expected 200, got %d", status)
		v.ok(rec["sha256"] == hash, "sha256 matches")
	})

	v.run("content endpoint serves identical bytes, no client filename", func() {
		resp, err := http.Get(v.baseURL + "/uploads/" + id + "/content")
		v.must(err == nil, "GET content: %v", err)
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		v.must(resp.StatusCode == 200, "expected 200, got %d", resp.StatusCode)
		v.ok(bytes.Equal(raw, content), "downloaded bytes equal upload")
		v.ok(resp.Header.Get("Content-Type") == "text/plain", "content type served")
		cd := resp.Header.Get("Content-Disposition")
		v.ok(!strings.Contains(cd, "ignored.txt"), "client filename never used (%q)", cd)
		v.ok(resp.Header.Get("ETag") == `"`+hash+`"`, "ETag is the sha256")
	})

	v.run("idempotent replay: same id + same content -> 200 original", func() {
		body, boundary := multipartBody([]formPart{
			{name: "meta", contentType: "application/json", value: metaJSON(id, "image/png")},
			{name: "file", filename: "other.png", value: content},
		}, nil)
		status, rec, _ := doJSON("POST", v.baseURL+"/uploads", bytes.NewReader(body),
			http.Header{"Content-Type": {"multipart/form-data; boundary=" + boundary}})
		v.must(status == 200, "expected 200, got %d", status)
		v.ok(rec["sha256"] == hash, "original hash returned")
		v.ok(rec["content_type"] == "text/plain", "original metadata preserved, got %v", rec["content_type"])
	})

	v.run("conflict: same id + different content -> 409", func() {
		body, boundary := multipartBody([]formPart{
			{name: "meta", value: metaJSON(id, "")},
			{name: "file", filename: "x", value: []byte("totally different bytes")},
		}, nil)
		status, _, _ := doJSON("POST", v.baseURL+"/uploads", bytes.NewReader(body),
			http.Header{"Content-Type": {"multipart/form-data; boundary=" + boundary}})
		v.ok(status == 409, "expected 409, got %d", status)
	})

	v.run("unknown id -> 404, unknown sub-resource -> 404", func() {
		status, _, _ := doJSON("GET", v.baseURL+"/uploads/no-such-id", nil, nil)
		v.ok(status == 404, "expected 404, got %d", status)
		status, _, _ = doJSON("GET", v.baseURL+"/uploads/"+id+"/nope", nil, nil)
		v.ok(status == 404, "expected 404 for unknown sub-resource, got %d", status)
	})

	v.run("wrong methods are rejected", func() {
		status, _, _ := doJSON("PUT", v.baseURL+"/uploads", nil, nil)
		v.ok(status == 405, "PUT /uploads -> %d", status)
		status, _, _ = doJSON("DELETE", v.baseURL+"/uploads/"+id, nil, nil)
		v.ok(status == 405, "DELETE -> %d", status)
	})

	v.run("non-multipart content type -> 415", func() {
		status, _, _ := doJSON("POST", v.baseURL+"/uploads",
			strings.NewReader("{}"), http.Header{"Content-Type": {"application/json"}})
		v.ok(status == 415, "expected 415, got %d", status)
	})

	v.run("metadata JSON: unknown field, duplicate key, bad JSON, missing id", func() {
		cases := [][]byte{
			[]byte(`{"request_id":"x","bogus":1}`),
			[]byte(`{"request_id":"x","request_id":"y"}`),
			[]byte(`{not json`),
			[]byte(`{"content_type":"text/plain"}`),
			[]byte(`[1,2,3]`),
			[]byte(`{"request_id":"x"} trailing`),
		}
		for i, m := range cases {
			body, boundary := multipartBody([]formPart{
				{name: "meta", value: m},
				{name: "file", filename: "f", value: []byte("abc")},
			}, nil)
			status, _, _ := doJSON("POST", v.baseURL+"/uploads", bytes.NewReader(body),
				http.Header{"Content-Type": {"multipart/form-data; boundary=" + boundary}})
			v.ok(status == 400, "case %d (%s): expected 400, got %d", i, m, status)
		}
	})

	v.run("multipart shape: missing/duplicate/unknown parts all fail", func() {
		// File only.
		body, boundary := multipartBody([]formPart{
			{name: "file", filename: "f", value: []byte("abc")},
		}, nil)
		status, _, _ := doJSON("POST", v.baseURL+"/uploads", bytes.NewReader(body),
			http.Header{"Content-Type": {"multipart/form-data; boundary=" + boundary}})
		v.ok(status == 400, "file-only: expected 400, got %d", status)

		// Meta only.
		body, boundary = multipartBody([]formPart{
			{name: "meta", value: metaJSON("meta-only", "")},
		}, nil)
		status, _, _ = doJSON("POST", v.baseURL+"/uploads", bytes.NewReader(body),
			http.Header{"Content-Type": {"multipart/form-data; boundary=" + boundary}})
		v.ok(status == 400, "meta-only: expected 400, got %d", status)

		// Duplicate meta.
		body, boundary = multipartBody([]formPart{
			{name: "meta", value: metaJSON("dup-meta", "")},
			{name: "meta", value: metaJSON("dup-meta2", "")},
			{name: "file", filename: "f", value: []byte("abc")},
		}, nil)
		status, _, _ = doJSON("POST", v.baseURL+"/uploads", bytes.NewReader(body),
			http.Header{"Content-Type": {"multipart/form-data; boundary=" + boundary}})
		v.ok(status == 400, "duplicate meta: expected 400, got %d", status)

		// Duplicate file.
		body, boundary = multipartBody([]formPart{
			{name: "meta", value: metaJSON("dup-file", "")},
			{name: "file", filename: "f", value: []byte("abc")},
			{name: "file", filename: "f", value: []byte("def")},
		}, nil)
		status, _, _ = doJSON("POST", v.baseURL+"/uploads", bytes.NewReader(body),
			http.Header{"Content-Type": {"multipart/form-data; boundary=" + boundary}})
		v.ok(status == 400, "duplicate file: expected 400, got %d", status)

		// Unknown field.
		body, boundary = multipartBody([]formPart{
			{name: "meta", value: metaJSON("unknown-field", "")},
			{name: "file", filename: "f", value: []byte("abc")},
			{name: "extra", value: []byte("nope")},
		}, nil)
		status, _, _ = doJSON("POST", v.baseURL+"/uploads", bytes.NewReader(body),
			http.Header{"Content-Type": {"multipart/form-data; boundary=" + boundary}})
		v.ok(status == 400, "unknown field: expected 400, got %d", status)

		// Empty file.
		body, boundary = multipartBody([]formPart{
			{name: "meta", value: metaJSON("empty-file", "")},
			{name: "file", filename: "f", value: []byte{}},
		}, nil)
		status, _, _ = doJSON("POST", v.baseURL+"/uploads", bytes.NewReader(body),
			http.Header{"Content-Type": {"multipart/form-data; boundary=" + boundary}})
		v.ok(status == 400, "empty file: expected 400, got %d", status)
	})

	v.run("size limit: exactly 16 MiB succeeds, one byte more fails", func() {
		const sixteenMiB = 16 << 20
		status, rec := v.postStreaming(v.uniq("exact-16mib"), sixteenMiB, "application/octet-stream", nil)
		v.must(status == 201, "16 MiB: expected 201, got %d", status)
		v.ok(int64(rec["size"].(float64)) == sixteenMiB, "size recorded as 16 MiB")
		// Hash of 16 MiB of zero bytes, computed incrementally so verify
		// itself does not allocate a second giant buffer.
		hasher := sha256.New()
		zeros := make([]byte, 1<<20)
		for i := 0; i < 16; i++ {
			hasher.Write(zeros)
		}
		v.ok(rec["sha256"] == hex.EncodeToString(hasher.Sum(nil)), "streamed sha256 correct")

		statusTooBig, _ := v.postStreaming(v.uniq("too-big"), sixteenMiB+1, "", nil)
		v.ok(statusTooBig == 413, "16 MiB+1: expected 413, got %d", statusTooBig)
	})

	v.run("truncated request (declared length, cut connection) fails entirely", func() {
		id := v.uniq("truncated")
		_, total := streamingRequest(id, 1<<20, "") // claims 1 MiB file
		host := strings.TrimPrefix(v.baseURL, "http://")
		conn, err := net.Dial("tcp", host)
		v.must(err == nil, "dial: %v", err)
		defer conn.Close()
		req, _ := streamingRequest(id, 1<<20, "")
		req.URL.Host = host
		// Send only the head + meta + a fraction of the file, then half-close.
		head := fmt.Sprintf("POST /uploads HTTP/1.1\r\nHost: %s\r\n"+
			"Content-Type: %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n",
			host, req.Header.Get("Content-Type"), total)
		_, _ = conn.Write([]byte(head))
		buf := make([]byte, 4096)
		body := req.Body
		var sent int64
		for sent < 8192 {
			n, _ := body.Read(buf)
			if n == 0 {
				break
			}
			_, werr := conn.Write(buf[:n])
			if werr != nil {
				break
			}
			sent += int64(n)
		}
		conn.(*net.TCPConn).CloseWrite()
		// Read whatever response comes back (may be empty on reset).
		resp, _ := io.ReadAll(conn)
		gotStatus := 0
		if len(resp) > 12 && strings.HasPrefix(string(resp[:12]), "HTTP/1.") {
			fmt.Sscanf(string(resp[9:12]), "%d", &gotStatus)
		}
		v.ok(gotStatus == 400 || gotStatus == 0, "truncation rejected (status=%d)", gotStatus)
		// The upload must not exist.
		deadline := time.Now().Add(3 * time.Second)
		var finalStatus int
		for time.Now().Before(deadline) {
			finalStatus, _, _ = doJSON("GET", v.baseURL+"/uploads/"+id, nil, nil)
			if finalStatus == 404 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		v.ok(finalStatus == 404, "truncated upload never became readable (status=%d)", finalStatus)
	})

	v.run("path traversal filename is harmless", func() {
		id := v.uniq("traversal")
		body, boundary := multipartBody([]formPart{
			{name: "meta", value: metaJSON(id, "text/plain")},
			{name: "file", filename: "../../../../etc/passwd",
				contentType: "text/plain", value: []byte("not really passwd\n")},
		}, nil)
		status, rec, _ := doJSON("POST", v.baseURL+"/uploads", bytes.NewReader(body),
			http.Header{"Content-Type": {"multipart/form-data; boundary=" + boundary}})
		v.must(status == 201, "expected 201, got %d", status)
		v.ok(rec["sha256"] == shaHex([]byte("not really passwd\n")),
			"stored under content hash, not filename")
	})
}

// ---------------------------------------------------------------------------
// Phase B: crash injection with a server the test owns
// ---------------------------------------------------------------------------

type crashServer struct {
	cmd    *exec.Cmd
	bin    string
	data   string
	addr   string
	base   string
	logOut *os.File
}

func (v *verifier) phaseB(bin, data, addr string) {
	v.must(fileExists(bin), "server binary %s available in verify image", bin)
	_ = os.RemoveAll(data)
	v.must(os.MkdirAll(data, 0o750) == nil, "create fault data dir")

	cs := &crashServer{bin: bin, data: data, addr: addr, base: "http://" + addr}
	if err := cs.start(); err != nil {
		v.must(false, "crash-test server starts: %v", err)
	}

	seed := []byte("committed before any crash - must survive")
	seedHash := shaHex(seed)
	v.run("seed a durable upload", func() {
		body, boundary := multipartBody([]formPart{
			{name: "meta", value: metaJSON("seed-durable", "text/plain")},
			{name: "file", filename: "seed.txt", value: seed},
		}, nil)
		status, rec, _ := doJSON("POST", cs.base+"/uploads", bytes.NewReader(body),
			http.Header{"Content-Type": {"multipart/form-data; boundary=" + boundary}})
		v.must(status == 201, "seed upload: expected 201, got %d", status)
		v.must(rec["sha256"] == seedHash, "seed hash")
	})

	crashCases := []struct {
		name      string
		id        string
		payload   []byte
		fault     string
		readable  bool
		keepBlobs int // expected total committed blob files after reconciliation
	}{
		{"crash after write (staged only)", "crash-write",
			[]byte("staged bytes never become visible"), "after_write", false, 1},
		{"crash after rename (orphan blob, no row)", "crash-rename",
			[]byte("renamed but not committed"), "after_rename", false, 1},
		{"crash after commit, before response (durable)", "crash-commit",
			[]byte("committed row and blob"), "after_commit", true, 2},
	}

	for _, tc := range crashCases {
		v.run(tc.name, func() {
			body, boundary := multipartBody([]formPart{
				{name: "meta", value: metaJSON(tc.id, "application/octet-stream")},
				{name: "file", filename: "evil.txt", value: tc.payload},
			}, nil)
			req, err := http.NewRequest("POST", cs.base+"/uploads", bytes.NewReader(body))
			v.must(err == nil, "build request: %v", err)
			req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
			req.Header.Set("X-Upload-Fault", tc.fault)
			resp, doErr := http.DefaultClient.Do(req)
			if resp != nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			v.ok(doErr != nil || resp == nil, "no response was sent (err=%v)", doErr)

			v.must(cs.waitExit(8*time.Second), "server process exited from injected fault")
			if err := cs.start(); err != nil {
				v.must(false, "server restarts after crash: %v", err)
			}

			// tmp/ must be empty after reconciliation.
			v.ok(len(listFiles(filepath.Join(cs.data, "tmp"))) == 0,
				"no staging files survive restart")

			// The crashed upload's visibility.
			status, rec, raw := doJSON("GET", cs.base+"/uploads/"+tc.id, nil, nil)
			if tc.readable {
				v.must(status == 200, "committed-before-crash upload readable, got %d: %s", status, raw)
				v.ok(rec["sha256"] == shaHex(tc.payload), "committed hash correct")
				status2, got := v.getContent(cs.base + "/uploads/" + tc.id + "/content")
				v.must(status2 == 200, "content fetch 200, got %d", status2)
				v.ok(bytes.Equal(got, tc.payload), "content bytes intact")
			} else {
				v.ok(status == 404, "half-finished upload not readable (status=%d)", status)
			}

			// Committed blobs: only the expected set, all content-addressed.
			blobs := listFiles(filepath.Join(cs.data, "blobs"))
			sort.Strings(blobs)
			v.ok(len(blobs) == tc.keepBlobs,
				"exactly %d committed blob(s) on disk, found %d: %v", tc.keepBlobs, len(blobs), blobs)
			for _, name := range blobs {
				// Full content hash is the 2-char shard plus 62-char leaf.
				full := strings.ReplaceAll(filepath.ToSlash(name), "/", "")
				v.ok(len(full) == 64 && isHex(full), "blob path is a sha256 hex, got %q", name)
			}

			// The pre-existing seed must never be damaged by cleanup.
			status, rec, _ = doJSON("GET", cs.base+"/uploads/seed-durable", nil, nil)
			v.must(status == 200, "seed record still present, got %d", status)
			v.ok(rec["sha256"] == seedHash, "seed hash unchanged")
			status2, got := v.getContent(cs.base + "/uploads/seed-durable/content")
			v.must(status2 == 200, "seed content fetch 200, got %d", status2)
			v.ok(bytes.Equal(got, seed), "seed bytes intact after cleanup")
		})
	}

	v.run("rejected upload leaves no staging file while server keeps running", func() {
		// Oversize request: 16 MiB + 1, streamed to the live server.
		before := listFiles(filepath.Join(cs.data, "tmp"))
		status, _ := v.postStreamingTo(cs.base, "oversize-live", 16<<20+1, "", nil)
		v.ok(status == 413, "oversize rejected with 413, got %d", status)
		after := listFiles(filepath.Join(cs.data, "tmp"))
		v.ok(len(before) == 0 && len(after) == 0, "staging dir empty after rejection: %v", after)

		// Duplicate-field request likewise leaves nothing staged.
		body, boundary := multipartBody([]formPart{
			{name: "meta", value: metaJSON("dup-live", "")},
			{name: "file", filename: "f", value: []byte("aaaa")},
			{name: "file", filename: "g", value: []byte("bbbb")},
		}, nil)
		status, _, _ = doJSON("POST", cs.base+"/uploads", bytes.NewReader(body),
			http.Header{"Content-Type": {"multipart/form-data; boundary=" + boundary}})
		v.ok(status == 400, "duplicate file rejected, got %d", status)
		v.ok(len(listFiles(filepath.Join(cs.data, "tmp"))) == 0, "no staging file left")
	})

	cs.stop()
}

func (v *verifier) getContent(url string) (int, []byte) {
	resp, err := http.Get(url)
	if err != nil {
		return -1, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func (v *verifier) postStreamingTo(base, id string, size int64, ct string, hdr http.Header) (int, map[string]any) {
	saved := v.baseURL
	v.baseURL = base
	defer func() { v.baseURL = saved }()
	return v.postStreaming(id, size, ct, hdr)
}

func (c *crashServer) start() error {
	logPath := filepath.Join(c.data, "server.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	cmd := exec.Command(c.bin, "-addr", c.addr, "-data", c.data, "-faults")
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		f.Close()
		return err
	}
	c.cmd = cmd
	c.logOut = f

	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(c.base + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("crash-test server at %s did not become ready", c.base)
}

func (c *crashServer) waitExit(timeout time.Duration) bool {
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case <-done:
		if c.logOut != nil {
			c.logOut.Close()
		}
		c.cmd = nil
		return true
	case <-time.After(timeout):
		return false
	}
}

func (c *crashServer) stop() {
	if c.cmd == nil || c.cmd.Process == nil {
		return
	}
	_ = c.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
	}
	if c.logOut != nil {
		c.logOut.Close()
	}
}

// ---------------------------------------------------------------------------
// small utilities
// ---------------------------------------------------------------------------

func listFiles(root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if filepath.Base(path) == "server.log" {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		out = append(out, rel)
		return nil
	})
	return out
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

func envOr(k, def string) string {
	if x := os.Getenv(k); x != "" {
		return x
	}
	return def
}
