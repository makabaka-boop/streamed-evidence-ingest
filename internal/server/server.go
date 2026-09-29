// Package server wires the strict multipart parser, the streaming blob
// store and the SQLite metadata table into an HTTP handler.
//
// Publishing order is deliberate:
//
//  1. stream bytes to a private temp file while hashing them;
//  2. fsync and atomically rename into the content-addressed blob area;
//  3. insert the metadata row in a transaction;
//  4. only then send the success response.
//
// Before step 3 the file is not readable through the API (GET is gated on
// the database row). A failure at any gap leaves state matching a crash at
// that instant; abandoned files are reaped at startup without touching
// committed files.
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"uploadsvc/internal/blobstore"
	"uploadsvc/internal/metadata"
	"uploadsvc/internal/multipartx"
	"uploadsvc/internal/store"
)

const (
	// MaxFileBytes caps a single upload at 16 MiB.
	MaxFileBytes = 16 << 20
	// MaxMetadataBytes caps the JSON metadata part at 1 MiB.
	MaxMetadataBytes = 1 << 20
	// maxEnvelope allows multipart framing overhead (boundaries, part
	// headers) on top of the largest legal body before the outer guard
	// rejects the request.
	maxEnvelope = MaxFileBytes + MaxMetadataBytes + (1 << 20)

	// failpointHeader selects an injected failure for one request.
	FailpointHeader = "X-Upload-Failpoint"
)

// IsCrashFailpoint reports whether name terminates the process.
func IsCrashFailpoint(name string) bool {
	switch name {
	case FailWriteCrash, FailRenameCrash, FailCommitCrash, FailRespondCrash:
		return true
	}
	return false
}

// Failpoint names accepted by the Hooks and the X-Upload-Failpoint header.
// The plain names fail the request in-process (HTTP 500). The *-crash
// variants terminate the process before the next step, exercising the
// real restart-recovery path.
const (
	FailWrite        = "write"        // after bytes are streamed, before rename
	FailWriteCrash   = "write-crash"  // crash in the same gap
	FailRename       = "rename"       // after filesystem publish, before DB commit
	FailRenameCrash  = "rename-crash" // crash in the same gap
	FailCommit       = "commit"       // alias of rename
	FailCommitCrash  = "commit-crash" // alias of rename-crash
	FailRespond      = "respond"      // after DB commit, before success response
	FailRespondCrash = "respond-crash"
)

// Hooks inject failures around the publish steps. A hook returning an
// error aborts the request with HTTP 500 exactly as if the process had
// crashed at that instant:
//
//   - AfterWrite:  the staged temp file is removed, nothing is published;
//   - AfterRename: the blob exists without a DB row (startup reaps it,
//     and GET cannot read it meanwhile);
//   - AfterRespond: the commit is already durable; only the client sees a
//     failure, and a retry returns the same record.
type Hooks struct {
	AfterWrite   func(r *http.Request, requestID string) error
	AfterRename  func(r *http.Request, requestID, storedAs string) error
	AfterRespond func(r *http.Request, rec *store.Record) error
}

// Server holds the dependencies.
type Server struct {
	blobs        *blobstore.BlobStore
	db           *store.Store
	hooks        Hooks
	failpointsOn bool
	log          *log.Logger
}

// Option customises a Server.
type Option func(*Server)

// WithHooks installs failure hooks.
func WithHooks(h Hooks) Option {
	return func(s *Server) { s.hooks = h }
}

// WithHTTPFailpoints lets clients trigger hooks through the
// X-Upload-Failpoint header. Enable only for tests.
func WithHTTPFailpoints() Option {
	return func(s *Server) { s.failpointsOn = true }
}

// New constructs a Server.
func New(b *blobstore.BlobStore, db *store.Store, logger *log.Logger, opts ...Option) *Server {
	if logger == nil {
		logger = log.Default()
	}
	s := &Server{blobs: b, db: db, log: logger}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Routes registers the HTTP routes.
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /uploads", s.handleUpload)
	mux.HandleFunc("GET /blobs/{id}", s.handleGet)
	mux.HandleFunc("HEAD /blobs/{id}", s.handleGet)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}

type response struct {
	RequestID string `json:"request_id"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	MediaType string `json:"media_type"`
	CreatedAt string `json:"created_at"`
	Download  string `json:"download"`
}

type errorBody struct {
	Error string `json:"error"`
}

func (s *Server) writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: msg})
}

// fireHook runs a hook. Programmatic hooks (used by tests) run on every
// request. HTTP-header failpoints run only in test mode, when the header
// names one of the stages supplied for this point.
func (s *Server) fireHook(r *http.Request, names []string, fn func() error) error {
	if s.failpointsOn {
		want := r.Header.Get(FailpointHeader)
		matched := false
		for _, n := range names {
			if want == n {
				matched = true
				break
			}
		}
		if !matched {
			return nil
		}
	}
	return fn()
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	boundary, err := multipartx.Boundary(r.Header.Get("Content-Type"))
	if err != nil {
		s.failMultipart(w, err)
		return
	}

	// Outer guard on the entire wire body including multipart framing.
	body := http.MaxBytesReader(w, r.Body, maxEnvelope)
	mr, err := multipartx.NewReader(body, boundary,
		multipartx.Options{MaxFileBytes: MaxFileBytes, MaxMetaBytes: MaxMetadataBytes})
	if err != nil {
		s.failMultipart(w, err)
		return
	}

	rawMeta, _, err := mr.ReadMetadata()
	if err != nil {
		s.failMultipart(w, err)
		return
	}
	meta, err := metadata.Decode(rawMeta)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	part, clientFilename, err := mr.FilePart()
	if err != nil {
		s.failMultipart(w, err)
		return
	}
	// The client-supplied filename is intentionally ignored entirely: it is
	// never logged, stored or used in any filesystem path.
	_ = clientFilename

	mediaType := strings.TrimSpace(meta.MediaType)
	if mediaType == "" {
		if ct := strings.TrimSpace(part.Header.Get("Content-Type")); ct != "" {
			mediaType = ct
		} else {
			mediaType = "application/octet-stream"
		}
	}

	sess, err := s.blobs.NewSession()
	if err != nil {
		s.log.Printf("temp session: %v", err)
		s.writeError(w, http.StatusInternalServerError, "could not open staging file")
		return
	}

	// Step 1: stream to the private temp file while hashing. Memory stays
	// flat: 32 KiB chunks go straight to disk and through the hasher.
	hasher := sha256.New()
	n, err := mr.StreamFile(part, io.MultiWriter(sess.File(), hasher))
	if err != nil {
		sess.Abort()
		s.failMultipart(w, err)
		return
	}
	// Validate the multipart trailer (closing boundary / extra fields) only
	// after the bytes are consumed; a malformed trailer fails the request.
	if err := mr.Finish(part); err != nil {
		sess.Abort()
		s.failMultipart(w, err)
		return
	}

	if s.hooks.AfterWrite != nil {
		if err := s.fireHook(r, []string{FailWrite, FailWriteCrash}, func() error {
			return s.hooks.AfterWrite(r, meta.RequestID)
		}); err != nil {
			sess.Abort()
			s.log.Printf("failpoint write: %v", err)
			s.writeError(w, http.StatusInternalServerError, "injected failure after write")
			return
		}
	}

	if err := sess.Sync(); err != nil {
		sess.Abort()
		s.log.Printf("sync: %v", err)
		s.writeError(w, http.StatusInternalServerError, "could not persist staging file")
		return
	}

	// Step 2: atomic filesystem publish, named by content hash only.
	sha := hex.EncodeToString(hasher.Sum(nil))
	storedAs, err := sess.Finalize(sha)
	if err != nil {
		sess.Abort()
		s.log.Printf("finalize: %v", err)
		s.writeError(w, http.StatusInternalServerError, "could not publish blob")
		return
	}

	if s.hooks.AfterRename != nil {
		if err := s.fireHook(r, []string{FailRename, FailRenameCrash, FailCommit, FailCommitCrash}, func() error {
			return s.hooks.AfterRename(r, meta.RequestID, storedAs)
		}); err != nil {
			// Simulated post-rename crash: leave the file exactly where the
			// rename put it. It has no DB row, so GET cannot see it; the
			// next startup recovery reaps it.
			s.log.Printf("failpoint rename: %v", err)
			s.writeError(w, http.StatusInternalServerError, "injected failure after rename")
			return
		}
	}

	// Step 3: metadata commit is the visibility gate.
	rec, created, err := s.db.Commit(meta.RequestID, sha, n, mediaType, storedAs)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			// Same request id, different content. Do not delete any blob on
			// a live request; this file is an orphan until startup recovery.
			s.writeError(w, http.StatusConflict,
				"request_id already used with different content")
			return
		}
		s.log.Printf("commit: %v", err)
		s.writeError(w, http.StatusInternalServerError, "could not commit upload")
		return
	}

	if s.hooks.AfterRespond != nil {
		if err := s.fireHook(r, []string{FailRespond, FailRespondCrash}, func() error {
			return s.hooks.AfterRespond(r, rec)
		}); err != nil {
			s.log.Printf("failpoint respond: %v", err)
			s.writeError(w, http.StatusInternalServerError, "injected failure before response")
			return
		}
	}

	// Step 4: success. A brand-new blob is 201; an idempotent replay with
	// identical content is 200 and returns the original record.
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response{
		RequestID: rec.RequestID,
		SHA256:    rec.SHA256,
		Size:      rec.Size,
		MediaType: rec.MediaType,
		CreatedAt: rec.CreatedAt,
		Download:  "/blobs/" + rec.RequestID,
	})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec, err := s.db.Lookup(id)
	if err != nil {
		s.log.Printf("lookup: %v", err)
		s.writeError(w, http.StatusInternalServerError, "could not read metadata")
		return
	}
	if rec == nil {
		// Uncommitted blobs are indistinguishable from ids that never
		// existed: the database row is the sole read gate.
		s.writeError(w, http.StatusNotFound, "not found")
		return
	}
	f, err := os.Open(s.blobs.Path(rec.StoredAs))
	if err != nil {
		s.log.Printf("open blob: %v", err)
		s.writeError(w, http.StatusInternalServerError, "stored file unavailable")
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", rec.MediaType)
	w.Header().Set("X-Content-SHA256", rec.SHA256)
	http.ServeContent(w, r, id, time.Time{}, f)
}

func (s *Server) failMultipart(w http.ResponseWriter, err error) {
	var me *multipartx.Error
	if errors.As(err, &me) {
		switch me.Kind {
		case multipartx.KindFileTooLarge, multipartx.KindMetadataTooLarge:
			s.writeError(w, http.StatusRequestEntityTooLarge, me.Msg)
			return
		case multipartx.KindDuplicateField, multipartx.KindUnknownField,
			multipartx.KindMalformed, multipartx.KindTruncated:
			s.writeError(w, http.StatusBadRequest, me.Msg)
			return
		}
	}
	// Outer request-body cap hit.
	var mbErr *http.MaxBytesError
	if errors.As(err, &mbErr) {
		s.writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	s.writeError(w, http.StatusBadRequest, err.Error())
}
