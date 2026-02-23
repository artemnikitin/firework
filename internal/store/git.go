package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

// GitStore implements Store by cloning a Git repository and pulling updates.
// The expected repo layout is:
//
//	nodes/<node-name>.yaml   — per-node service assignments
type GitStore struct {
	repoURL  string
	branch   string
	localDir string
	auth     transport.AuthMethod

	mu   sync.Mutex
	repo *git.Repository
}

// NewGitStore creates a new GitStore. It will clone the repo into a
// temporary directory under baseDir on the first Fetch call.
func NewGitStore(repoURL, branch, baseDir string) (*GitStore, error) {
	localDir := filepath.Join(baseDir, "config-repo")
	return &GitStore{
		repoURL:  repoURL,
		branch:   branch,
		localDir: localDir,
		auth:     gitAuth(repoURL),
	}, nil
}

// Fetch clones or pulls the repo and reads nodes/<nodeName>.yaml.
func (g *GitStore) Fetch(ctx context.Context, nodeName string) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if err := g.sync(ctx); err != nil {
		return nil, fmt.Errorf("syncing git repo: %w", err)
	}

	configPath := filepath.Join(g.localDir, "nodes", nodeName+".yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("reading node config %s: %w", configPath, err)
	}
	return data, nil
}

// Revision returns the HEAD commit hash of the tracked branch.
func (g *GitStore) Revision(_ context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.repo == nil {
		return "", nil
	}

	ref, err := g.repo.Head()
	if err != nil {
		return "", fmt.Errorf("getting HEAD: %w", err)
	}
	return ref.Hash().String(), nil
}

// Close removes the local clone directory.
func (g *GitStore) Close() error {
	return os.RemoveAll(g.localDir)
}

// sync ensures the local repo is up to date with the remote.
func (g *GitStore) sync(ctx context.Context) error {
	if g.repo == nil {
		return g.cloneRepo(ctx)
	}
	return g.pullRepo(ctx)
}

// cloneRepo performs the initial shallow clone.
func (g *GitStore) cloneRepo(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(g.localDir), 0o755); err != nil {
		return fmt.Errorf("creating parent dir: %w", err)
	}
	// Remove stale clone if it exists.
	_ = os.RemoveAll(g.localDir)

	opts := &git.CloneOptions{
		URL:           g.repoURL,
		ReferenceName: plumbing.NewBranchReferenceName(g.branch),
		SingleBranch:  true,
		Depth:         1,
		Auth:          g.auth,
	}

	repo, err := git.PlainCloneContext(ctx, g.localDir, false, opts)
	if err != nil {
		return fmt.Errorf("cloning repo: %w", err)
	}
	g.repo = repo
	return nil
}

// pullRepo fetches and resets the working tree to the latest remote commit.
func (g *GitStore) pullRepo(ctx context.Context) error {
	refSpec := gitconfig.RefSpec(
		fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", g.branch, g.branch),
	)

	err := g.repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName: "origin",
		RefSpecs:   []gitconfig.RefSpec{refSpec},
		Depth:      1,
		Auth:       g.auth,
	})
	if err != nil && err != git.NoErrAlreadyUpToDate {
		return fmt.Errorf("fetching: %w", err)
	}

	// Resolve the remote tracking ref.
	remoteRef, err := g.repo.Reference(
		plumbing.NewRemoteReferenceName("origin", g.branch), true,
	)
	if err != nil {
		return fmt.Errorf("resolving remote ref: %w", err)
	}

	wt, err := g.repo.Worktree()
	if err != nil {
		return fmt.Errorf("getting worktree: %w", err)
	}

	// Hard reset working tree to the fetched commit.
	if err := wt.Reset(&git.ResetOptions{
		Commit: remoteRef.Hash(),
		Mode:   git.HardReset,
	}); err != nil {
		return fmt.Errorf("resetting to remote HEAD: %w", err)
	}

	return nil
}

// gitAuth returns HTTP basic auth using GITHUB_TOKEN for HTTPS GitHub URLs.
// Returns nil if the token is not set or the URL is not an HTTPS GitHub URL.
func gitAuth(repoURL string) transport.AuthMethod {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return nil
	}
	const prefix = "https://github.com/"
	if !strings.HasPrefix(repoURL, prefix) {
		return nil
	}
	return &http.BasicAuth{
		Username: "x-access-token",
		Password: token,
	}
}
