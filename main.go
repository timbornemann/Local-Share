package main

import (
	"context"
	"embed"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed web/*
var webFS embed.FS

func main() {
	cfg := Config{
		Addr:            env("LOCAL_SHARE_ADDR", ":8080"),
		TempDir:         env("LOCAL_SHARE_TEMP_DIR", filepath.Join(os.TempDir(), "local-share")),
		TTL:             5 * time.Minute,
		CleanupInterval: 5 * time.Second,
		StaticFS:        webFS,
		AllowedHosts:    splitList(os.Getenv("LOCAL_SHARE_ALLOWED_HOSTS")),
		MaxFileSize:     envMegabytes("LOCAL_SHARE_MAX_FILE_MB"),
		MaxTotalSize:    envMegabytes("LOCAL_SHARE_MAX_TOTAL_MB"),
		MaxTextSize:     envMegabytes("LOCAL_SHARE_MAX_TEXT_MB"),
		MaxItems:        int(envInt("LOCAL_SHARE_MAX_ITEMS")),
	}

	app, err := NewServer(cfg)
	if err != nil {
		log.Fatalf("create server: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go app.RunCleanup(ctx)

	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           app.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// No read/write timeout: uploads and downloads of large files are
		// legitimately slow. Idle keep-alive connections are still reaped.
		IdleTimeout:    2 * time.Minute,
		MaxHeaderBytes: 64 << 10,
	}
	// Open event streams never become idle, so they must be told to stop or
	// Shutdown would wait for its full timeout.
	server.RegisterOnShutdown(app.StopEvents)

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()

	log.Printf("Local Share listening on %s", listenURL(cfg.Addr))
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		app.Close()
		log.Fatalf("listen: %v", err)
	}

	// ListenAndServe returns as soon as Shutdown starts; wait for in-flight
	// requests to finish before deleting the stored files.
	<-shutdownDone
	app.Close()
}

// listenURL renders a listen address such as ":8080" or "127.0.0.1:9000" as a
// URL for the startup log.
func listenURL(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "http://0.0.0.0" + addr
	}
	return "http://" + addr
}

// splitList splits a comma separated environment value, dropping empty items.
func splitList(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// envInt reads an integer limit. Unset or invalid selects the built-in default
// (0); "0" or a negative number turns the limit off (-1).
func envInt(key string) int64 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return 0
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		log.Printf("ignoring %s=%q: not a number", key, value)
		return 0
	}
	if n <= 0 {
		return -1
	}
	return n
}

func envMegabytes(key string) int64 {
	n := envInt(key)
	if n <= 0 {
		return n
	}
	return n << 20
}

func env(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}
