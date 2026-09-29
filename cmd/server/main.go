// Command server runs the upload HTTP service.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"uploadsvc/internal/api"
	"uploadsvc/internal/storage"
)

func main() {
	ping := flag.Bool("ping", false, "probe -addr health endpoint and exit")
	addr := flag.String("addr", envOr("UPLOAD_ADDR", ":8080"), "listen address")
	dataDir := flag.String("data", envOr("UPLOAD_DATA", "/data"), "data directory")
	faults := flag.Bool("faults", envOr("UPLOAD_FAULTS", "") == "1",
		"enable X-Upload-Fault crash injection (verify only)")
	flag.Parse()

	if *ping {
		os.Exit(pingHealth(*addr))
	}

	store, err := storage.Open(*dataDir)
	if err != nil {
		log.Fatalf("open storage: %v", err)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           api.NewServer(store, *faults, nil).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: large legitimate uploads stream for a while.
		IdleTimeout: 60 * time.Second,
	}

	go func() {
		log.Printf("upload service listening on %s (data=%s, faults=%v)", *addr, *dataDir, *faults)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Printf("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
	}
	if err := store.Close(); err != nil {
		log.Printf("close store: %v", err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// pingHealth performs the container healthcheck without extra tooling in the
// image. addr has the form "host:port".
func pingHealth(addr string) int {
	host := addr
	if strings.HasPrefix(host, ":") {
		host = "127.0.0.1" + host
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + host + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
