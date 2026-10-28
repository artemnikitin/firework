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

	"github.com/artemnikitin/firework/internal/agent"
	"github.com/artemnikitin/firework/internal/agentconfig"
	"github.com/artemnikitin/firework/internal/store"
	"github.com/artemnikitin/firework/internal/version"
	"github.com/artemnikitin/firework/internal/vm"
)

func main() {
	if err := runCommand(os.Args[1:], "/etc/firework/agent.yaml"); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func runCommand(args []string, defaultConfigPath string) error {
	commandFlags := flag.NewFlagSet("firework-agent", flag.ContinueOnError)
	commandFlags.SetOutput(os.Stderr)
	configPath := commandFlags.String("config", defaultConfigPath, "path to agent config file")
	showVersion := commandFlags.Bool("version", false, "print version and exit")
	if err := commandFlags.Parse(args); err != nil {
		return err
	}

	if *showVersion {
		fmt.Println("firework-agent", version.String())
		return nil
	}
	if commandFlags.NArg() > 0 {
		if commandFlags.Arg(0) != "clear-recovery" {
			return fmt.Errorf("unexpected argument %q", commandFlags.Arg(0))
		}
		return runClearRecovery(commandFlags.Args()[1:], *configPath)
	}

	return run(*configPath)
}

func runClearRecovery(args []string, defaultConfigPath string) error {
	commandFlags := flag.NewFlagSet("clear-recovery", flag.ContinueOnError)
	commandFlags.SetOutput(os.Stderr)
	configPath := commandFlags.String("config", defaultConfigPath, "path to agent config file")
	forget := commandFlags.Bool("forget", false, "clear unresolved shutdown state only after proving the VM and launcher are absent")
	commandFlags.Usage = func() {
		fmt.Fprintln(commandFlags.Output(), "Usage: firework-agent clear-recovery [--config PATH] [--forget] SERVICE")
		commandFlags.PrintDefaults()
	}
	if err := commandFlags.Parse(args); err != nil {
		return err
	}
	if commandFlags.NArg() != 1 {
		commandFlags.Usage()
		return fmt.Errorf("clear-recovery requires one service name")
	}
	cfg, err := agentconfig.LoadAgentConfig(*configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	resolve := agent.ClearRecovery
	if *forget {
		resolve = agent.ForgetRecovery
	}
	result, err := resolve(context.Background(), cfg.StateDir, commandFlags.Arg(0))
	if err != nil {
		return err
	}
	switch result.Outcome {
	case vm.RecoveryOutcomeNoop:
		if result.State == "" {
			fmt.Fprintf(os.Stdout, "service %s has no tracked VM\n", result.Service)
		} else {
			fmt.Fprintf(os.Stdout, "service %s is not recovery_pending (state %s)\n", result.Service, result.State)
		}
	case vm.RecoveryOutcomeCleared:
		fmt.Fprintf(os.Stdout, "cleared stale VM state for service %s\n", result.Service)
	case vm.RecoveryOutcomeReadopted:
		fmt.Fprintf(os.Stdout, "re-adopted the live VM for service %s; reconciliation was queued\n", result.Service)
	default:
		return fmt.Errorf("unexpected recovery outcome %q for service %s", result.Outcome, result.Service)
	}
	return nil
}

func run(configPath string) error {
	// Load agent configuration.
	cfg, err := agentconfig.LoadAgentConfig(configPath)
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
	case "gcs":
		gs, err := store.NewGCSStore(ctx, store.GCSStoreConfig{
			Bucket:          cfg.GCSBucket,
			Prefix:          cfg.GCSPrefix,
			CredentialsFile: cfg.GCSCredentialsFile,
			Project:         cfg.GCSProject,
		})
		if err != nil {
			return fmt.Errorf("creating gcs store: %w", err)
		}
		defer gs.Close()
		s = gs
	default:
		return fmt.Errorf("unsupported store type: %s", cfg.StoreType)
	}

	// Create and run the agent.
	a := agent.New(cfg, s, logger)

	return a.Run(ctx)
}
