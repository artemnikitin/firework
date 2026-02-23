package enricher

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

// GitReader clones a Git repository to a temporary directory using go-git.
type GitReader struct {
	repoURL  string
	branch   string
	localDir string
}

// NewGitReader creates a GitReader. It clones the repo immediately.
// The caller must call Close() to clean up the temporary directory.
func NewGitReader(ctx context.Context, repoURL, branch string) (*GitReader, error) {
	tmpDir, err := os.MkdirTemp("", "enricher-clone-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp dir: %w", err)
	}

	g := &GitReader{
		repoURL:  repoURL,
		branch:   branch,
		localDir: tmpDir,
	}

	if err := g.clone(ctx); err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, err
	}

	return g, nil
}

// Dir returns the path to the local clone.
func (g *GitReader) Dir() string {
	return g.localDir
}

// Close removes the temporary clone directory.
func (g *GitReader) Close() error {
	return os.RemoveAll(g.localDir)
}

func (g *GitReader) clone(ctx context.Context) error {
	opts := &git.CloneOptions{
		URL:           g.repoURL,
		ReferenceName: plumbing.NewBranchReferenceName(g.branch),
		SingleBranch:  true,
		Depth:         1,
	}

	// Inject GitHub token for private repos if available.
	if auth := gitHubAuth(g.repoURL); auth != nil {
		opts.Auth = auth
	}

	_, err := git.PlainCloneContext(ctx, g.localDir, false, opts)
	if err != nil {
		return fmt.Errorf("cloning %s (branch %s): %w", g.repoURL, g.branch, err)
	}
	return nil
}

// gitHubAuth returns HTTP basic auth using GITHUB_TOKEN for HTTPS GitHub URLs.
// Returns nil if the token is not set or the URL is not an HTTPS GitHub URL.
func gitHubAuth(repoURL string) *http.BasicAuth {
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
