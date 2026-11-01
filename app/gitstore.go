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
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitclient "github.com/go-git/go-git/v5/plumbing/transport/client"
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
// The local store is shallow and sparse. Pickle only ever reads the remote
// head's root tree and this cluster's directory, so after the first download
// it keeps just those: other clusters' directories are referenced by hash in
// the root tree and never stored, read or walked. Fetches are incremental
// (only what other clusters changed since the last one), and pushes send only
// the objects along the paths this cluster changed. Objects left behind by
// earlier cycles are dropped by compact, without touching the network.
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
	for _, leftover := range []string{gitCompactDir, gitOldDir, gitFullDir} {
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
	gitFullDir    = ".git-full"
)

// initialClone downloads the branch head (depth 1), keeps only what pickle
// reads of it (see downloadSnapshot) and writes this cluster's directory into an
// empty worktree; other clusters' files are never written out. A marker inside
// .git is removed only once the checkout is complete, so an interrupted clone
// is redone on the next start. A non-empty directory without a .git is
// refused, never deleted.
func (g *gitStore) initialClone() error {
	entries, err := os.ReadDir(g.dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("PICKLE_REPO_DIR %s is not empty and is not a pickle clone; refusing to touch it", g.dir)
	}
	if err := os.MkdirAll(filepath.Join(g.dir, ".git"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(g.dir, ".git", cloneMarker), nil, 0o644); err != nil {
		return err
	}
	st := g.storage(gitCompactDir)
	if _, err := g.initStore(st); err != nil {
		return err
	}
	if _, err := g.downloadSnapshot(context.Background(), st, filepath.Join(g.dir, gitFullDir)); err != nil {
		os.RemoveAll(filepath.Join(g.dir, gitCompactDir))
		return fmt.Errorf("clone: %w", err)
	}
	if err := os.WriteFile(filepath.Join(g.dir, gitCompactDir, cloneMarker), nil, 0o644); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(g.dir, ".git")); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(g.dir, gitCompactDir), filepath.Join(g.dir, ".git")); err != nil {
		return err
	}
	if g.repo, err = git.Open(g.storage(".git"), osfs.New(g.dir)); err != nil {
		return err
	}
	head, err := g.repo.Reference(plumbing.NewBranchReferenceName(g.branch), true)
	switch {
	case errors.Is(err, plumbing.ErrReferenceNotFound):
	case err != nil:
		return err
	default:
		if err := g.checkout(head.Hash()); err != nil {
			return fmt.Errorf("checkout: %w", err)
		}
	}
	g.drops.later(nil, []string{g.dir}) // the checkout: read back only if an object changes
	return os.Remove(filepath.Join(g.dir, ".git", cloneMarker))
}

// checkout writes this cluster's directory of commit h into the worktree,
// one file at a time.
func (g *gitStore) checkout(h plumbing.Hash) error {
	root, err := g.rootTree(h)
	if err != nil || root.IsZero() {
		return err
	}
	entries, err := g.treeEntries(root)
	if err != nil {
		return err
	}
	e, ok := entries[g.cluster]
	if !ok || e.Mode != filemode.Dir {
		return nil
	}
	return g.checkoutTree(e.Hash, filepath.Join(g.dir, g.cluster))
}

func (g *gitStore) checkoutTree(h plumbing.Hash, dir string) error {
	t, err := object.GetTree(g.repo.Storer, h)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, e := range t.Entries {
		if strings.ContainsAny(e.Name, `/\`) || e.Name == "." || e.Name == ".." || e.Name == ".git" {
			return fmt.Errorf("unsafe path %q in tree %s", e.Name, h)
		}
		p := filepath.Join(dir, e.Name)
		switch {
		case e.Mode == filemode.Dir:
			if err := g.checkoutTree(e.Hash, p); err != nil {
				return err
			}
		case e.Mode.IsFile():
			if err := g.checkoutBlob(e.Hash, p); err != nil {
				return err
			}
		}
	}
	return nil
}

func (g *gitStore) checkoutBlob(h plumbing.Hash, p string) error {
	b, err := object.GetBlob(g.repo.Storer, h)
	if err != nil {
		return err
	}
	r, err := b.Reader()
	if err != nil {
		return err
	}
	defer r.Close()
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
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

// initStore initializes an empty object store with the origin remote and HEAD
// pointing at the branch.
func (g *gitStore) initStore(st *filesystem.Storage) (*git.Repository, error) {
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
	return repo, nil
}

// compact replaces the local object store with a sparse copy of itself (see
// sparseStore) once enough commits have piled up: the packs fetched since,
// and the loose objects of earlier commits, are never needed again, and
// without this they would grow until the emptyDir limit evicts the pod. It
// reads only this cluster's directory and costs no network. On any failure
// the current store is kept.
func (g *gitStore) compact(ctx context.Context) {
	g.sinceCompact++
	if g.sinceCompact < g.compactEvery {
		return
	}
	g.sinceCompact = 0
	if err := os.RemoveAll(filepath.Join(g.dir, gitCompactDir)); err != nil {
		slog.Warn("git compaction skipped", "err", err)
		return
	}
	if err := g.sparseStore(g.repo, gitCompactDir); err != nil {
		os.RemoveAll(filepath.Join(g.dir, gitCompactDir))
		slog.Warn("git compaction skipped", "err", err)
		return
	}
	if err := g.swapIn(gitCompactDir); err != nil {
		slog.Warn("git compaction skipped", "err", err)
		return
	}
	slog.Info("local git store compacted")
}

// swapIn makes the store at <dir>/<gitDir> the local store, keeping the
// current one if anything fails.
func (g *gitStore) swapIn(gitDir string) error {
	fresh, cur, old := filepath.Join(g.dir, gitDir), filepath.Join(g.dir, ".git"), filepath.Join(g.dir, gitOldDir)
	os.RemoveAll(old)
	if err := os.Rename(cur, old); err != nil {
		os.RemoveAll(fresh)
		return err
	}
	if err := os.Rename(fresh, cur); err != nil {
		os.Rename(old, cur)
		os.RemoveAll(fresh)
		return err
	}
	repo, err := git.Open(g.storage(".git"), osfs.New(g.dir))
	if err != nil {
		os.RemoveAll(cur)
		os.Rename(old, cur)
		return fmt.Errorf("rolled back: %w", err)
	}
	g.repo = repo
	g.written = g.written[:0]
	os.RemoveAll(old)
	g.drops.later(nil, []string{cur})
	return nil
}

// sparseStore writes a new object store at <dir>/<gitDir> holding, for the
// local branch head and the last known remote head, only what pickle reads:
// the commit, its root tree, and this cluster's directory. Other clusters'
// directories stay referenced by hash in the root tree, the way a partial
// clone references what it hasn't fetched. Both commits are recorded as
// shallow, and the remote-tracking ref is kept so the next fetch only brings
// what changed since.
func (g *gitStore) sparseStore(src *git.Repository, gitDir string) error {
	st := g.storage(gitDir)
	if _, err := g.initStore(st); err != nil {
		return err
	}
	var refs []*plumbing.Reference
	var commits []plumbing.Hash
	for _, name := range []plumbing.ReferenceName{plumbing.NewBranchReferenceName(g.branch), plumbing.NewRemoteReferenceName("origin", g.branch)} {
		ref, err := src.Reference(name, true)
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		refs = append(refs, plumbing.NewHashReference(name, ref.Hash()))
		if !slices.Contains(commits, ref.Hash()) {
			commits = append(commits, ref.Hash())
		}
	}
	if err := g.copySparse(src, st, commits); err != nil {
		return err
	}
	for _, r := range refs {
		if err := st.SetReference(r); err != nil {
			return err
		}
	}
	return st.SetShallow(commits)
}

// copySparse copies into st, for each of commits, the commit, its root tree
// and this cluster's directory.
func (g *gitStore) copySparse(src *git.Repository, st *filesystem.Storage, commits []plumbing.Hash) error {
	seen := map[plumbing.Hash]bool{}
	var keep []plumbing.Hash
	add := func(h plumbing.Hash) bool {
		if h.IsZero() || seen[h] {
			return false
		}
		seen[h] = true
		keep = append(keep, h)
		return true
	}
	var addTree func(h plumbing.Hash) error
	addTree = func(h plumbing.Hash) error {
		if !add(h) {
			return nil
		}
		t, err := object.GetTree(src.Storer, h)
		if err != nil {
			return err
		}
		for _, e := range t.Entries {
			switch {
			case e.Mode == filemode.Dir:
				if err := addTree(e.Hash); err != nil {
					return err
				}
			case e.Mode.IsFile():
				add(e.Hash)
			}
		}
		return nil
	}
	for _, h := range commits {
		c, err := object.GetCommit(src.Storer, h)
		if err != nil {
			return err
		}
		add(h)
		add(c.TreeHash)
		root, err := object.GetTree(src.Storer, c.TreeHash)
		if err != nil {
			return err
		}
		if e, err := root.FindEntry(g.cluster); err == nil && e.Mode == filemode.Dir {
			if err := addTree(e.Hash); err != nil {
				return err
			}
		}
	}
	return writePack(st, keep, func(h plumbing.Hash) (plumbing.ObjectType, []byte, error) {
		o, err := src.Storer.EncodedObject(plumbing.AnyObject, h)
		if err != nil {
			return 0, nil, err
		}
		r, err := o.Reader()
		if err != nil {
			return 0, nil, err
		}
		defer r.Close()
		b, err := io.ReadAll(r)
		return o.Type(), b, err
	})
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

// fetch brings the remote branch head into the local store and returns it,
// or ZeroHash if the branch doesn't exist yet.
//
// The fetch is incremental: the server is told which commits are here and
// sends only what was committed since. A depth-limited fetch can't do that:
// without thin packs, which go-git doesn't take, the server sends the whole
// tree of the new head again, every cluster's directory included, on every
// fetch. If the server shares none of the local commits (the branch's history
// was rewritten), the head is downloaded afresh instead (see resnapshot),
// never the rewritten history.
func (g *gitStore) fetch(ctx context.Context) (plumbing.Hash, error) {
	remote, haves, err := g.fetchKnown(ctx)
	if err != nil || remote.IsZero() || haves {
		return remote, err
	}
	return g.resnapshot(ctx)
}

// fetchKnown fetches the remote head on top of the local commits the server
// also has. haves is false when it shares none of them: nothing was fetched.
func (g *gitStore) fetchKnown(ctx context.Context) (remote plumbing.Hash, haves bool, err error) {
	sess, ar, err := g.uploadSession(ctx)
	if err != nil || ar == nil {
		return plumbing.ZeroHash, false, err
	}
	defer sess.Close()
	refs, err := ar.AllReferences()
	if err != nil {
		return plumbing.ZeroHash, false, err
	}
	ref, err := refs.Reference(plumbing.NewBranchReferenceName(g.branch))
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return plumbing.ZeroHash, false, nil
	}
	if err != nil {
		return plumbing.ZeroHash, false, err
	}
	remote = ref.Hash()
	if g.repo.Storer.HasEncodedObject(remote) == nil {
		return remote, true, g.trackRemote(remote)
	}
	var known []plumbing.Hash
	for _, name := range []plumbing.ReferenceName{plumbing.NewRemoteReferenceName("origin", g.branch), plumbing.NewBranchReferenceName(g.branch)} {
		if r, err := g.repo.Reference(name, true); err == nil && !slices.Contains(known, r.Hash()) && g.repo.Storer.HasEncodedObject(r.Hash()) == nil {
			known = append(known, r.Hash())
		}
	}
	if len(known) == 0 {
		return remote, false, nil
	}
	req := packp.NewUploadPackRequestFromCapabilities(ar.Capabilities)
	req.Wants = []plumbing.Hash{remote}
	req.Haves = known
	if ar.Capabilities.Supports(capability.NoProgress) {
		_ = req.Capabilities.Set(capability.NoProgress)
	}
	resp, err := sess.UploadPack(ctx, req)
	if err != nil {
		return plumbing.ZeroHash, false, err
	}
	defer resp.Close()
	if len(resp.ACKs) == 0 {
		slog.Warn("the remote branch shares no commit with the local clone (was its history rewritten?); downloading its head afresh")
		return remote, false, nil
	}
	if err := packfile.UpdateObjectStorage(g.repo.Storer, demux(req.Capabilities, resp)); err != nil {
		return plumbing.ZeroHash, false, err
	}
	if g.repo.Storer.HasEncodedObject(remote) != nil {
		return plumbing.ZeroHash, false, fmt.Errorf("fetch: %s not received", remote)
	}
	return remote, true, g.trackRemote(remote)
}

// uploadSession opens a fetch session; nil AdvRefs for an empty remote.
func (g *gitStore) uploadSession(ctx context.Context) (transport.UploadPackSession, *packp.AdvRefs, error) {
	ep, err := transport.NewEndpoint(g.url)
	if err != nil {
		return nil, nil, err
	}
	cl, err := gitclient.NewClient(ep)
	if err != nil {
		return nil, nil, err
	}
	sess, err := cl.NewUploadPackSession(ep, g.auth)
	if err != nil {
		return nil, nil, err
	}
	ar, err := sess.AdvertisedReferencesContext(ctx)
	if err != nil {
		sess.Close()
		if errors.Is(err, transport.ErrEmptyRemoteRepository) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	return sess, ar, nil
}

// resnapshot replaces the local store with a sparse download of the remote
// head (see downloadSnapshot), keeping the local branch head if it differs,
// so a commit waiting to be pushed can still be rebased.
func (g *gitStore) resnapshot(ctx context.Context) (plumbing.Hash, error) {
	if err := os.RemoveAll(filepath.Join(g.dir, gitCompactDir)); err != nil {
		return plumbing.ZeroHash, err
	}
	st := g.storage(gitCompactDir)
	fail := func(err error) (plumbing.Hash, error) {
		os.RemoveAll(filepath.Join(g.dir, gitCompactDir))
		return plumbing.ZeroHash, err
	}
	if _, err := g.initStore(st); err != nil {
		return fail(err)
	}
	head, err := g.downloadSnapshot(ctx, st, filepath.Join(g.dir, gitFullDir))
	if err != nil {
		return fail(err)
	}
	branch := plumbing.NewBranchReferenceName(g.branch)
	if ref, err := g.repo.Reference(branch, true); err == nil && ref.Hash() != head {
		if err := g.copySparse(g.repo, st, []plumbing.Hash{ref.Hash()}); err != nil {
			return fail(err)
		}
		if err := st.SetReference(plumbing.NewHashReference(branch, ref.Hash())); err != nil {
			return fail(err)
		}
		if err := st.SetShallow([]plumbing.Hash{head, ref.Hash()}); err != nil {
			return fail(err)
		}
	}
	if err := g.swapIn(gitCompactDir); err != nil {
		return plumbing.ZeroHash, err
	}
	g.sinceCompact = 0
	return head, nil
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

// readFile returns the file at p in commit h, and whether it exists there.
func (g *gitStore) readFile(h plumbing.Hash, p string) ([]byte, bool, error) {
	t, err := g.treeOf(h)
	if err != nil {
		return nil, false, err
	}
	return readTreeFile(t, p)
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
			return conflicts, fmt.Errorf("%w (rebase: %w)", err, rerr)
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

// errRemoteMoved: the remote branch is no longer the parent of the local head.
var errRemoteMoved = errors.New("remote branch moved")

// pushOnce sends the local head if the remote branch is still its parent.
// The pack holds only the objects along the paths the commit changed (see
// newObjects). go-git's own push would instead walk the entire tree of the
// remote head, and of every shallow commit, to find what the server already
// has: every object of every cluster on the branch, read and held in memory
// on each push.
func (g *gitStore) pushOnce(ctx context.Context) error {
	refName := plumbing.NewBranchReferenceName(g.branch)
	ref, err := g.repo.Reference(refName, true)
	if err != nil {
		return err
	}
	local, err := object.GetCommit(g.repo.Storer, ref.Hash())
	if err != nil {
		return err
	}
	var parent plumbing.Hash
	if len(local.ParentHashes) > 0 {
		parent = local.ParentHashes[0]
	}
	ep, err := transport.NewEndpoint(g.url)
	if err != nil {
		return err
	}
	cl, err := gitclient.NewClient(ep)
	if err != nil {
		return err
	}
	sess, err := cl.NewReceivePackSession(ep, g.auth)
	if err != nil {
		return err
	}
	defer sess.Close()
	ar, err := sess.AdvertisedReferencesContext(ctx)
	if err != nil {
		return err
	}
	var remote plumbing.Hash
	if refs, err := ar.AllReferences(); err != nil {
		return err
	} else if r, err := refs.Reference(refName); err == nil {
		remote = r.Hash()
	}
	switch {
	case remote == local.Hash:
		return g.trackRemote(local.Hash)
	case remote != parent:
		return errRemoteMoved
	}
	hashes, err := g.newObjects(local, parent)
	if err != nil {
		return err
	}
	req := packp.NewReferenceUpdateRequestFromCapabilities(ar.Capabilities)
	req.Commands = []*packp.Command{{Name: refName, Old: remote, New: local.Hash}}
	rd, wr := io.Pipe()
	req.Packfile = rd
	done := make(chan error, 1)
	go func() {
		_, err := packfile.NewEncoder(wr, g.repo.Storer, !ar.Capabilities.Supports(capability.OFSDelta)).Encode(hashes, 0)
		if err != nil {
			done <- wr.CloseWithError(err)
			return
		}
		done <- wr.Close()
	}()
	rs, err := sess.ReceivePack(ctx, req)
	if err != nil {
		rd.Close()
		<-done
		return err
	}
	if err := <-done; err != nil {
		return err
	}
	if rs != nil {
		if err := rs.Error(); err != nil {
			return err
		}
	}
	return g.trackRemote(local.Hash)
}

// trackRemote records h as the remote branch head.
func (g *gitStore) trackRemote(h plumbing.Hash) error {
	return g.repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", g.branch), h))
}

// newObjects lists the objects of commit c the server lacks when it has
// commit parent: c, and every tree and blob along the paths where c's tree
// differs from parent's. Unchanged directories are skipped by hash, so the
// cost follows the size of the change, not of the repo.
func (g *gitStore) newObjects(c *object.Commit, parent plumbing.Hash) ([]plumbing.Hash, error) {
	out := []plumbing.Hash{c.Hash}
	seen := map[plumbing.Hash]bool{c.Hash: true}
	base, err := g.rootTree(parent)
	if err != nil {
		return nil, err
	}
	var walk func(h, old plumbing.Hash) error
	walk = func(h, old plumbing.Hash) error {
		if h == old || seen[h] {
			return nil
		}
		seen[h] = true
		out = append(out, h)
		was, err := g.treeEntries(old)
		if err != nil {
			return err
		}
		t, err := object.GetTree(g.repo.Storer, h)
		if err != nil {
			return err
		}
		for _, e := range t.Entries {
			o, existed := was[e.Name]
			if existed && o.Hash == e.Hash {
				continue
			}
			switch {
			case e.Mode == filemode.Dir:
				var sub plumbing.Hash
				if existed && o.Mode == filemode.Dir {
					sub = o.Hash
				}
				if err := walk(e.Hash, sub); err != nil {
					return err
				}
			case e.Mode == filemode.Submodule:
			case !seen[e.Hash]:
				seen[e.Hash] = true
				out = append(out, e.Hash)
			}
		}
		return nil
	}
	if err := walk(c.TreeHash, base); err != nil {
		return nil, err
	}
	return out, nil
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
	if err := g.checkClaim(own[g.cluster], entries[g.cluster]); err != nil {
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

// checkClaim refuses to replace the remote's directory of this cluster when
// the remote's claim on it (see claim.go) isn't the local one: another
// cluster pushed under the same name meanwhile.
func (g *gitStore) checkClaim(local, remote object.TreeEntry) error {
	remoteClaim, err := g.claimIn(remote)
	if err != nil || remoteClaim == "" {
		return err
	}
	localClaim, err := g.claimIn(local)
	if err != nil {
		return err
	}
	if localClaim != remoteClaim {
		return &claimError{dir: g.cluster, have: remoteClaim, want: localClaim}
	}
	return nil
}

// claimIn returns the UID claimed in the directory tree dir, "" for none.
func (g *gitStore) claimIn(dir object.TreeEntry) (string, error) {
	if dir.Hash.IsZero() || dir.Mode != filemode.Dir {
		return "", nil
	}
	entries, err := g.treeEntries(dir.Hash)
	if err != nil {
		return "", err
	}
	f, ok := entries[clusterIDFile]
	if !ok {
		return "", nil
	}
	b, err := object.GetBlob(g.repo.Storer, f.Hash)
	if err != nil {
		return "", err
	}
	r, err := b.Reader()
	if err != nil {
		return "", err
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, 4096))
	if err != nil {
		return "", err
	}
	return parseClaim(data), nil
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
