// Command configcheck validates a GitOps input directory using the same
// enricher code path as the control-plane events server, without performing any
// writes or cloud calls. It is intended to run in GitOps CI so a bad runtime
// configuration is rejected before expensive image builds.
//
// With --node-config it instead validates a hand-authored node config, the
// direct-Git alternative to control-plane-managed placement.
//
// Usage:
//
//	configcheck --input-dir <dir> [--require-remote-routing]
//	configcheck --node-config <file>
//
// Exit status is non-zero on any validation error, and on any promoted warning
// when --require-remote-routing is set.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/enricher"
	"github.com/artemnikitin/firework/internal/volume"
)

func main() {
	inputDir := flag.String("input-dir", "", "path to the GitOps input directory to validate")
	nodeConfig := flag.String("node-config", "", "path to a hand-authored node config to validate (direct-Git mode)")
	requireRemoteRouting := flag.Bool("require-remote-routing", false,
		"treat a routed service without a valid first port_forwards host port as a validation failure")
	flag.Parse()

	if (*inputDir == "") == (*nodeConfig == "") {
		fmt.Fprintln(os.Stderr, "configcheck: exactly one of --input-dir or --node-config is required")
		os.Exit(2)
	}

	if *nodeConfig != "" {
		if err := runNodeConfig(*nodeConfig); err != nil {
			fmt.Fprintln(os.Stderr, "configcheck: "+err.Error())
			os.Exit(1)
		}
		fmt.Println("configcheck: OK")
		return
	}

	if err := run(*inputDir, *requireRemoteRouting); err != nil {
		fmt.Fprintln(os.Stderr, "configcheck: "+err.Error())
		os.Exit(1)
	}
	fmt.Println("configcheck: OK")
}

func run(inputDir string, requireRemoteRouting bool) error {
	result, err := enricher.Enrich(inputDir)
	if err != nil {
		return fmt.Errorf("validation failed:\n%v", err)
	}

	// Print warnings with service context. Promote the remote-routing warning to
	// a failure when requested, so CI can enforce multi-node routability without
	// breaking single-node users that intentionally route via a health-check port.
	var promoted int
	for _, w := range result.Warnings {
		fmt.Fprintf(os.Stderr, "warning [%s]: %s\n", w.Code, w.Message)
		if requireRemoteRouting && w.Code == enricher.WarnRemoteRoutingNoHostPort {
			promoted++
		}
	}
	if promoted > 0 {
		return fmt.Errorf("%d service(s) cannot participate in remote routing (--require-remote-routing)", promoted)
	}

	fmt.Printf("validated %d node config(s)\n", len(result.NodeConfigs))
	return nil
}

// runNodeConfig validates a hand-authored node config. Its warnings are
// advisory: nothing can verify a resize_generation without history, so the
// check reports the shape that is almost always a mistake rather than failing.
func runNodeConfig(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading node config: %w", err)
	}
	nc, err := config.ParseNodeConfig(data)
	if err != nil {
		return err
	}
	// Parsing only proves the YAML is well formed. A hand-authored node config
	// is the direct-Git equivalent of an enriched one, so it gets the same
	// semantic validation the control plane applies before rendering —
	// otherwise this command reports OK for a config with no node name, no
	// image or kernel, zero compute, or a negative volume size, which defeats
	// the point of running it in CI.
	if err := enricher.ValidateOutput(nc); err != nil {
		return fmt.Errorf("validation failed:\n%v", err)
	}
	// ValidateOutput covers generic service fields. The volume contract is
	// enforced separately, by the agent's own rules, so a config cannot pass
	// here and then fail to start on the node.
	if err := volume.ValidateNodeVolumes(nc); err != nil {
		return fmt.Errorf("validation failed:\n%v", err)
	}
	for _, warning := range config.NodeConfigWarnings(nc) {
		fmt.Fprintf(os.Stderr, "warning [volume_size_without_generation]: %s\n", warning)
	}
	fmt.Printf("validated node config %s with %d service(s)\n", nc.Node, len(nc.Services))
	return nil
}
