package architecturetest

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const modulePath = "github.com/artemnikitin/firework"

type packageInfo struct {
	ImportPath string
	Imports    []string
	Standard   bool
}

func TestCommandRuntimeBoundaries(t *testing.T) {
	tests := []struct {
		name      string
		root      string
		forbidden []string
	}{
		{
			name: "agent does not depend on control plane",
			root: modulePath + "/cmd/agent",
			forbidden: []string{
				modulePath + "/internal/controlplane",
			},
		},
		{
			name: "control plane does not depend on agent runtime",
			root: modulePath + "/cmd/controlplane",
			forbidden: []string{
				modulePath + "/internal/agent",
				modulePath + "/internal/agentconfig",
				modulePath + "/internal/api",
				modulePath + "/internal/capacity",
				modulePath + "/internal/healthcheck",
				modulePath + "/internal/imagesync",
				modulePath + "/internal/network",
				modulePath + "/internal/reconciler",
				modulePath + "/internal/store",
				modulePath + "/internal/traefik",
				modulePath + "/internal/vm",
				modulePath + "/internal/volume",
			},
		},
	}

	graph := loadProductionGraph(t, "./cmd/agent", "./cmd/controlplane")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := dependencyPath(graph, tt.root, func(importPath string) bool {
				return matchesAnyPrefix(importPath, tt.forbidden)
			})
			if len(path) > 0 {
				t.Fatalf("forbidden production dependency:\n%s", strings.Join(path, "\n  -> "))
			}
		})
	}
}

func TestFireworkctlInternalDependencies(t *testing.T) {
	const root = modulePath + "/cmd/fireworkctl"
	allowed := map[string]bool{
		root:                                 true,
		modulePath + "/internal/operatorapi": true,
		modulePath + "/internal/version":     true,
	}
	graph := loadProductionGraph(t, "./cmd/fireworkctl")
	path := dependencyPath(graph, root, func(importPath string) bool {
		return strings.HasPrefix(importPath, modulePath+"/") && !allowed[importPath]
	})
	if len(path) > 0 {
		t.Fatalf("unexpected internal dependency:\n%s", strings.Join(path, "\n  -> "))
	}
}

func TestContractPackageDependencies(t *testing.T) {
	tests := []struct {
		name    string
		target  string
		allowed []string
	}{
		{name: "operator API", target: "operatorapi"},
		{name: "registry API", target: "registryapi", allowed: []string{"statusmodel"}},
		{name: "agent config", target: "agentconfig", allowed: []string{"config", "ingress", "statusmodel", "gopkg.in/yaml.v3"}},
		{name: "workload config", target: "config", allowed: []string{"gopkg.in/yaml.v3"}},
		{name: "status model", target: "statusmodel"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := modulePath + "/internal/" + tt.target
			allowed := map[string]bool{root: true}
			for _, importPath := range tt.allowed {
				if strings.Contains(importPath, ".") {
					allowed[importPath] = true
				} else {
					allowed[modulePath+"/internal/"+importPath] = true
				}
			}
			graph := loadProductionGraph(t, "./internal/"+tt.target)
			path := dependencyPath(graph, root, func(importPath string) bool {
				pkg, ok := graph[importPath]
				return ok && !pkg.Standard && !allowed[importPath]
			})
			if len(path) > 0 {
				t.Fatalf("unexpected dependency:\n%s", strings.Join(path, "\n  -> "))
			}
		})
	}
}

func TestDependencyPath(t *testing.T) {
	graph := map[string]packageInfo{
		"root":      {ImportPath: "root", Imports: []string{"allowed", "middle"}},
		"middle":    {ImportPath: "middle", Imports: []string{"forbidden"}},
		"allowed":   {ImportPath: "allowed"},
		"forbidden": {ImportPath: "forbidden"},
	}

	path := dependencyPath(graph, "root", func(importPath string) bool {
		return importPath == "forbidden"
	})
	if got, want := strings.Join(path, " -> "), "root -> middle -> forbidden"; got != want {
		t.Fatalf("dependencyPath() = %q, want %q", got, want)
	}
}

func TestMatchesAnyPrefixIncludesSubpackages(t *testing.T) {
	prefix := modulePath + "/internal/controlplane"
	if !matchesAnyPrefix(prefix+"/registry", []string{prefix}) {
		t.Fatal("subpackages must match a forbidden package prefix")
	}
}

func loadProductionGraph(t *testing.T, targets ...string) map[string]packageInfo {
	t.Helper()

	root := moduleRoot(t)
	args := append([]string{"list", "-deps", "-json"}, targets...)
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOOS=linux")
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			t.Fatalf("go list failed: %v\n%s", err, exitErr.Stderr)
		}
		t.Fatalf("go list failed: %v", err)
	}

	graph := make(map[string]packageInfo)
	decoder := json.NewDecoder(strings.NewReader(string(out)))
	for decoder.More() {
		var pkg packageInfo
		if err := decoder.Decode(&pkg); err != nil {
			t.Fatalf("decode go list output: %v", err)
		}
		graph[pkg.ImportPath] = pkg
	}
	return graph
}

func dependencyPath(graph map[string]packageInfo, root string, forbidden func(string) bool) []string {
	type candidate struct {
		importPath string
		path       []string
	}
	queue := []candidate{{importPath: root, path: []string{root}}}
	seen := map[string]bool{root: true}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if forbidden(current.importPath) {
			return current.path
		}
		for _, imported := range graph[current.importPath].Imports {
			if seen[imported] {
				continue
			}
			seen[imported] = true
			queue = append(queue, candidate{
				importPath: imported,
				path:       append(append([]string(nil), current.path...), imported),
			})
		}
	}
	return nil
}

func matchesAnyPrefix(importPath string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if importPath == prefix || strings.HasPrefix(importPath, prefix+"/") {
			return true
		}
	}
	return false
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal(fmt.Errorf("go.mod not found above %s", dir))
		}
		dir = parent
	}
}
