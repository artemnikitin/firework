// Command configcheck validates a GitOps input directory using the same
// enricher code path as the control-plane events server, without performing any
// writes or cloud calls. It is intended to run in GitOps CI so a bad runtime
// configuration is rejected before expensive image builds.
//
// Usage:
//
//	configcheck --input-dir <dir> [--require-remote-routing]
//
// Exit status is non-zero on any validation error, and on any promoted warning
// when --require-remote-routing is set.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/artemnikitin/firework/internal/enricher"
)

func main() {
	inputDir := flag.String("input-dir", "", "path to the GitOps input directory to validate")
	requireRemoteRouting := flag.Bool("require-remote-routing", false,
		"treat a routed service without a valid first port_forwards host port as a validation failure")
	flag.Parse()

	if *inputDir == "" {
		fmt.Fprintln(os.Stderr, "configcheck: --input-dir is required")
		os.Exit(2)
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
