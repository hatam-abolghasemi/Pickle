package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// gitStore owns the local clone. Several clusters may push to one branch, each
// under its own directory, so every cycle rebuilds the commit on top of the
// latest remote head and stages only this instance's changed paths.
//
// The clone is shallow: pickle only ever needs the remote head to build on, so
// history never has to be downloaded, kept on disk or held in memory. Objects
// left behind by earlier cycles are dropped by compact.
type gitStore struct {
	repo        *git.Repository
	wt          *git.Worktree
	dir         string
	url         string
	branch      string
	auth        transport.AuthMethod
	authorName  string
	authorEmail string

	compactEvery int
	sinceCompact int
}

const (
	objectCacheSize     = 8 * cache.MiByte
	defaultCompactEvery = 256
)

func openGitStore(cfg *config) (*gitStore, error) {
	auth, err := gitAuth(cfg)
	if err != nil {
		return nil, err
	}
	g := &gitStore{
		dir: cfg.RepoDir, url: cfg.GitURL, branch: cfg.GitBranch, auth: auth,
		authorName: cfg.AuthorName, authorEmail: cfg.AuthorEmail, compactEvery: cfg.CompactEvery,
	}
	if g.compactEvery <= 0 {
		g.compactEvery = defaultCompactEvery
	}
	for _, leftover := range []string{gitCompactDir, gitOldDir} {
		if err := os.RemoveAll(filepath.Join(g.dir, leftover)); err != nil {
			return nil, err
		}
	}
	_, gitErr := os.Stat(filepath.Join(g.dir, ".git"))
	_, markerErr := os.Stat(filepath.Join(g.dir, ".git", cloneMarker))
	switch {
	case gitErr == nil && markerErr != nil:
		if g.repo, err = git.Open(g.storage(".git"), osfs.New(g.dir)); err != nil {
			return nil, err
		}
	case gitErr == nil:
		slog.Warn("redoing an interrupted clone", "dir", g.dir)
		if err := clearDir(g.dir); err != nil {
			return nil, err
		}
		fallthrough
	default:
		if err := g.initialClone(); err != nil {
			return nil, err
		}
	}
	g.wt, err = g.repo.Worktree()
	return g, err
}

const (
	cloneMarker   = "pickle-clone-incomplete"
	gitCompactDir = ".git-compact"
	gitOldDir     = ".git-old"
)

// initialClone checks out the branch head (depth 1) into an empty directory.
// A marker inside .git (where a hard reset never looks) is removed only once
// the checkout is complete, so an interrupted clone is redone on the next start.
// A non-empty directory without a .git is refused, never deleted.
func (g *gitStore) initialClone() error {
	entries, err := os.ReadDir(g.dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("PICKLE_REPO_DIR %s is not empty and is not a pickle clone; refusing to touch it", g.dir)
	}
	marker := filepath.Join(g.dir, ".git", cloneMarker)
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		return err
	}
	if g.repo, err = g.cloneInto(context.Background(), ".git"); err != nil {
		return err
	}
	head, err := g.repo.Reference(plumbing.NewBranchReferenceName(g.branch), true)
	switch {
	case errors.Is(err, plumbing.ErrReferenceNotFound):
	case err != nil:
		return err
	default:
		wt, err := g.repo.Worktree()
		if err != nil {
			return err
		}
		if err := wt.Reset(&git.ResetOptions{Commit: head.Hash(), Mode: git.HardReset}); err != nil {
			return fmt.Errorf("checkout: %w", err)
		}
	}
	return os.Remove(marker)
}

// clearDir empties dir without removing dir itself (it may be a mount point).
func clearDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, en := range entries {
		if err := os.RemoveAll(filepath.Join(dir, en.Name())); err != nil {
			return err
		}
	}
	return nil
}

// storage opens a git object store under dir with a bounded object cache.
func (g *gitStore) storage(gitDir string) *filesystem.Storage {
	return filesystem.NewStorage(osfs.New(filepath.Join(g.dir, gitDir)), cache.NewObjectLRU(objectCacheSize))
}

// cloneInto fetches the branch tip (depth 1) into a new object store at
// <dir>/<gitDir>, with dir as its worktree. Worktree files are not touched.
// An empty remote or a missing branch gives an empty repository; the first push
// creates the branch.
func (g *gitStore) cloneInto(ctx context.Context, gitDir string) (*git.Repository, error) {
	st := g.storage(gitDir)
	repo, err := git.Init(st, nil) // no worktree here: Init would write a .git pointer file into it
	if err != nil {
		return nil, err
	}
	rc, err := repo.Config()
	if err != nil {
		return nil, err
	}
	rc.Core.IsBare = false
	if err := repo.Storer.SetConfig(rc); err != nil {
		return nil, err
	}
	if _, err := repo.CreateRemote(&gitconfig.RemoteConfig{Name: "origin", URLs: []string{g.url}}); err != nil {
		return nil, err
	}
	branchRef := plumbing.NewBranchReferenceName(g.branch)
	if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, branchRef)); err != nil {
		return nil, err
	}
	head, err := fetchHead(ctx, repo, g.branch, g.auth)
	if err != nil {
		return nil, fmt.Errorf("clone: %w", err)
	}
	if !head.IsZero() {
		if err := repo.Storer.SetReference(plumbing.NewHashReference(branchRef, head)); err != nil {
			return nil, err
		}
	}
	return git.Open(st, osfs.New(g.dir))
}

// compact replaces the local object store with a fresh depth-1 clone once enough
// commits have piled up. Old loose objects and fetched packs are never needed
// again, and without this they would grow until the emptyDir limit evicts the pod.
// On any failure the current store is kept.
func (g *gitStore) compact(ctx context.Context) {
	g.sinceCompact++
	if g.sinceCompact < g.compactEvery {
		return
	}
	g.sinceCompact = 0
	const fresh, old = gitCompactDir, gitOldDir
	if err := os.RemoveAll(filepath.Join(g.dir, fresh)); err != nil {
		slog.Warn("git compaction skipped", "err", err)
		return
	}
	if _, err := g.cloneInto(ctx, fresh); err != nil {
		os.RemoveAll(filepath.Join(g.dir, fresh))
		slog.Warn("git compaction skipped", "err", err)
		return
	}
	os.RemoveAll(filepath.Join(g.dir, old))
	if err := os.Rename(filepath.Join(g.dir, ".git"), filepath.Join(g.dir, old)); err != nil {
		os.RemoveAll(filepath.Join(g.dir, fresh))
		slog.Warn("git compaction skipped", "err", err)
		return
	}
	if err := os.Rename(filepath.Join(g.dir, fresh), filepath.Join(g.dir, ".git")); err != nil {
		os.Rename(filepath.Join(g.dir, old), filepath.Join(g.dir, ".git"))
		slog.Warn("git compaction skipped", "err", err)
		return
	}
	repo, err := git.Open(g.storage(".git"), osfs.New(g.dir))
	var wt *git.Worktree
	if err == nil {
		wt, err = repo.Worktree()
	}
	if err != nil {
		os.RemoveAll(filepath.Join(g.dir, ".git"))
		os.Rename(filepath.Join(g.dir, old), filepath.Join(g.dir, ".git"))
		slog.Warn("git compaction rolled back", "err", err)
		return
	}
	g.repo, g.wt = repo, wt
	os.RemoveAll(filepath.Join(g.dir, old))
	slog.Info("local git store compacted")
}

func gitAuth(cfg *config) (transport.AuthMethod, error) {
	u := cfg.GitURL
	switch {
	case strings.HasPrefix(u, "file://") || strings.HasPrefix(u, "/"):
		return nil, nil
	case strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://"):
		tok, err := os.ReadFile(cfg.GitTokenFile)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // anonymous
		}
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(u, "http://") {
			return nil, fmt.Errorf("refusing to send the git token over plain http://; use https://")
		}
		return &githttp.BasicAuth{Username: cfg.GitUsername, Password: strings.TrimSpace(string(tok))}, nil
	default: // ssh:// or git@host:path
		auth, err := gitssh.NewPublicKeysFromFile(cfg.GitSSHUser, cfg.GitSSHKeyFile, "")
		if err != nil {
			return nil, fmt.Errorf("ssh key: %w", err)
		}
		cb, err := gitssh.NewKnownHostsCallback(cfg.GitKnownHostsFile)
		if err != nil {
			return nil, fmt.Errorf("known_hosts (required, host keys are always verified): %w", err)
		}
		auth.HostKeyCallback = cb
		return auth, nil
	}
}

func isEmptyRemote(err error) bool {
	if errors.Is(err, transport.ErrEmptyRemoteRepository) || errors.Is(err, plumbing.ErrReferenceNotFound) {
		return true
	}
	var nm git.NoMatchingRefSpecError
	if errors.As(err, &nm) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "couldn't find remote ref") || strings.Contains(s, "reference not found")
}

// fetch returns the remote branch head, or ZeroHash if the branch doesn't exist yet.
func (g *gitStore) fetch(ctx context.Context) (plumbing.Hash, error) {
	return fetchHead(ctx, g.repo, g.branch, g.auth)
}

// fetchHead fetches only the tip of the branch (depth 1).
func fetchHead(ctx context.Context, repo *git.Repository, branch string, auth transport.AuthMethod) (plumbing.Hash, error) {
	spec := gitconfig.RefSpec("+refs/heads/" + branch + ":refs/remotes/origin/" + branch)
	err := repo.FetchContext(ctx, &git.FetchOptions{RemoteName: "origin", RefSpecs: []gitconfig.RefSpec{spec}, Auth: auth, Depth: 1, Force: true})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		if isEmptyRemote(err) {
			return plumbing.ZeroHash, nil
		}
		return plumbing.ZeroHash, err
	}
	ref, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", branch), true)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return plumbing.ZeroHash, nil
	}
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return ref.Hash(), nil
}

// commit points HEAD and the index at the remote head (worktree untouched), stages
// exactly the given paths from disk and commits. Returns false if nothing changed.
func (g *gitStore) commit(head plumbing.Hash, paths []string, msg string, allowEmpty bool) (bool, error) {
	if !head.IsZero() {
		if err := g.wt.Reset(&git.ResetOptions{Commit: head, Mode: git.MixedReset}); err != nil {
			return false, fmt.Errorf("reset to remote: %w", err)
		}
	}
	for _, p := range paths {
		if _, err := os.Stat(filepath.Join(g.dir, p)); err == nil {
			if err := g.wt.AddWithOptions(&git.AddOptions{Path: p, SkipStatus: true}); err != nil {
				return false, fmt.Errorf("add %s: %w", p, err)
			}
		} else if _, err := g.wt.Remove(p); err != nil && !errors.Is(err, index.ErrEntryNotFound) {
			return false, fmt.Errorf("remove %s: %w", p, err)
		}
	}
	sig := &object.Signature{Name: g.authorName, Email: g.authorEmail, When: time.Now()}
	_, err := g.wt.Commit(msg, &git.CommitOptions{Author: sig, Committer: sig, AllowEmptyCommits: allowEmpty})
	if errors.Is(err, git.ErrEmptyCommit) {
		return false, nil
	}
	return err == nil, err
}

func (g *gitStore) push(ctx context.Context) error {
	spec := gitconfig.RefSpec("refs/heads/" + g.branch + ":refs/heads/" + g.branch)
	err := g.repo.PushContext(ctx, &git.PushOptions{RemoteName: "origin", RefSpecs: []gitconfig.RefSpec{spec}, Auth: g.auth})
	if errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil
	}
	return err
}
