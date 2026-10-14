package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
)

// gitStore owns the local clone. Several clusters may push to one branch, each
// under its own directory, so every cycle rebuilds the commit on top of the
// latest remote head and stages only this instance's changed paths.
type gitStore struct {
	repo        *git.Repository
	wt          *git.Worktree
	dir         string
	branch      string
	auth        transport.AuthMethod
	authorName  string
	authorEmail string
}

func openGitStore(cfg *config) (*gitStore, error) {
	auth, err := gitAuth(cfg)
	if err != nil {
		return nil, err
	}
	g := &gitStore{dir: cfg.RepoDir, branch: cfg.GitBranch, auth: auth, authorName: cfg.AuthorName, authorEmail: cfg.AuthorEmail}
	branchRef := plumbing.NewBranchReferenceName(cfg.GitBranch)

	if _, err := os.Stat(filepath.Join(g.dir, ".git")); err == nil {
		g.repo, err = git.PlainOpen(g.dir)
		if err != nil {
			return nil, err
		}
	} else {
		g.repo, err = git.PlainClone(g.dir, false, &git.CloneOptions{
			URL: cfg.GitURL, Auth: auth, ReferenceName: branchRef, SingleBranch: true,
		})
		if err != nil {
			if !isEmptyRemote(err) {
				return nil, fmt.Errorf("clone: %w", err)
			}
			// Empty repo or missing branch: start fresh; the first push creates it.
			if err := os.RemoveAll(g.dir); err != nil {
				return nil, err
			}
			if g.repo, err = git.PlainInit(g.dir, false); err != nil {
				return nil, err
			}
			if _, err := g.repo.CreateRemote(&gitconfig.RemoteConfig{Name: "origin", URLs: []string{cfg.GitURL}}); err != nil {
				return nil, err
			}
			if err := g.repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, branchRef)); err != nil {
				return nil, err
			}
		}
	}
	g.wt, err = g.repo.Worktree()
	return g, err
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
	spec := gitconfig.RefSpec("+refs/heads/" + g.branch + ":refs/remotes/origin/" + g.branch)
	err := g.repo.FetchContext(ctx, &git.FetchOptions{RemoteName: "origin", RefSpecs: []gitconfig.RefSpec{spec}, Auth: g.auth, Force: true})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		if isEmptyRemote(err) {
			return plumbing.ZeroHash, nil
		}
		return plumbing.ZeroHash, err
	}
	ref, err := g.repo.Reference(plumbing.NewRemoteReferenceName("origin", g.branch), true)
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
