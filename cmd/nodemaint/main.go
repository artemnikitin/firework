// Command firework-node-maintenance contains the explicit operator recovery
// operation for a retired node identity. Normal control-plane reconciliation
// never clears registry tombstones.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/artemnikitin/firework/internal/controlplane"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printUsage(out)
		return nil
	}
	if args[0] != "clear-node-tombstone" {
		return fmt.Errorf("unknown command %q", args[0])
	}
	flags := flag.NewFlagSet("clear-node-tombstone", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "node-maintenance config file")
	nodeID := flags.String("node", "", "retired node ID")
	expectedGeneration := flags.Int64("expected-generation", 0, "exact tombstone generation floor")
	reason := flags.String("reason", "", "bounded operator reason")
	externalFenceReference := flags.String("external-fence-reference", "", "incident or ticket reference for the external host fence and credential revocation")
	retainedPoolReference := flags.String("retained-pool-reference", "", "verified retained local-pool identity, or 'none' after confirming no local volume is bound")
	confirmHostFenced := flags.Bool("confirm-host-fenced", false, "confirm the old host is externally fenced and its bootstrap credential revoked or rotated")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(out)
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if strings.TrimSpace(*configPath) == "" || strings.TrimSpace(*nodeID) == "" {
		return errors.New("--config and --node are required")
	}
	if *expectedGeneration <= 0 {
		return errors.New("--expected-generation must be positive")
	}
	if !*confirmHostFenced {
		return errors.New("--confirm-host-fenced is required")
	}
	if strings.TrimSpace(*reason) == "" || strings.TrimSpace(*externalFenceReference) == "" || strings.TrimSpace(*retainedPoolReference) == "" {
		return errors.New("--reason, --external-fence-reference, and --retained-pool-reference are required")
	}
	cfg, err := controlplane.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if cfg.Role != controlplane.RoleNodeMaintenance {
		return fmt.Errorf("config role must be %q", controlplane.RoleNodeMaintenance)
	}
	store, err := openStateStore(context.Background(), cfg.State)
	if err != nil {
		return fmt.Errorf("opening state store: %w", err)
	}
	defer store.Close()
	audit, err := controlplane.ClearNodeTombstone(context.Background(), store, cfg.State.Prefix, controlplane.NodeTombstoneRecoveryRequest{
		NodeID: *nodeID, ExpectedGeneration: *expectedGeneration, Reason: *reason,
		ExternalFenceReference: *externalFenceReference, RetainedPoolReference: *retainedPoolReference,
		ConfirmedHostFenced: *confirmHostFenced,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "cleared node tombstone: node=%s generation=%d outcome=%s fence_reference=%s pool_reference=%s\n",
		audit.NodeID, audit.ClearedGeneration, audit.Outcome, audit.ExternalFenceReference, audit.RetainedPoolReference)
	return nil
}

func openStateStore(ctx context.Context, cfg controlplane.StateConfig) (controlplane.StateStore, error) {
	switch cfg.Backend {
	case "s3":
		return controlplane.NewS3StateStore(ctx, cfg.S3)
	case "gcs":
		return controlplane.NewGCSStateStore(ctx, cfg.GCS)
	default:
		return nil, fmt.Errorf("unsupported state backend %q", cfg.Backend)
	}
}

func printUsage(out io.Writer) {
	fmt.Fprint(out, `Usage:
  firework-node-maintenance clear-node-tombstone --config <path> --node <node-id> --expected-generation <generation> --reason <text> --external-fence-reference <ticket> --retained-pool-reference <identity|none> --confirm-host-fenced

The old host must be externally fenced, its bootstrap credential revoked or
rotated, and its retained local pool verified before this command runs.
`)
}
