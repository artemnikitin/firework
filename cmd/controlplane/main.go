package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/artemnikitin/firework/internal/controlplane"
	"github.com/artemnikitin/firework/internal/version"
)

func main() {
	configPath := flag.String("config", "/etc/firework/controlplane.yaml", "path to control-plane config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("firework-controlplane", version.String())
		return
	}

	if err := run(*configPath); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := controlplane.LoadConfig(configPath)
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logger.Info("received signal, shutting down", "signal", sig)
		cancel()
	}()

	logger.Info("starting control plane", "role", cfg.Role)
	if err := controlplane.Run(ctx, cfg, logger); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
