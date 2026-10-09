// Command zrl-bot-email serves the mailbox MCP server.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/zachlatta/zrl-bot-email-control/internal/app"
	"github.com/zachlatta/zrl-bot-email-control/internal/config"
	"github.com/zachlatta/zrl-bot-email-control/internal/store"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	command := "serve"
	if len(args) > 0 {
		command = args[0]
	}
	switch command {
	case "serve":
	case "version":
		cfg, _ := config.Load(os.Getenv)
		fmt.Fprintln(stdout, cfg.Version)
		return 0
	default:
		fmt.Fprintf(stderr, "usage: zrl-bot-email [serve|version]\n")
		return 2
	}
	_ = godotenv.Load() // existing environment variables win
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "config: %v\n", err)
		return 1
	}
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	fileStore, err := store.Open(filepath.Join(cfg.DataDir, "state.json"), time.Now)
	if err != nil {
		fmt.Fprintf(stderr, "store: %v\n", err)
		return 1
	}
	handler, err := app.New(cfg, fileStore, app.Options{})
	if err != nil {
		fmt.Fprintf(stderr, "app: %v\n", err)
		return 1
	}
	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           logRequests(logger, handler),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errs := make(chan error, 1)
	go func() { errs <- server.ListenAndServe() }()
	logger.Info("zrl-bot email control listening", "addr", cfg.Addr, "public_url", cfg.PublicURL, "mailbox", cfg.MailboxAddress,
		"allowed_recipients", strings.Join(cfg.Allowlist.Addresses(), ","), "version", cfg.Version)
	select {
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server failed", "error", err)
			return 1
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}
	return 0
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Flush() {
	if flusher, ok := s.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// logRequests logs method, path and status only: never query strings, which
// can carry OAuth codes.
func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		if r.URL.Path != "/healthz" {
			logger.Info("request", "method", r.Method, "path", r.URL.Path, "status", recorder.status, "duration_ms", time.Since(started).Milliseconds())
		}
	})
}
