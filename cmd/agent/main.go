package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/artemnikitin/firework/internal/agent"
	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/store"
	"github.com/artemnikitin/firework/internal/version"
)

func main() {
	configPath := flag.String("config", "/etc/firework/agent.yaml", "path to agent config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("firework-agent", version.String())
		return
	}

	if err := run(*configPath); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	// Load agent configuration.
	cfg, err := config.LoadAgentConfig(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Set up structured logging.
	logLevel := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))

	// Handle graceful shutdown on SIGINT/SIGTERM.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		logger.Info("received signal, shutting down", "signal", sig)
		cancel()
	}()

	// Initialize the config store.
	var s store.Store
	switch cfg.StoreType {
	case "git":
		gs, err := store.NewGitStore(cfg.StoreURL, cfg.StoreBranch, cfg.StateDir)
		if err != nil {
			return fmt.Errorf("creating git store: %w", err)
		}
		defer gs.Close()
		s = gs
	case "s3":
		ss, err := store.NewS3Store(ctx, store.S3StoreConfig{
			Bucket:      cfg.S3Bucket,
			Prefix:      cfg.S3Prefix,
			Region:      cfg.S3Region,
			EndpointURL: cfg.S3EndpointURL,
		})
		if err != nil {
			return fmt.Errorf("creating s3 store: %w", err)
		}
		defer ss.Close()
		s = ss
	default:
		return fmt.Errorf("unsupported store type: %s", cfg.StoreType)
	}

	// Create and run the agent.
	a := agent.New(cfg, s, logger)

	return a.Run(ctx)
}
