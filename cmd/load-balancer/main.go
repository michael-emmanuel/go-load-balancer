// Command load-balancer runs the HTTP load balancer.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/example/load-balancer/internal/config"
	"github.com/example/load-balancer/internal/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "", "path to a config file (optional)")
	listen := flag.String("listen", "", "listen address, overrides config and LB_LISTEN_ADDR")
	flag.Parse()

	// Precedence, lowest to highest: defaults, file, environment, flags.
	cfg := config.Default()
	if *configPath != "" {
		var err error
		if cfg, err = config.LoadFile(*configPath); err != nil {
			return err
		}
	}
	if err := cfg.ApplyEnv(os.Getenv); err != nil {
		return err
	}
	if *listen != "" {
		cfg.ListenAddr = *listen
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	srv, err := server.New(cfg, logger)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		// After the first signal, restore default handling so a second Ctrl-C
		// terminates immediately if graceful shutdown hangs.
		<-ctx.Done()
		stop()
	}()

	return srv.Run(ctx)
}
