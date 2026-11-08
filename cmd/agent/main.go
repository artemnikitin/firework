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
	forget := commandFlags.Bool("forget", false, "only clean up a proven absent VM; do not adopt a live VM")
	commandFlags.Usage = func() {
		fmt.Fprintln(commandFlags.Output(), "Usage: firework-agent clear-recovery [--config PATH] [--forget] SERVICE")
		commandFlags.PrintDefaults()
	}
	if err := commandFlags.Parse(args); err != nil {
		return err
	}
	// Accept flags after the service name too, as in "clear-recovery app
	// --forget": parsing stops at the first argument that is not a flag.
	var services []string
	for commandFlags.NArg() > 0 {
		services = append(services, commandFlags.Arg(0))
		if err := commandFlags.Parse(commandFlags.Args()[1:]); err != nil {
			return err
		}
	}
	if len(services) != 1 {
		commandFlags.Usage()
		return fmt.Errorf("clear-recovery requires one service name")
	}
	service := services[0]
	cfg, err := agentconfig.LoadAgentConfig(*configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	resolve := agent.ClearRecovery
	if *forget {
		resolve = agent.ForgetRecovery
	}
	result, err := resolve(context.Background(), cfg.StateDir, service)
	if err != nil {
		return recoveryCommandError(result, err)
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

// recoveryCommandError adds the next step to a failed clear-recovery: a
// refusal changed nothing, while an incomplete resolution may have changed
// part of the VM's state before it failed.
func recoveryCommandError(result vm.RecoveryResult, err error) error {
	switch result.Outcome {
	case vm.RecoveryOutcomeAmbiguous:
		return fmt.Errorf("%w\nthe VM and its state were left unchanged; resolve the reason above, for example by stopping a process that still uses the VM's paths or waiting for one to finish, then run the command again (--forget clears state only once the VM is proven absent)", err)
	case vm.RecoveryOutcomeIncomplete:
		return fmt.Errorf("%w\nownership was settled, but part of the VM's state may already have changed; the service stays held and the running agent retries the rest, so check its status before running the command again; run it again if the service is still held after an agent restart", err)
	default:
		return err
	}
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
