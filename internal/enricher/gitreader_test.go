package enricher

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGitReader_CloneAndRead(t *testing.T) {
	// Skip if git is not available.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}

	// Create a temporary bare repo to clone from.
	repoDir := t.TempDir()
	ctx := context.Background()

	// Initialize a git repo with a nodes.yaml file.
	commands := [][]string{
		{"git", "init", "--initial-branch=main", repoDir},
		{"git", "-C", repoDir, "config", "user.email", "test@test.com"},
		{"git", "-C", repoDir, "config", "user.name", "Test"},
	}
	for _, cmd := range commands {
		if out, err := exec.CommandContext(ctx, cmd[0], cmd[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("setup %v: %s: %v", cmd, out, err)
		}
	}

	// Create test files matching the enricher input layout.
	os.WriteFile(filepath.Join(repoDir, "defaults.yaml"), []byte("kernel: /vmlinux\n"), 0o644)
	svcDir := filepath.Join(repoDir, "services")
	os.MkdirAll(svcDir, 0o755)
	os.WriteFile(filepath.Join(svcDir, "web.yaml"), []byte("name: web\nimage: /img/web.ext4\nnode_type: compute\n"), 0o644)

	// Commit.
	commitCmds := [][]string{
		{"git", "-C", repoDir, "add", "."},
		{"git", "-C", repoDir, "commit", "-m", "initial"},
	}
	for _, cmd := range commitCmds {
		if out, err := exec.CommandContext(ctx, cmd[0], cmd[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("commit %v: %s: %v", cmd, out, err)
		}
	}

	// Clone via GitReader.
	reader, err := NewGitReader(ctx, repoDir, "main")
	if err != nil {
		t.Fatalf("NewGitReader: %v", err)
	}
	defer reader.Close()

	// Verify we can read the cloned files.
	data, err := os.ReadFile(filepath.Join(reader.Dir(), "defaults.yaml"))
	if err != nil {
		t.Fatalf("reading defaults.yaml: %v", err)
	}
	if len(data) == 0 {
		t.Error("defaults.yaml is empty")
	}

	// Verify services directory was cloned.
	data, err = os.ReadFile(filepath.Join(reader.Dir(), "services", "web.yaml"))
	if err != nil {
		t.Fatalf("reading services/web.yaml: %v", err)
	}
	if len(data) == 0 {
		t.Error("services/web.yaml is empty")
	}
}

func TestGitReader_Close(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}

	// Create a minimal repo.
	repoDir := t.TempDir()
	ctx := context.Background()

	commands := [][]string{
		{"git", "init", "--initial-branch=main", repoDir},
		{"git", "-C", repoDir, "config", "user.email", "test@test.com"},
		{"git", "-C", repoDir, "config", "user.name", "Test"},
		{"git", "-C", repoDir, "commit", "--allow-empty", "-m", "init"},
	}
	for _, cmd := range commands {
		if out, err := exec.CommandContext(ctx, cmd[0], cmd[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("setup %v: %s: %v", cmd, out, err)
		}
	}

	reader, err := NewGitReader(ctx, repoDir, "main")
	if err != nil {
		t.Fatalf("NewGitReader: %v", err)
	}

	dir := reader.Dir()
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("clone dir should exist: %v", err)
	}

	if err := reader.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("clone dir should be removed after Close")
	}
}

func TestGitHubAuth_WithToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_test123")
	auth := gitHubAuth("https://github.com/owner/repo.git")
	if auth == nil {
		t.Fatal("expected non-nil auth")
	}
	if auth.Username != "x-access-token" {
		t.Errorf("username = %q, want x-access-token", auth.Username)
	}
	if auth.Password != "ghp_test123" {
		t.Errorf("password = %q, want ghp_test123", auth.Password)
	}
}

func TestGitHubAuth_NoToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	auth := gitHubAuth("https://github.com/owner/repo.git")
	if auth != nil {
		t.Errorf("expected nil auth when no token, got %+v", auth)
	}
}

func TestGitHubAuth_NonGitHub(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_test123")
	auth := gitHubAuth("https://gitlab.com/owner/repo.git")
	if auth != nil {
		t.Errorf("expected nil auth for non-GitHub URL, got %+v", auth)
	}
}

func TestGitHubAuth_SSHUrl(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_test123")
	auth := gitHubAuth("git@github.com:owner/repo.git")
	if auth != nil {
		t.Errorf("expected nil auth for SSH URL, got %+v", auth)
	}
}
