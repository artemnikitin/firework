package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/reconciler"
	"github.com/artemnikitin/firework/internal/traefik"
)

// localRouteReader reads the route directory and plans a publish.
// *traefik.Manager implements it.
type localRouteReader interface {
	ReadLocalRoutes() ([]traefik.RouteFile, []string, error)
	PlannedLocalRoutes([]config.ServiceConfig) ([]traefik.RouteFile, error)
}

// beginRouteWithdrawals durably records every local route a publish of
// selected removes or moves, before any route file changes: Traefik may keep
// serving such a route, so its address must not go to another service until
// the serving process has restarted (L7). An unreadable route file the
// publish replaces may have routed anywhere.
func (a *Agent) beginRouteWithdrawals(selected []config.ServiceConfig) error {
	reader, ok := a.traefikMgr.(localRouteReader)
	if !ok || a.reconciler == nil {
		return nil
	}
	current, unreadable, err := reader.ReadLocalRoutes()
	if err != nil {
		return err
	}
	planned, err := reader.PlannedLocalRoutes(selected)
	if err != nil {
		return nil // The publish fails the same validation and changes nothing.
	}
	kept := make(map[traefik.RouteFile]bool, len(planned))
	for _, route := range planned {
		kept[route] = true
	}
	var withdrawn []reconciler.RouteTarget
	for _, route := range current {
		if !kept[route] {
			withdrawn = append(withdrawn, reconciler.RouteTarget{Service: route.Service, GuestIP: route.GuestIP})
		}
	}
	everyAddress := slices.ContainsFunc(unreadable, func(name string) bool { return strings.HasSuffix(name, ".yaml") })
	return a.reconciler.BeginRouteWithdrawals(withdrawn, everyAddress)
}

// routeProcess identifies the process serving the route directory now: the
// host's boot ID and the process's start, or an empty start when none runs.
type routeProcess interface {
	Observe() (bootID, start string, err error)
}

// agentRouteEvidence gives the reconciler the agent's routes and the process
// that serves them. It reads the agent's current route manager, so it follows
// a manager replaced after construction.
type agentRouteEvidence struct{ agent *Agent }

func (e *agentRouteEvidence) LocalRoutes() ([]reconciler.RouteTarget, []string, error) {
	reader, ok := e.agent.traefikMgr.(localRouteReader)
	if !ok {
		return nil, nil, nil
	}
	files, unreadable, err := reader.ReadLocalRoutes()
	if err != nil {
		return nil, nil, err
	}
	routes := make([]reconciler.RouteTarget, len(files))
	for i, file := range files {
		routes[i] = reconciler.RouteTarget{Service: file.Service, GuestIP: file.GuestIP}
	}
	return routes, unreadable, nil
}

func (e *agentRouteEvidence) ServingProcess() (string, string, error) {
	return e.agent.routeProcess.Observe()
}

func (e *agentRouteEvidence) Unit() string { return e.agent.cfg.TraefikUnit }

// systemdRouteProcess observes the main process of the configured systemd
// unit, which must be the only process serving the route directory.
type systemdRouteProcess struct{ unit string }

func (p systemdRouteProcess) Observe() (string, string, error) {
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", "", fmt.Errorf("reading the boot ID: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "show", p.unit,
		"--property=LoadState,ActiveState,MainPID,ExecMainStartTimestampMonotonic").Output()
	if err != nil {
		return "", "", fmt.Errorf("inspecting %s: %w", p.unit, err)
	}
	start, err := servingProcessStart(p.unit, string(out))
	return strings.TrimSpace(string(boot)), start, err
}

// servingProcessStart reads `systemctl show` output: the main process's start
// timestamp, or "" when the unit runs no process.
func servingProcessStart(unit, show string) (string, error) {
	fields := map[string]string{}
	for _, line := range strings.Split(show, "\n") {
		if key, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			fields[key] = value
		}
	}
	if fields["LoadState"] != "loaded" {
		return "", fmt.Errorf("systemd unit %s is not loaded (%q)", unit, fields["LoadState"])
	}
	pid, started := fields["MainPID"], fields["ExecMainStartTimestampMonotonic"]
	switch {
	case (fields["ActiveState"] == "inactive" || fields["ActiveState"] == "failed") && (pid == "" || pid == "0"):
		return "", nil
	case pid == "" || pid == "0" || started == "" || started == "0":
		return "", fmt.Errorf("systemd unit %s is %s without an identifiable main process", unit, fields["ActiveState"])
	}
	return started, nil
}
