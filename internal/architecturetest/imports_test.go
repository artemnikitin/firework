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

type buildTarget struct {
	goos   string
	goarch string
}

func (target buildTarget) String() string {
	return target.goos + "/" + target.goarch
}

var (
	linuxReleaseTargets = []buildTarget{
		{goos: "linux", goarch: "amd64"},
		{goos: "linux", goarch: "arm64"},
	}
	allReleaseTargets = []buildTarget{
		{goos: "linux", goarch: "amd64"},
		{goos: "linux", goarch: "arm64"},
		{goos: "darwin", goarch: "amd64"},
		{goos: "darwin", goarch: "arm64"},
	}
)

func TestCommandRuntimeBoundaries(t *testing.T) {
	tests := []struct {
		command              string
		targets              []buildTarget
		internalDependencies []string
	}{
		{
			command: "agent", targets: linuxReleaseTargets,
			internalDependencies: []string{
				"agent", "agentconfig", "api", "capacity", "config", "healthcheck",
				"imagesync", "ingress", "network", "objectstorage", "reconciler",
				"registryapi", "statusmodel", "store", "traefik", "version", "vm", "volume",
			},
		},
		{
			command: "controlplane", targets: linuxReleaseTargets,
			internalDependencies: []string{
				"config", "controlplane", "enricher", "ingress", "objectstorage",
				"operatorapi", "registryapi", "scheduler", "statusmodel", "version",
			},
		},
		{
			command: "fireworkctl", targets: allReleaseTargets,
			internalDependencies: []string{"operatorapi", "version"},
		},
		{
			command: "configcheck", targets: linuxReleaseTargets,
			internalDependencies: []string{
				"agentconfig", "config", "enricher", "ingress", "statusmodel", "volume",
			},
		},
		{command: "fc-init", targets: linuxReleaseTargets},
	}

	for _, tt := range tests {
		for _, target := range tt.targets {
			t.Run(tt.command+"/"+target.String(), func(t *testing.T) {
				root := modulePath + "/cmd/" + tt.command
				graph := loadProductionGraph(t, target, "./cmd/"+tt.command)
				allowed := make(map[string]bool, len(tt.internalDependencies))
				for _, name := range tt.internalDependencies {
					allowed[modulePath+"/internal/"+name] = true
				}

				path := dependencyPath(graph, root, func(importPath string) bool {
					return strings.HasPrefix(importPath, modulePath+"/internal/") && !allowed[importPath]
				})
				if len(path) > 0 {
					t.Fatalf("unexpected internal dependency:\n%s", strings.Join(path, "\n  -> "))
				}
				for _, name := range tt.internalDependencies {
					importPath := modulePath + "/internal/" + name
					if _, ok := graph[importPath]; !ok {
						t.Errorf("expected internal dependency is absent: %s", importPath)
					}
				}
			})
		}
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
		for _, target := range allReleaseTargets {
			t.Run(tt.name+"/"+target.String(), func(t *testing.T) {
				root := modulePath + "/internal/" + tt.target
				allowed := map[string]bool{root: true}
				for _, importPath := range tt.allowed {
					if strings.Contains(importPath, ".") {
						allowed[importPath] = true
					} else {
						allowed[modulePath+"/internal/"+importPath] = true
					}
				}
				graph := loadProductionGraph(t, target, "./internal/"+tt.target)
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

func loadProductionGraph(t *testing.T, target buildTarget, packages ...string) map[string]packageInfo {
	t.Helper()

	root := moduleRoot(t)
	args := append([]string{"list", "-deps", "-json"}, packages...)
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	cmd.Env = buildEnvironment(target)
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

func buildEnvironment(target buildTarget) []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "GOOS=") || strings.HasPrefix(entry, "GOARCH=") {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "GOOS="+target.goos, "GOARCH="+target.goarch)
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
