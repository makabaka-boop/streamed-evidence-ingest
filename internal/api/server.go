// Package api implements the HTTP surface of the upload service.
package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"

	"uploadsvc/internal/meta"
	"uploadsvc/internal/storage"
)

// maxRequestBody caps the whole multipart body: 16 MiB of file plus room for
// JSON metadata and multipart framing.
const maxRequestBody = storage.MaxFileSize + 2<<20

// copyBufferSize keeps per-request memory small and independent of file size.
const copyBufferSize = 32 << 10

// Server wires the store to HTTP handlers.
type Server struct {
	store         *storage.Store
	faultsEnabled bool
	log           *log.Logger
}

// NewServer builds a Server. When faultsEnabled is true the X-Upload-Fault
// header may crash the process at chosen pipeline stages (used by verify).
func NewServer(store *storage.Store, faultsEnabled bool, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.New(os.Stderr, "upload: ", log.LstdFlags)
	}
	return &Server{store: store, faultsEnabled: faultsEnabled, log: logger}
}

// Routes returns the root handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/uploads", s.handleUploads)
	mux.HandleFunc("/uploads/", s.handleUploadByID)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// hooks are failure-injection points between pipeline stages. Production
// requests use a zero value (all nil).
type hooks struct {
	afterWrite  func()
	afterRename func()
	afterCommit func()
}

func crash(stage string) func() {
	return func() {
		// Exit without defers: simulates a hard crash exactly between
		// stages, leaving whatever was already persisted on disk.
		log.Printf("FAULT injected: crashing %s", stage)
		_ = os.Stderr.Sync()
		os.Exit(3)
	}
}

// faultHooks parses the X-Upload-Fault header.
func (s *Server) faultHooks(r *http.Request) (hooks, *apiError) {
	value := strings.TrimSpace(r.Header.Get("X-Upload-Fault"))
	if value == "" {
		return hooks{}, nil
	}
	if !s.faultsEnabled {
		// The header is ignored entirely unless fault injection is enabled.
		return hooks{}, nil
	}
	h := hooks{}
	switch value {
	case "after_write":
		h.afterWrite = crash("after staging file was written")
	case "after_rename":
		h.afterRename = crash("after blob rename, before db commit")
	case "after_commit":
		h.afterCommit = crash("after db commit, before response")
	default:
		return hooks{}, &apiError{http.StatusBadRequest,
			fmt.Sprintf("unknown fault point %q", value)}
	}
	return h, nil
}

func (s *Server) handleUploads(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	hooks, aerr := s.faultHooks(r)
	if aerr != nil {
		writeAPIError(w, aerr)
		return
	}

	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		writeError(w, http.StatusUnsupportedMediaType, "expected multipart/form-data")
		return
	}
	boundary := params["boundary"]
	if boundary == "" {
		writeError(w, http.StatusBadRequest, "missing multipart boundary")
		return
	}

	// A chunked request with no length makes truncation indistinguishable
	// from a complete body, so it is not accepted for uploads.
	if r.ContentLength < 0 {
		writeError(w, http.StatusLengthRequired, "Content-Length is required")
		return
	}
	if r.ContentLength > maxRequestBody {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}

	counter := &countingReader{r: r.Body}
	limited := http.MaxBytesReader(w, io.NopCloser(counter), maxRequestBody)
	mr := multipartNewReader(limited, boundary)

	// ---- Part 1: exactly one "meta" field, JSON, first in order. ----
	part, aerr := nextPart(mr)
	if aerr != nil {
		writeAPIError(w, aerr)
		return
	}
	if part.FormName() != "meta" {
		part.Close()
		writeError(w, http.StatusBadRequest,
			`first part must be the JSON metadata field named "meta"`)
		return
	}
	if !meta.HasJSONContentType(part.Header.Get("Content-Type")) {
		part.Close()
		writeError(w, http.StatusBadRequest, "meta part must be application/json")
		return
	}
	metaBytes, aerr := readMeta(part)
	part.Close()
	if aerr != nil {
		writeAPIError(w, aerr)
		return
	}
	md, err := meta.Parse(bytesReader(metaBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	existing, lookupErr := s.store.Get(r.Context(), md.RequestID)
	idReused := lookupErr == nil
	if lookupErr != nil && !errors.Is(lookupErr, storage.ErrNotFound) {
		s.log.Printf("lookup: %v", lookupErr)
		writeError(w, http.StatusInternalServerError, "storage lookup failed")
		return
	}

	// ---- Part 2: exactly one "file" field, no repeated/unknown fields. ----
	part, aerr = nextPart(mr)
	if aerr != nil {
		writeAPIError(w, aerr)
		return
	}
	switch part.FormName() {
	case "meta":
		part.Close()
		writeError(w, http.StatusBadRequest, `duplicate field "meta"`)
		return
	case "file":
	default:
		name := part.FormName()
		part.Close()
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown field %q", name))
		return
	}

	// ---- Stream the file, hashing as it arrives. It is never fully buffered. ----
	if idReused {
		// Idempotent replay: still stream and hash to prove the payload is
		// identical, but do not persist anything.
		sum, n, aerr := drainAndHash(part)
		part.Close()
		if aerr != nil {
			writeAPIError(w, aerr)
			return
		}
		if n == 0 {
			writeError(w, http.StatusBadRequest, "uploaded file is empty")
			return
		}
		if aerr := framingComplete(mr, limited, counter, r.ContentLength); aerr != nil {
			writeAPIError(w, aerr)
			return
		}
		if sum != existing.SHA256 || n != existing.Size {
			writeError(w, http.StatusConflict,
				"request_id was already committed with different content")
			return
		}
		writeRecord(w, http.StatusOK, existing)
		return
	}

	staged, err := s.store.BeginStage()
	if err != nil {
		s.log.Printf("begin stage: %v", err)
		writeError(w, http.StatusInternalServerError, "could not open staging file")
		return
	}
	aborted := false
	abort := func() {
		if !aborted {
			staged.Abort()
			aborted = true
		}
	}

	n, copyErr := copyToStage(staged, part)
	size := n
	if copyErr != nil {
		part.Close()
		abort()
		if errors.Is(copyErr, storage.ErrTooLarge) || isMaxBytes(copyErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "file exceeds 16 MiB limit")
			return
		}
		// io.ErrUnexpectedEOF: part/body truncated mid-stream.
		writeError(w, http.StatusBadRequest, "truncated or malformed multipart body")
		return
	}
	if size == 0 {
		part.Close()
		abort()
		writeError(w, http.StatusBadRequest, "uploaded file is empty")
		return
	}
	part.Close()
	hashHex := staged.Hash()

	// ---- A third part must not exist, and framing must be fully present. ----
	if aerr := framingComplete(mr, limited, counter, r.ContentLength); aerr != nil {
		abort()
		writeAPIError(w, aerr)
		return
	}

	// All validation passed. From here the staged bytes become durable.
	if hooks.afterWrite != nil {
		hooks.afterWrite()
	}

	if _, err := staged.Commit(); err != nil {
		abort()
		s.log.Printf("commit stage: %v", err)
		writeError(w, http.StatusInternalServerError, "could not commit staged file")
		return
	}
	aborted = true // Commit closed and renamed the descriptor.

	if hooks.afterRename != nil {
		hooks.afterRename()
	}

	rec := storage.Record{
		RequestID:   md.RequestID,
		SHA256:      hashHex,
		Size:        size,
		ContentType: md.ContentType,
		CreatedAt:   time.Now(),
	}
	if err := s.store.InsertRecord(rec); err != nil {
		if errors.Is(err, storage.ErrAlreadyExists) {
			// Lost a race with a concurrent upload of the same request_id.
			winner, gerr := s.store.Get(r.Context(), rec.RequestID)
			if gerr == nil {
				if winner.SHA256 == rec.SHA256 && winner.Size == rec.Size {
					writeRecord(w, http.StatusOK, winner)
					return
				}
				// Different content: the blob we just renamed references no
				// row, so remove it now instead of leaving it for reconcile.
				s.removeOrphan(hashHex)
				writeError(w, http.StatusConflict,
					"request_id was already committed with different content")
				return
			}
		}
		// Row missing while blob exists: remove this uncommitted blob; the
		// startup reconciler would do the same. Fail visibly.
		s.removeOrphan(hashHex)
		s.log.Printf("insert record: %v", err)
		writeError(w, http.StatusInternalServerError, "could not store record")
		return
	}

	if hooks.afterCommit != nil {
		hooks.afterCommit()
	}

	s.log.Printf("committed request_id=%s sha256=%s size=%d", rec.RequestID, rec.SHA256, rec.Size)
	writeRecord(w, http.StatusCreated, rec)
}

// removeOrphan deletes a renamed blob only if no committed row references it.
// It is best-effort; the startup reconciler guarantees eventual cleanup.
func (s *Server) removeOrphan(hashHex string) {
	if hashHex == "" {
		return
	}
	var n int
	if err := s.store.DB().QueryRow(
		`SELECT COUNT(*) FROM uploads WHERE sha256 = ?`, hashHex).Scan(&n); err != nil || n > 0 {
		return
	}
	if err := os.Remove(s.store.BlobPath(hashHex)); err != nil && !os.IsNotExist(err) {
		s.log.Printf("remove orphan blob %s: %v", hashHex, err)
	}
}

// nextPart wraps multipart.Reader.NextPart, translating transport errors:
// max-body errors become 413, framing/truncation errors become 400.
func nextPart(mr *multipartReaderT) (*multipartPartT, *apiError) {
	p, err := mr.NextPart()
	if err == io.EOF {
		return nil, &apiError{http.StatusBadRequest, "expected another multipart part"}
	}
	if err != nil {
		if isMaxBytes(err) {
			return nil, &apiError{http.StatusRequestEntityTooLarge, "request body too large"}
		}
		return nil, &apiError{http.StatusBadRequest, "malformed multipart body"}
	}
	return p, nil
}

// framingComplete verifies there are no extra parts and that the entire
// declared body was consumed (truncation/trailing-garbage detection).
func framingComplete(mr *multipartReaderT, body io.Reader, counter *countingReader, declared int64) *apiError {
	p, err := mr.NextPart()
	if err == nil {
		name := p.FormName()
		p.Close()
		return &apiError{http.StatusBadRequest, fmt.Sprintf("unexpected extra field %q", name)}
	}
	if err != io.EOF {
		if isMaxBytes(err) {
			return &apiError{http.StatusRequestEntityTooLarge, "request body too large"}
		}
		return &apiError{http.StatusBadRequest, "malformed multipart body"}
	}
	// Drain anything still buffered so truncation is visible as a short read.
	if _, err := io.Copy(io.Discard, body); err != nil {
		if isMaxBytes(err) {
			return &apiError{http.StatusRequestEntityTooLarge, "request body too large"}
		}
		return &apiError{http.StatusBadRequest, "malformed multipart body"}
	}
	if declared >= 0 && counter.n != declared {
		return &apiError{http.StatusBadRequest,
			"truncated request: fewer bytes received than Content-Length"}
	}
	return nil
}

func (s *Server) handleUploadByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/uploads/")
	parts := strings.Split(rest, "/")
	var id, sub string
	switch len(parts) {
	case 1:
		id = parts[0]
	case 2:
		id, sub = parts[0], parts[1]
	default:
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if id == "" || (sub != "" && sub != "content") {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	rec, err := s.store.Get(r.Context(), id)
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "upload not found")
		return
	}
	if err != nil {
		s.log.Printf("lookup: %v", err)
		writeError(w, http.StatusInternalServerError, "storage lookup failed")
		return
	}

	if sub == "" {
		writeRecord(w, http.StatusOK, rec)
		return
	}

	f, _, err := s.store.OpenBlob(rec)
	if err != nil {
		s.log.Printf("open blob %s: %v", rec.SHA256, err)
		writeError(w, http.StatusInternalServerError, "stored content unavailable")
		return
	}
	defer f.Close()

	ct := rec.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	// Never derive a path or name from client input: attachment without a
	// filename. The identifier comes from metadata, not the upload filename.
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("ETag", `"`+rec.SHA256+`"`)
	http.ServeContent(w, r, "download", rec.CreatedAt, f)
}

// readMeta reads the meta part capped at meta.MaxBytes. One extra byte means
// the part is too large.
func readMeta(p *multipartPartT) ([]byte, *apiError) {
	lr := io.LimitReader(p, meta.MaxBytes+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		if isMaxBytes(err) {
			return nil, &apiError{http.StatusRequestEntityTooLarge, "metadata part too large"}
		}
		return nil, &apiError{http.StatusBadRequest, "cannot read metadata part"}
	}
	if int64(len(data)) > meta.MaxBytes {
		return nil, &apiError{http.StatusRequestEntityTooLarge, "metadata part too large"}
	}
	return data, nil
}

// drainAndHash streams a replay payload while hashing, enforcing the 16 MiB
// cap. The file never resides in memory.
func drainAndHash(p *multipartPartT) (string, int64, *apiError) {
	h := sha256.New()
	lr := io.LimitReader(p, storage.MaxFileSize+1)
	buf := make([]byte, copyBufferSize)
	var n int64
	for {
		m, rerr := lr.Read(buf)
		if m > 0 {
			_, _ = h.Write(buf[:m])
			n += int64(m)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if isMaxBytes(rerr) {
				return "", n, &apiError{http.StatusRequestEntityTooLarge, "file exceeds 16 MiB limit"}
			}
			return "", n, &apiError{http.StatusBadRequest, "truncated or malformed file part"}
		}
	}
	if n > storage.MaxFileSize {
		return "", n, &apiError{http.StatusRequestEntityTooLarge, "file exceeds 16 MiB limit"}
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// copyToStage streams a part into the staging file through a fixed-size
// buffer. The SHA-256 is updated inside StagedFile.Write.
func copyToStage(staged *storage.StagedFile, p *multipartPartT) (int64, error) {
	buf := make([]byte, copyBufferSize)
	var total int64
	for {
		m, rerr := p.Read(buf)
		if m > 0 {
			if _, werr := staged.Write(buf[:m]); werr != nil {
				return total, werr
			}
			total += int64(m)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return total, rerr
		}
	}
	return total, nil
}

// countingReader counts bytes pulled from the request body.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

type apiError struct {
	status int
	msg    string
}

func writeAPIError(w http.ResponseWriter, e *apiError) { writeError(w, e.status, e.msg) }

func writeError(w http.ResponseWriter, status int, msg string) {
	// Mid-upload failures can leave the client still sending; make teardown
	// explicit so no half response is ever reused.
	w.Header().Set("Connection", "close")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

type recordResponse struct {
	RequestID   string    `json:"request_id"`
	SHA256      string    `json:"sha256"`
	Size        int64     `json:"size"`
	ContentType string    `json:"content_type"`
	CreatedAt   time.Time `json:"created_at"`
}

func writeRecord(w http.ResponseWriter, status int, rec storage.Record) {
	ct := rec.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(recordResponse{
		RequestID:   rec.RequestID,
		SHA256:      rec.SHA256,
		Size:        rec.Size,
		ContentType: ct,
		CreatedAt:   rec.CreatedAt.UTC(),
	})
}
