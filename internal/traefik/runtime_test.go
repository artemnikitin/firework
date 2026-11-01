package traefik

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
	"gopkg.in/yaml.v3"
)

func assertRuntimeBackend(t *testing.T, dir, service, host, backend string) {
	t.Helper()
	data := read(t, dir, configFileName(service))
	var document fileConfig
	if err := yaml.Unmarshal([]byte(data), &document); err != nil {
		t.Fatal(err)
	}
	if document.HTTP.Routers[service].Rule != "Host(`"+host+"`)" {
		t.Fatalf("unexpected hostname: %s", data)
	}
	servers := document.HTTP.Services[service].LoadBalancer.Servers
	if len(servers) != 1 || servers[0].URL != "http://"+backend {
		t.Fatalf("unexpected backend: %s", data)
	}
}

func TestSyncRuntimeRetainsPublishedMetadataAcrossRestartAndDisable(t *testing.T) {
	dir := t.TempDir()
	manager := NewManager(dir, "example.com", testLogger())
	launch := config.ServiceConfig{Name: "app", Network: &config.NetworkConfig{GuestIP: "172.16.0.2"}, HealthCheck: &config.HealthCheckConfig{Port: 80}, Metadata: map[string]string{"host": "launch.example.com"}}
	route := RuntimeRoute{Service: launch, InstanceID: "instance-1"}
	if err := manager.SyncRuntime([]RuntimeRoute{route}, nil, false); err != nil {
		t.Fatal(err)
	}
	current := launch
	current.Metadata = map[string]string{"host": "current.example.com"}
	current.HealthCheck = &config.HealthCheckConfig{Port: 90}
	route.Service = current
	if err := manager.SyncRuntime([]RuntimeRoute{route}, nil, false); err != nil {
		t.Fatal(err)
	}
	// The next daemon sees the launch configuration, not the hot routing
	// changes. A held update must use the projection actually published.
	manager = NewManager(dir, "changed.example.com", testLogger())
	held := RuntimeRoute{Service: launch, InstanceID: route.InstanceID, RetainPublished: true}
	if err := manager.SyncRuntime([]RuntimeRoute{held}, nil, false); err != nil {
		t.Fatal(err)
	}
	assertRuntimeBackend(t, dir, "app", "current.example.com", "172.16.0.2:90")
	current.Metadata = nil
	route.Service = current
	if err := manager.SyncRuntime([]RuntimeRoute{route}, nil, false); err != nil {
		t.Fatal(err)
	}
	manager = NewManager(dir, "example.com", testLogger())
	if err := manager.SyncRuntime([]RuntimeRoute{held}, nil, false); err != nil {
		t.Fatal(err)
	}
	data := read(t, dir, "app.yaml")
	var document fileConfig
	if err := yaml.Unmarshal([]byte(data), &document); err != nil {
		t.Fatal(err)
	}
	if len(document.HTTP.Routers) != 0 || len(document.HTTP.Services) != 0 {
		t.Fatalf("held update resurrected a removed hostname: %s", data)
	}
	if !strings.HasPrefix(data, runtimeRouteHeader) {
		t.Fatal("disabled route lost its durable owner binding")
	}
	if err := manager.SyncRuntime(nil, nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "app.yaml")); !os.IsNotExist(err) {
		t.Fatalf("removed runtime retained its projection: %v", err)
	}
}

func TestSyncRuntimeNeverRetainsUnboundOrDifferentOwner(t *testing.T) {
	for _, kind := range []string{"missing", "legacy", "malformed", "unknown-version", "bad-host", "bad-port", "changed-owner", "changed-IP", "empty-owner", "oversized-header", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			manager := NewManager(dir, "example.com", testLogger())
			service := localSvc("app", "172.16.0.2", 8080, 80, map[string]string{"host": "launch.example.com"})
			route := RuntimeRoute{Service: service, InstanceID: "instance-1"}
			if err := manager.SyncRuntime([]RuntimeRoute{route}, nil, false); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "app.yaml")
			var replacement []byte
			switch kind {
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "legacy":
				if err := manager.Sync([]config.ServiceConfig{service}, nil); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				replacement = []byte(runtimeRouteHeader + "invalid-base64!\nhttp: {}\n")
			case "unknown-version", "bad-host", "bad-port":
				projection := publishedRuntimeRoute{Version: runtimeRouteVersion, InstanceID: route.InstanceID, GuestIP: service.Network.GuestIP, Host: "launch.example.com", Port: 80}
				if kind == "unknown-version" {
					projection.Version++
				} else if kind == "bad-host" {
					projection.Host = "bad`hostname"
				} else {
					projection.Port = 70000
				}
				var err error
				replacement, err = marshalRuntimeRoute(service.Name, projection)
				if err != nil {
					t.Fatal(err)
				}
			case "changed-owner":
				route.InstanceID = "instance-2"
			case "changed-IP":
				route.Service.Network = &config.NetworkConfig{GuestIP: "172.16.0.3"}
			case "empty-owner":
				route.InstanceID = ""
			case "oversized-header":
				replacement = []byte(runtimeRouteHeader + strings.Repeat("A", maxRuntimeRouteHeaderBytes) + "\nhttp: {}\n")
			case "symlink":
				if err := os.Rename(path, filepath.Join(dir, "foreign-route")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("foreign-route", path); err != nil {
					t.Fatal(err)
				}
			}
			if replacement != nil {
				if err := os.WriteFile(path, replacement, 0600); err != nil {
					t.Fatal(err)
				}
			}
			route.RetainPublished = true
			if err := manager.SyncRuntime([]RuntimeRoute{route}, nil, false); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("retained an unsafe projection or fell back to launch metadata: %v", err)
			}
			if kind == "symlink" {
				if _, err := os.Stat(filepath.Join(dir, "foreign-route")); err != nil {
					t.Fatalf("followed or deleted another file: %v", err)
				}
			}
		})
	}
}

func TestSyncRuntimeDuplicateRetainedHostnameDoesNotMutateFiles(t *testing.T) {
	dir := t.TempDir()
	manager := NewManager(dir, "example.com", testLogger())
	a := RuntimeRoute{Service: localSvc("a", "172.16.0.2", 8080, 80, map[string]string{"host": "a.example.com"}), InstanceID: "a-1"}
	b := RuntimeRoute{Service: localSvc("b", "172.16.0.3", 8081, 81, map[string]string{"host": "b.example.com"}), InstanceID: "b-1"}
	if err := manager.SyncRuntime([]RuntimeRoute{a, b}, nil, false); err != nil {
		t.Fatal(err)
	}
	beforeA, beforeB := read(t, dir, "a.yaml"), read(t, dir, "b.yaml")
	a.RetainPublished = true
	a.Service.Metadata = map[string]string{"host": "ignored-desired.example.com"}
	b.Service.Metadata = map[string]string{"host": "a.example.com"}
	if err := manager.SyncRuntime([]RuntimeRoute{a, b}, nil, false); err == nil {
		t.Fatal("accepted a hostname already owned by a retained projection")
	}
	if read(t, dir, "a.yaml") != beforeA || read(t, dir, "b.yaml") != beforeB {
		t.Fatal("duplicate hostname validation changed published routes")
	}
}

func TestSyncRuntimeRemoteClaimsUseRetainedHostnameAndLocalOnlyPreservesPeers(t *testing.T) {
	dir := t.TempDir()
	manager := NewManager(dir, "example.com", testLogger())
	a := RuntimeRoute{Service: localSvc("a", "172.16.0.2", 8080, 80, map[string]string{"host": "retained.example.com"}), InstanceID: "a-1"}
	if err := manager.SyncRuntime([]RuntimeRoute{a}, nil, false); err != nil {
		t.Fatal(err)
	}
	a.RetainPublished = true
	a.Service.Metadata = map[string]string{"host": "desired.example.com"}
	peers := []config.NodeConfig{{Node: "peer", HostIP: "10.0.0.2", Services: []config.ServiceConfig{
		localSvc("conflict", "", 9090, 90, map[string]string{"host": "retained.example.com"}),
		localSvc("accepted", "", 9091, 91, map[string]string{"host": "desired.example.com"}),
	}}}
	if err := manager.SyncRuntime([]RuntimeRoute{a}, peers, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "remote-conflict.yaml")); !os.IsNotExist(err) {
		t.Fatalf("remote route claimed retained local hostname: %v", err)
	}
	before := read(t, dir, "remote-accepted.yaml")
	if err := manager.SyncRuntime([]RuntimeRoute{a}, nil, true); err != nil {
		t.Fatal(err)
	}
	if read(t, dir, "remote-accepted.yaml") != before {
		t.Fatal("local-only update changed an undiscovered peer route")
	}
}

func TestSyncRuntimeRetainsPerFilePublicationAfterPartialBatchFailure(t *testing.T) {
	dir := t.TempDir()
	manager := NewManager(dir, "example.com", testLogger())
	a := RuntimeRoute{Service: localSvc("a", "172.16.0.2", 8080, 80, map[string]string{"host": "old.example.com"}), InstanceID: "a-1"}
	if err := manager.SyncRuntime([]RuntimeRoute{a}, nil, false); err != nil {
		t.Fatal(err)
	}
	obstacle := filepath.Join(dir, "z.yaml")
	if err := os.Mkdir(obstacle, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(obstacle, "keep"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "stale.yaml")
	if err := os.WriteFile(stale, []byte("http: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a.Service.Metadata = map[string]string{"host": "published.example.com"}
	z := RuntimeRoute{Service: localSvc("z", "172.16.0.3", 8081, 81, map[string]string{"host": "z.example.com"}), InstanceID: "z-1"}
	if err := manager.SyncRuntime([]RuntimeRoute{a, z}, nil, false); err == nil {
		t.Fatal("expected the second rename to fail")
	}
	assertRuntimeBackend(t, dir, "a", "published.example.com", "172.16.0.2:80")
	if _, err := os.Stat(stale); err != nil {
		t.Fatal("partial batch failure removed stale files", err)
	}
	if err := os.RemoveAll(obstacle); err != nil {
		t.Fatal(err)
	}
	// No process-local cache survived, and the overall previous sync failed.
	// The header still records the first file that actually became visible.
	manager = NewManager(dir, "example.com", testLogger())
	a.RetainPublished = true
	a.Service.Metadata = map[string]string{"host": "unpublished.example.com"}
	if err := manager.SyncRuntime([]RuntimeRoute{a, z}, nil, false); err != nil {
		t.Fatal(err)
	}
	assertRuntimeBackend(t, dir, "a", "published.example.com", "172.16.0.2:80")
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("successful retry did not remove stale routes: %v", err)
	}
}

func TestSyncRuntimeFailedStagingKeepsPreviouslyPublishedProjection(t *testing.T) {
	dir := t.TempDir()
	manager := NewManager(dir, "example.com", testLogger())
	route := RuntimeRoute{Service: localSvc("app", "172.16.0.2", 8080, 80, map[string]string{"host": "published.example.com"}), InstanceID: "app-1"}
	if err := manager.SyncRuntime([]RuntimeRoute{route}, nil, false); err != nil {
		t.Fatal(err)
	}
	before := read(t, dir, "app.yaml")
	// A directory at the staging file's path makes WriteFile fail before any
	// rename, regardless of the permissions of the user running the tests.
	if err := os.MkdirAll(filepath.Join(dir, stageDirName, "app.yaml"+stageSuffix), 0755); err != nil {
		t.Fatal(err)
	}
	route.Service.Metadata = map[string]string{"host": "failed.example.com"}
	if err := manager.SyncRuntime([]RuntimeRoute{route}, nil, false); err == nil {
		t.Fatal("expected staging write to fail")
	}
	if read(t, dir, "app.yaml") != before {
		t.Fatal("failed staging changed published YAML or its owner projection")
	}
	manager = NewManager(dir, "example.com", testLogger())
	route.RetainPublished = true
	if err := manager.SyncRuntime([]RuntimeRoute{route}, nil, false); err != nil {
		t.Fatal(err)
	}
	assertRuntimeBackend(t, dir, "app", "published.example.com", "172.16.0.2:80")
}
