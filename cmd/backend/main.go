// Command backend is a tiny demo upstream server. Its responses identify the
// port that served them so load-balancing behavior is visible with curl.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	port := flag.Int("port", 8081, "port to listen on")
	healthDelay := flag.Duration("health-delay", 0, "artificial delay before /health responds (simulates a slow backend)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	id := fmt.Sprintf("backend=%d", *port)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(*healthDelay):
			fmt.Fprintln(w, "ok")
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		logger.Info("request", "backend", *port, "method", r.Method, "path", r.URL.Path)
		w.Header().Set("X-Backend", fmt.Sprint(*port))
		fmt.Fprintln(w, id)
	})

	srv := &http.Server{Addr: fmt.Sprintf(":%d", *port), Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	logger.Info("backend listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("backend failed", "error", err)
		os.Exit(1)
	}
}
