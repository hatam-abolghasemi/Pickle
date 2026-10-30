package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// gitStore owns the local clone. Several clusters may push to one branch, each
// under its own directory, so every cycle builds the commit on top of the latest
// remote head, rewriting only the trees along this instance's changed paths.
// If another cluster pushes first, this cluster's directory is grafted onto the
// new remote head and pushed again; no merge is ever needed.
//
// The clone is shallow: pickle only ever needs the remote head to build on, so
// history never has to be downloaded, kept on disk or held in memory. Objects
// left behind by earlier cycles are dropped by compact.
type gitStore struct {
	repo        *git.Repository
	dir         string
	url         string
	branch      string
	cluster     string
	auth        transport.AuthMethod
	authorName  string
	authorEmail string

	compactEvery int
	sinceCompact int

	// packedPlain is the repo already set to push without delta search.
	packedPlain *git.Repository
	// written: objects stored since their page cache was last dropped.
	written []plumbing.Hash
	drops   dropper
}

const (
	objectCacheSize     = 2 * cache.MiByte
	defaultCompactEvery = 256
)

func openGitStore(cfg *config) (*gitStore, error) {
	auth, err := gitAuth(cfg)
	if err != nil {
		return nil, err
	}
	g := &gitStore{
		dir: cfg.RepoDir, url: cfg.GitURL, branch: cfg.GitBranch, cluster: cfg.Cluster, auth: auth,
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
	return g, nil
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
	g.drops.later(nil, []string{g.dir}) // the checkout: read back only if an object changes
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
	if err != nil {
		os.RemoveAll(filepath.Join(g.dir, ".git"))
		os.Rename(filepath.Join(g.dir, old), filepath.Join(g.dir, ".git"))
		slog.Warn("git compaction rolled back", "err", err)
		return
	}
	g.repo = repo
	os.RemoveAll(filepath.Join(g.dir, old))
	g.drops.later(nil, []string{filepath.Join(g.dir, ".git")})
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

// commit builds a commit on top of head (ZeroHash: the first commit) in which
// each path holds its file from disk, or is gone if the file is gone, and points
// the branch at it. Only the trees along the changed paths are rewritten, so the
// cost follows the size of the change, not of the repo; the index and worktree
// are never touched. Returns false if nothing changed.
func (g *gitStore) commit(head plumbing.Hash, paths []string, msg string, allowEmpty bool) (bool, error) {
	base, err := g.rootTree(head)
	if err != nil {
		return false, err
	}
	edits := make(map[string]plumbing.Hash, len(paths))
	for _, p := range paths {
		b, err := os.ReadFile(filepath.Join(g.dir, filepath.FromSlash(p)))
		switch {
		case err == nil:
			if edits[p], err = g.storeObject(plumbing.BlobObject, b); err != nil {
				return false, err
			}
		case errors.Is(err, os.ErrNotExist):
			edits[p] = plumbing.ZeroHash
		default:
			return false, err
		}
	}
	tree, err := g.editTree(base, edits)
	if err != nil {
		return false, err
	}
	if tree == base && !allowEmpty {
		return false, nil
	}
	now := time.Now()
	sig := object.Signature{Name: g.authorName, Email: g.authorEmail, When: now}
	if err := g.writeCommit(tree, head, msg, sig, sig); err != nil {
		return false, err
	}
	return true, nil
}

// rootTree returns the tree of commit h, or ZeroHash for ZeroHash.
func (g *gitStore) rootTree(h plumbing.Hash) (plumbing.Hash, error) {
	if h.IsZero() {
		return plumbing.ZeroHash, nil
	}
	c, err := object.GetCommit(g.repo.Storer, h)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return c.TreeHash, nil
}

// treeOf returns the root tree of commit h, or nil for ZeroHash.
func (g *gitStore) treeOf(h plumbing.Hash) (*object.Tree, error) {
	if h.IsZero() {
		return nil, nil
	}
	c, err := object.GetCommit(g.repo.Storer, h)
	if err != nil {
		return nil, err
	}
	return c.Tree()
}

// readTreeFile returns the file at p in tree t, and whether it exists there.
func readTreeFile(t *object.Tree, p string) ([]byte, bool, error) {
	if t == nil {
		return nil, false, nil
	}
	f, err := t.File(p)
	if errors.Is(err, object.ErrFileNotFound) || errors.Is(err, object.ErrDirectoryNotFound) || errors.Is(err, object.ErrEntryNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	s, err := f.Contents()
	if err != nil {
		return nil, false, err
	}
	return []byte(s), true, nil
}

// storeObject writes an object unless the store already has it.
func (g *gitStore) storeObject(t plumbing.ObjectType, content []byte) (plumbing.Hash, error) {
	h := plumbing.ComputeHash(t, content)
	if g.repo.Storer.HasEncodedObject(h) == nil {
		return h, nil
	}
	obj := g.repo.Storer.NewEncodedObject()
	obj.SetType(t)
	obj.SetSize(int64(len(content)))
	w, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err := w.Write(content); err != nil {
		w.Close()
		return plumbing.ZeroHash, err
	}
	if err := w.Close(); err != nil {
		return plumbing.ZeroHash, err
	}
	stored, err := g.repo.Storer.SetEncodedObject(obj)
	if err == nil {
		g.written = append(g.written, stored)
	}
	return stored, err
}

// treeEntries loads the entries of tree h by name (none for ZeroHash).
func (g *gitStore) treeEntries(h plumbing.Hash) (map[string]object.TreeEntry, error) {
	entries := map[string]object.TreeEntry{}
	if h.IsZero() {
		return entries, nil
	}
	t, err := object.GetTree(g.repo.Storer, h)
	if err != nil {
		return nil, err
	}
	for _, e := range t.Entries {
		entries[e.Name] = e
	}
	return entries, nil
}

// storeTree writes a tree in git's entry order. An empty tree is ZeroHash
// (git drops empty directories) unless keepEmpty is set.
func (g *gitStore) storeTree(entries map[string]object.TreeEntry, keepEmpty bool) (plumbing.Hash, error) {
	if len(entries) == 0 && !keepEmpty {
		return plumbing.ZeroHash, nil
	}
	list := make([]object.TreeEntry, 0, len(entries))
	for _, e := range entries {
		list = append(list, e)
	}
	key := func(e object.TreeEntry) string {
		if e.Mode == filemode.Dir {
			return e.Name + "/"
		}
		return e.Name
	}
	sort.Slice(list, func(i, j int) bool { return key(list[i]) < key(list[j]) })
	obj := &plumbing.MemoryObject{}
	if err := (&object.Tree{Entries: list}).Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	r, err := obj.Reader()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	defer r.Close()
	content, err := io.ReadAll(r)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return g.storeObject(plumbing.TreeObject, content)
}

// editTree returns tree base (ZeroHash: empty) with edits applied: each path,
// relative to base, set to a blob, or removed for ZeroHash. Directories left
// empty are dropped; an empty result is ZeroHash.
func (g *gitStore) editTree(base plumbing.Hash, edits map[string]plumbing.Hash) (plumbing.Hash, error) {
	entries, err := g.treeEntries(base)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	nested := map[string]map[string]plumbing.Hash{}
	for p, blob := range edits {
		name, rest, deeper := strings.Cut(p, "/")
		if deeper {
			if nested[name] == nil {
				nested[name] = map[string]plumbing.Hash{}
			}
			nested[name][rest] = blob
			continue
		}
		if blob.IsZero() {
			if e, ok := entries[name]; ok && e.Mode != filemode.Dir {
				delete(entries, name)
			}
			continue
		}
		entries[name] = object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: blob}
	}
	for name, sub := range nested {
		var subBase plumbing.Hash
		if e, ok := entries[name]; ok && e.Mode == filemode.Dir {
			subBase = e.Hash
		}
		h, err := g.editTree(subBase, sub)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		switch {
		case !h.IsZero():
			entries[name] = object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: h}
		case !subBase.IsZero():
			delete(entries, name)
		}
	}
	return g.storeTree(entries, false)
}

// writeCommit stores a commit (an empty tree for ZeroHash) and points the branch at it.
func (g *gitStore) writeCommit(tree, parent plumbing.Hash, msg string, author, committer object.Signature) error {
	if tree.IsZero() {
		var err error
		if tree, err = g.storeTree(nil, true); err != nil {
			return err
		}
	}
	c := &object.Commit{Author: author, Committer: committer, Message: msg, TreeHash: tree}
	if !parent.IsZero() {
		c.ParentHashes = []plumbing.Hash{parent}
	}
	obj := g.repo.Storer.NewEncodedObject()
	if err := c.Encode(obj); err != nil {
		return err
	}
	h, err := g.repo.Storer.SetEncodedObject(obj)
	if err == nil {
		g.written = append(g.written, h)
	}
	if err != nil {
		return err
	}
	return g.repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(g.branch), h))
}

const maxPushAttempts = 8

// push sends the branch head. When another cluster pushed first, the commit is
// rebuilt on the new remote head by rebase and sent again after a short random
// pause, so clusters that collide don't collide again in lockstep. Returns how
// many times the remote had moved.
func (g *gitStore) push(ctx context.Context) (conflicts int, err error) {
	for attempt := 1; ; attempt++ {
		err := g.pushOnce(ctx)
		if err == nil || attempt == maxPushAttempts || ctx.Err() != nil {
			return conflicts, err
		}
		sleepCtx(ctx, pushPause(attempt))
		moved, rerr := g.rebase(ctx)
		if rerr != nil {
			return conflicts, fmt.Errorf("%w (rebase: %v)", err, rerr)
		}
		if !moved {
			return conflicts, err
		}
		conflicts++
		slog.Debug("remote branch moved; pushing again on top of it", "attempt", attempt)
	}
}

// pushPause is a random pause, growing with the attempt, capped at 5s.
func pushPause(attempt int) time.Duration {
	d := min(250*time.Millisecond<<attempt, 5*time.Second)
	return d/2 + rand.N(d/2)
}

func (g *gitStore) pushOnce(ctx context.Context) error {
	g.packPlain()
	spec := gitconfig.RefSpec("refs/heads/" + g.branch + ":refs/heads/" + g.branch)
	err := g.repo.PushContext(ctx, &git.PushOptions{RemoteName: "origin", RefSpecs: []gitconfig.RefSpec{spec}, Auth: g.auth})
	if errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil
	}
	return err
}

// rebase fetches the remote head and, if it is no longer the parent of the local
// head, rebuilds the local head on top of it: the remote tree with this cluster's
// directory replaced by the local one. Only this instance writes that directory,
// so nothing of another cluster's is ever lost. Returns false if the remote has
// not moved (the push failed for another reason).
func (g *gitStore) rebase(ctx context.Context) (bool, error) {
	remote, err := g.fetch(ctx)
	if err != nil {
		return false, err
	}
	ref, err := g.repo.Reference(plumbing.NewBranchReferenceName(g.branch), true)
	if err != nil {
		return false, err
	}
	local, err := object.GetCommit(g.repo.Storer, ref.Hash())
	if err != nil {
		return false, err
	}
	var parent plumbing.Hash
	if len(local.ParentHashes) > 0 {
		parent = local.ParentHashes[0]
	}
	if parent == remote || local.Hash == remote {
		return false, nil
	}
	own, err := g.treeEntries(local.TreeHash)
	if err != nil {
		return false, err
	}
	remoteTree, err := g.rootTree(remote)
	if err != nil {
		return false, err
	}
	entries, err := g.treeEntries(remoteTree)
	if err != nil {
		return false, err
	}
	if e, ok := own[g.cluster]; ok {
		entries[g.cluster] = e
	} else {
		delete(entries, g.cluster)
	}
	tree, err := g.storeTree(entries, false)
	if err != nil {
		return false, err
	}
	if tree == remoteTree {
		return true, g.repo.Storer.SetReference(plumbing.NewHashReference(ref.Name(), remote))
	}
	committer := local.Committer
	committer.When = time.Now()
	return true, g.writeCommit(tree, remote, local.Message, local.Author, committer)
}

// packPlain makes pushes send objects without searching for deltas
// (pack.window = 0). The search loads every pushed object, and the trees of
// every directory a commit touches, into memory to compare them with each
// other: most of a push's memory and CPU, to save some bytes of small YAML
// files on the way to a server that repacks them anyway. Best effort.
func (g *gitStore) packPlain() {
	if g.repo == nil || g.packedPlain == g.repo {
		return
	}
	cfg, err := g.repo.Config()
	if err != nil {
		return
	}
	if cfg.Pack.Window != 0 {
		cfg.Pack.Window = 0
		if err := g.repo.SetConfig(cfg); err != nil {
			slog.Warn("can't turn off delta search for pushes", "err", err)
			return
		}
	}
	g.packedPlain = g.repo
}

// dropCaches hands back the page cache of what a pushed commit wrote: the
// objects stored since the last call, the packs (fetched ones included), and
// the working-tree files at paths (repo-relative). See pagecache.go.
func (g *gitStore) dropCaches(paths []string) {
	files := make([]string, 0, len(g.written)+len(paths))
	for _, h := range g.written {
		s := h.String()
		files = append(files, filepath.Join(g.dir, ".git", "objects", s[:2], s[2:]))
	}
	g.written = g.written[:0]
	for _, p := range paths {
		files = append(files, filepath.Join(g.dir, filepath.FromSlash(p)))
	}
	g.drops.later(files, []string{filepath.Join(g.dir, ".git", "objects", "pack")})
}
