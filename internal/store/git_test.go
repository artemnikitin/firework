package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// initTestRepo creates a bare-style local git repo with an initial commit
// containing nodes/<nodeName>.yaml. Returns the repo path.
func initTestRepo(t *testing.T, nodeName, content string) string {
	t.Helper()
	repoDir := t.TempDir()

	repo, err := git.PlainInit(repoDir, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}

	// Create nodes/<nodeName>.yaml
	nodesDir := filepath.Join(repoDir, "nodes")
	if err := os.MkdirAll(nodesDir, 0o755); err != nil {
		t.Fatalf("mkdir nodes: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nodesDir, nodeName+".yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if _, err := wt.Add("nodes"); err != nil {
		t.Fatalf("add: %v", err)
	}
	_, err = wt.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "test",
			Email: "test@test.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	return repoDir
}

func TestGitStore_FetchAndRevision(t *testing.T) {
	yamlContent := "node: web\nservices: []\n"
	repoDir := initTestRepo(t, "web", yamlContent)

	baseDir := t.TempDir()
	store, err := NewGitStore(repoDir, "master", baseDir)
	if err != nil {
		t.Fatalf("NewGitStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// First call should trigger clone.
	data, err := store.Fetch(ctx, "web")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(data) != yamlContent {
		t.Errorf("got %q, want %q", string(data), yamlContent)
	}

	// Revision should return a non-empty commit hash.
	rev, err := store.Revision(ctx)
	if err != nil {
		t.Fatalf("Revision: %v", err)
	}
	if rev == "" {
		t.Error("expected non-empty revision after Fetch")
	}
	if len(rev) != 40 {
		t.Errorf("expected 40-char SHA, got %d chars: %s", len(rev), rev)
	}
}

func TestGitStore_FetchMissingNode(t *testing.T) {
	repoDir := initTestRepo(t, "web", "node: web\n")

	baseDir := t.TempDir()
	store, err := NewGitStore(repoDir, "master", baseDir)
	if err != nil {
		t.Fatalf("NewGitStore: %v", err)
	}
	defer store.Close()

	_, err = store.Fetch(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error fetching nonexistent node")
	}
}

func TestGitStore_RevisionBeforeFetch(t *testing.T) {
	repoDir := initTestRepo(t, "web", "node: web\n")

	baseDir := t.TempDir()
	store, err := NewGitStore(repoDir, "master", baseDir)
	if err != nil {
		t.Fatalf("NewGitStore: %v", err)
	}
	defer store.Close()

	// Before any Fetch, Revision should return empty (forces initial fetch).
	rev, err := store.Revision(context.Background())
	if err != nil {
		t.Fatalf("Revision: %v", err)
	}
	if rev != "" {
		t.Errorf("expected empty revision before Fetch, got %q", rev)
	}
}

func TestGitStore_PullUpdates(t *testing.T) {
	yamlV1 := "node: web\nservices: []\n"
	repoDir := initTestRepo(t, "web", yamlV1)

	baseDir := t.TempDir()
	store, err := NewGitStore(repoDir, "master", baseDir)
	if err != nil {
		t.Fatalf("NewGitStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Initial fetch.
	data, err := store.Fetch(ctx, "web")
	if err != nil {
		t.Fatalf("Fetch v1: %v", err)
	}
	if string(data) != yamlV1 {
		t.Errorf("v1: got %q, want %q", string(data), yamlV1)
	}
	rev1, _ := store.Revision(ctx)

	// Update the source repo (simulate a push).
	yamlV2 := "node: web\nservices:\n  - name: kibana\n"
	os.WriteFile(filepath.Join(repoDir, "nodes", "web.yaml"), []byte(yamlV2), 0o644)

	srcRepo, _ := git.PlainOpen(repoDir)
	wt, _ := srcRepo.Worktree()
	wt.Add("nodes/web.yaml")
	wt.Commit("update web", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "test",
			Email: "test@test.com",
			When:  time.Now(),
		},
	})

	// Second fetch should pick up the update.
	data, err = store.Fetch(ctx, "web")
	if err != nil {
		t.Fatalf("Fetch v2: %v", err)
	}
	if string(data) != yamlV2 {
		t.Errorf("v2: got %q, want %q", string(data), yamlV2)
	}

	rev2, _ := store.Revision(ctx)
	if rev2 == rev1 {
		t.Error("revision should change after update")
	}
}

func TestGitStore_Close(t *testing.T) {
	repoDir := initTestRepo(t, "web", "node: web\n")

	baseDir := t.TempDir()
	store, err := NewGitStore(repoDir, "master", baseDir)
	if err != nil {
		t.Fatalf("NewGitStore: %v", err)
	}

	// Fetch to trigger clone.
	store.Fetch(context.Background(), "web")

	cloneDir := store.localDir
	if _, err := os.Stat(cloneDir); err != nil {
		t.Fatalf("clone dir should exist: %v", err)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := os.Stat(cloneDir); !os.IsNotExist(err) {
		t.Error("clone dir should be removed after Close")
	}
}

func TestGitAuth_WithToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_test123")
	auth := gitAuth("https://github.com/owner/repo.git")
	if auth == nil {
		t.Fatal("expected non-nil auth")
	}
}

func TestGitAuth_NoToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	auth := gitAuth("https://github.com/owner/repo.git")
	if auth != nil {
		t.Errorf("expected nil auth when no token, got %v", auth)
	}
}

func TestGitAuth_NonGitHub(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_test123")
	auth := gitAuth("https://gitlab.com/owner/repo.git")
	if auth != nil {
		t.Errorf("expected nil auth for non-GitHub URL, got %v", auth)
	}
}
