// Command uploadsrv runs the upload service. On startup it removes files
// left behind by crashed/aborted pre-commit attempts, never files that
// already have a database record.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"uploadsvc/internal/blobstore"
	"uploadsvc/internal/server"
	"uploadsvc/internal/store"
)

func main() {
	logger := log.New(os.Stdout, "uploadsrv ", log.LstdFlags|log.Lmsgprefix)

	dataDir := envOr("UPLOAD_DATA_DIR", "/data")
	addr := envOr("UPLOAD_ADDR", ":8080")
	dbPath := envOr("UPLOAD_DB", filepath.Join(dataDir, "uploads.db"))
	flag.Parse()

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		logger.Fatalf("create data dir: %v", err)
	}

	blobs, err := blobstore.New(dataDir)
	if err != nil {
		logger.Fatalf("blob store: %v", err)
	}

	// Open metadata first: recovery must know exactly which on-disk files
	// are committed before deleting anything.
	db, err := store.Open(dbPath)
	if err != nil {
		logger.Fatalf("open database: %v", err)
	}
	defer db.Close()

	if err := recoverTemp(logger, blobs, db); err != nil {
		logger.Fatalf("startup recovery: %v", err)
	}

	var opts []server.Option
	if os.Getenv("UPLOAD_ENABLE_FAILPOINTS") != "" {
		opts = append(opts, server.WithHTTPFailpoints(), server.WithHooks(crashHooks(logger)))
		logger.Print("HTTP failpoints enabled via X-Upload-Failpoint (test mode)")
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           server.New(blobs, db, logger, opts...).Routes(),
		ReadHeaderTimeout: 30 * time.Second,
		// No ReadTimeout: a legitimately slow upload of 16 MiB must not be
		// cut off; size limits bound the work instead.
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		logger.Printf("listening on %s (data=%s)", addr, dataDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("http: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	logger.Print("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Printf("shutdown: %v", err)
	}
}

// recoverTemp removes pre-commit leftovers while preserving committed
// blobs. It runs before the listener starts accepting requests.
func recoverTemp(logger *log.Logger, blobs *blobstore.BlobStore, db *store.Store) error {
	committed, err := db.AllStored()
	if err != nil {
		return err
	}
	tmpRemoved, orphanRemoved, err := blobs.Recover(committed)
	if err != nil {
		return err
	}
	if tmpRemoved+orphanRemoved > 0 {
		logger.Printf("startup recovery removed %d staged and %d uncommitted file(s)",
			tmpRemoved, orphanRemoved)
	} else {
		logger.Print("startup recovery: no leftover files")
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// crashHooks terminates the process immediately for the *-crash failpoint
// names, simulating a kill -9 at that exact point in the publish sequence.
// The restart policy brings the container back and startup recovery runs.
// Plain failpoint names without a matching crash request return an error,
// which the handler surfaces as HTTP 500.
func crashHooks(logger *log.Logger) server.Hooks {
	crash := func(r *http.Request, where string) error {
		if server.IsCrashFailpoint(r.Header.Get(server.FailpointHeader)) {
			logger.Printf("CRASH injected: exiting at %s", where)
			os.Exit(2)
		}
		return errors.New("injected failure at " + where)
	}
	return server.Hooks{
		AfterWrite: func(r *http.Request, requestID string) error {
			return crash(r, "write")
		},
		AfterRename: func(r *http.Request, requestID, storedAs string) error {
			return crash(r, "rename")
		},
		AfterRespond: func(r *http.Request, rec *store.Record) error {
			return crash(r, "respond")
		},
	}
}
