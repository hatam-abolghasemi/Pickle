package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	gitcfg "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func writeFile(p, content string) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), 0o644)
}

func newRemote(t *testing.T) string {
	t.Helper()
	remote := t.TempDir()
	if _, err := git.PlainInit(remote, true); err != nil {
		t.Fatal(err)
	}
	return remote
}

func storeFor(t *testing.T, remote, dir string) *gitStore {
	t.Helper()
	g, err := openGitStore(&config{RepoDir: dir, GitURL: remote, GitBranch: "master", Cluster: "stg", AuthorName: "p", AuthorEmail: "p@p"})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func cycle(t *testing.T, g *gitStore, path, content string) {
	t.Helper()
	ctx := context.Background()
	if err := writeFile(filepath.Join(g.dir, path), content); err != nil {
		t.Fatal(err)
	}
	head, err := g.fetch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.commit(head, []string{path}, "c", false); err != nil {
		t.Fatal(err)
	}
	if _, err := g.push(ctx); err != nil {
		t.Fatal(err)
	}
	g.compact(ctx)
}

func dirSize(t *testing.T, dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

func TestGitStoreShallowCompaction(t *testing.T) {
	remote := newRemote(t)
	pushFile(t, remote, "prod/a.yaml", "a\n", "prod")
	g := storeFor(t, remote, filepath.Join(t.TempDir(), "repo"))
	g.compactEvery = 3

	big := strings.Repeat("x", 64<<10)
	for i := 0; i < 10; i++ {
		cycle(t, g, "stg/big.yaml", big+string(rune('a'+i)))
		if i%4 == 0 {
			pushFile(t, remote, "prod/b.yaml", big+string(rune('A'+i)), "prod meanwhile")
		}
	}
	files := remoteFiles(t, remote)
	if files["stg/big.yaml"] != big+"j" || files["prod/a.yaml"] != "a\n" || files["prod/b.yaml"] == "" {
		t.Fatalf("remote lost data: have %v", keys(files))
	}
	for _, leftover := range []string{gitCompactDir, gitOldDir} {
		if _, err := os.Stat(filepath.Join(g.dir, leftover)); err == nil {
			t.Errorf("%s left behind", leftover)
		}
	}
	if sz := dirSize(t, filepath.Join(g.dir, ".git")); sz > 400<<10 {
		t.Errorf("local store should stay small after compaction, is %d bytes", sz)
	}
}

func TestGitStoreRefusesForeignDir(t *testing.T) {
	remote := newRemote(t)
	dir := t.TempDir()
	if err := writeFile(filepath.Join(dir, "precious.txt"), "keep me"); err != nil {
		t.Fatal(err)
	}
	if _, err := openGitStore(&config{RepoDir: dir, GitURL: remote, GitBranch: "master"}); err == nil {
		t.Fatal("a non-empty directory that is not a pickle clone must be refused")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "precious.txt")); string(b) != "keep me" {
		t.Fatal("foreign files must never be deleted")
	}
}

func TestGitStoreRedoesInterruptedClone(t *testing.T) {
	remote := newRemote(t)
	pushFile(t, remote, "stg/a.yaml", "a\n", "seed")
	dir := filepath.Join(t.TempDir(), "repo")
	if err := writeFile(filepath.Join(dir, ".git", cloneMarker), ""); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(dir, "stg", "half.yaml"), "partial"); err != nil {
		t.Fatal(err)
	}
	storeFor(t, remote, dir)
	if b, _ := os.ReadFile(filepath.Join(dir, "stg", "a.yaml")); string(b) != "a\n" {
		t.Fatal("clone was not redone")
	}
	if _, err := os.Stat(filepath.Join(dir, "stg", "half.yaml")); err == nil {
		t.Fatal("leftovers of the interrupted clone must be cleared")
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", cloneMarker)); err == nil {
		t.Fatal("marker must be gone after a complete clone")
	}
}

func clusterStore(t *testing.T, remote, cluster string) *gitStore {
	t.Helper()
	g, err := openGitStore(&config{RepoDir: filepath.Join(t.TempDir(), "repo"), GitURL: remote, GitBranch: "master", Cluster: cluster, AuthorName: "p", AuthorEmail: "p@p"})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func stage(t *testing.T, g *gitStore, files map[string]string) {
	t.Helper()
	head, err := g.fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for p, content := range files {
		full := filepath.Join(g.dir, p)
		if content == "" {
			os.Remove(full)
		} else if err := writeFile(full, content); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	if ok, err := g.commit(head, paths, "c", false); err != nil || !ok {
		t.Fatalf("commit: %v %v", ok, err)
	}
}

func TestGitStorePushRebasesWhenAnotherClusterPushedFirst(t *testing.T) {
	remote := newRemote(t)
	pushFile(t, remote, "prod/old.yaml", "old\n", "seed")
	stg, prod := clusterStore(t, remote, "stg"), clusterStore(t, remote, "prod")
	ctx := context.Background()

	stage(t, stg, map[string]string{"stg/ns/a.yaml": "a\n"})
	stage(t, prod, map[string]string{"prod/ns/b.yaml": "b\n", "prod/old.yaml": ""})
	if n, err := prod.push(ctx); err != nil || n != 0 {
		t.Fatalf("prod push: conflicts=%d err=%v", n, err)
	}
	n, err := stg.push(ctx)
	if err != nil {
		t.Fatalf("stg push after prod moved the branch: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 conflict, got %d", n)
	}
	files := remoteFiles(t, remote)
	if files["stg/ns/a.yaml"] != "a\n" || files["prod/ns/b.yaml"] != "b\n" || len(files) != 2 {
		t.Fatalf("remote should hold both clusters' changes and nothing else, has %v", keys(files))
	}
}

func TestGitStoreCommitEditsOnlyChangedPaths(t *testing.T) {
	remote := newRemote(t)
	pushFile(t, remote, "prod/x.yaml", "x\n", "seed")
	g := clusterStore(t, remote, "stg")
	ctx := context.Background()

	stage(t, g, map[string]string{"stg/a/one.yaml": "1\n", "stg/a/two.yaml": "2\n", "stg/b/three.yaml": "3\n"})
	if _, err := g.push(ctx); err != nil {
		t.Fatal(err)
	}
	stage(t, g, map[string]string{"stg/a/one.yaml": "", "stg/a/two.yaml": "", "stg/b/three.yaml": "3b\n"})
	if _, err := g.push(ctx); err != nil {
		t.Fatal(err)
	}
	files := remoteFiles(t, remote)
	if len(files) != 2 || files["stg/b/three.yaml"] != "3b\n" || files["prod/x.yaml"] != "x\n" {
		t.Fatalf("unexpected remote content %v", files)
	}

	head, _ := g.fetch(ctx)
	if ok, err := g.commit(head, []string{"stg/b/three.yaml", "stg/gone.yaml"}, "noop", false); err != nil || ok {
		t.Fatalf("unchanged content must not commit: %v %v", ok, err)
	}
	if ok, err := g.commit(head, nil, "note", true); err != nil || !ok {
		t.Fatalf("allowEmpty must commit: %v %v", ok, err)
	}
}

func TestGitStoreFirstCommitOnEmptyRemote(t *testing.T) {
	remote := newRemote(t)
	g := clusterStore(t, remote, "stg")
	stage(t, g, map[string]string{"stg/a.yaml": "a\n"})
	if _, err := g.push(context.Background()); err != nil {
		t.Fatal(err)
	}
	if files := remoteFiles(t, remote); files["stg/a.yaml"] != "a\n" {
		t.Fatalf("got %v", files)
	}
}

func TestGitStoreKeepsOnlyItsOwnCluster(t *testing.T) {
	remote := newRemote(t)
	pushFile(t, remote, "prod/ns/p.yaml", "p\n", "prod")
	pushFile(t, remote, "stg/ns/s.yaml", "s\n", "stg")
	g := clusterStore(t, remote, "stg")
	if b, _ := os.ReadFile(filepath.Join(g.dir, "stg", "ns", "s.yaml")); string(b) != "s\n" {
		t.Fatal("own cluster's files must be checked out")
	}
	if _, err := os.Stat(filepath.Join(g.dir, "prod")); err == nil {
		t.Fatal("other clusters' files must not be written out")
	}
	head, err := g.fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := g.readFile(head, "stg/ns/s.yaml"); err != nil || !ok {
		t.Fatalf("own cluster must be readable from the store: %v %v", ok, err)
	}
	if _, ok, _ := g.readFile(head, "prod/ns/p.yaml"); ok {
		t.Fatal("other clusters' objects must not be stored locally")
	}
	stage(t, g, map[string]string{"stg/ns/s2.yaml": "s2\n"})
	if _, err := g.push(context.Background()); err != nil {
		t.Fatal(err)
	}
	if files := remoteFiles(t, remote); files["prod/ns/p.yaml"] != "p\n" || files["stg/ns/s2.yaml"] != "s2\n" || files["stg/ns/s.yaml"] != "s\n" {
		t.Fatalf("push must keep every cluster's files, remote has %v", keys(files))
	}
}

func TestGitStoreFetchIsIncremental(t *testing.T) {
	remote := newRemote(t)
	big := strings.Repeat("y", 32<<10)
	for i := 0; i < 20; i++ {
		pushFile(t, remote, fmt.Sprintf("prod/f%d.yaml", i), big+fmt.Sprint(i), "prod")
	}
	g := clusterStore(t, remote, "stg")
	ctx := context.Background()
	if _, err := g.fetch(ctx); err != nil {
		t.Fatal(err)
	}
	before := dirSize(t, filepath.Join(g.dir, ".git"))
	pushFile(t, remote, "prod/f0.yaml", "small\n", "prod meanwhile")
	if _, err := g.fetch(ctx); err != nil {
		t.Fatal(err)
	}
	if grown := dirSize(t, filepath.Join(g.dir, ".git")) - before; grown > 16<<10 {
		t.Fatalf("a fetch after a one-line change elsewhere downloaded %d bytes: the whole tree again", grown)
	}
}

func TestNewObjectsFollowOnlyChangedPaths(t *testing.T) {
	remote := newRemote(t)
	for i := 0; i < 5; i++ {
		pushFile(t, remote, fmt.Sprintf("stg/ns%d/a.yaml", i), fmt.Sprint(i), "seed")
	}
	g := clusterStore(t, remote, "stg")
	stage(t, g, map[string]string{"stg/ns3/a.yaml": "changed"})
	ref, err := g.repo.Reference(plumbing.NewBranchReferenceName("master"), true)
	if err != nil {
		t.Fatal(err)
	}
	c, err := object.GetCommit(g.repo.Storer, ref.Hash())
	if err != nil {
		t.Fatal(err)
	}
	hs, err := g.newObjects(c, c.ParentHashes[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(hs) != 5 { // commit, root, stg, stg/ns3, the blob
		t.Fatalf("want 5 objects for a one-file change, got %d", len(hs))
	}
}

// rewriteRemote replaces the remote branch with a new history of one commit
// holding files.
func rewriteRemote(t *testing.T, remote string, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	r, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateRemote(&gitcfg.RemoteConfig{Name: "origin", URLs: []string{remote}}); err != nil {
		t.Fatal(err)
	}
	w, _ := r.Worktree()
	for p, c := range files {
		if err := writeFile(filepath.Join(dir, p), c); err != nil {
			t.Fatal(err)
		}
		w.Add(p)
	}
	sig := &object.Signature{Name: "t", Email: "t@t", When: time.Now()}
	if _, err := w.Commit("rewritten", &git.CommitOptions{Author: sig}); err != nil {
		t.Fatal(err)
	}
	if err := r.Push(&git.PushOptions{RefSpecs: []gitcfg.RefSpec{"+refs/heads/master:refs/heads/master"}}); err != nil {
		t.Fatal(err)
	}
}

func TestGitStoreFetchAfterHistoryRewrite(t *testing.T) {
	remote := newRemote(t)
	pushFile(t, remote, "stg/a.yaml", "a\n", "seed")
	g := clusterStore(t, remote, "stg")
	ctx := context.Background()
	stage(t, g, map[string]string{"stg/b.yaml": "b\n"})
	if _, err := g.push(ctx); err != nil {
		t.Fatal(err)
	}
	rewriteRemote(t, remote, map[string]string{"stg/a.yaml": "a\n", "stg/b.yaml": "b\n", "prod/p.yaml": "p\n"})
	head, err := g.fetch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if b, ok, err := g.readFile(head, "stg/b.yaml"); err != nil || !ok || string(b) != "b\n" {
		t.Fatalf("the rewritten head must be readable: %q %v %v", b, ok, err)
	}
	stage(t, g, map[string]string{"stg/c.yaml": "c\n"})
	if _, err := g.push(ctx); err != nil {
		t.Fatal(err)
	}
	if files := remoteFiles(t, remote); len(files) != 4 || files["stg/c.yaml"] != "c\n" || files["prod/p.yaml"] != "p\n" {
		t.Fatalf("remote has %v", files)
	}
}

func TestGitStoreRebaseAfterHistoryRewrite(t *testing.T) {
	remote := newRemote(t)
	pushFile(t, remote, "stg/a.yaml", "a\n", "seed")
	g := clusterStore(t, remote, "stg")
	stage(t, g, map[string]string{"stg/b.yaml": "b\n"})
	rewriteRemote(t, remote, map[string]string{"stg/a.yaml": "a\n", "prod/p.yaml": "p\n"})
	n, err := g.push(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want 1 conflict, got %d", n)
	}
	if files := remoteFiles(t, remote); len(files) != 3 || files["stg/b.yaml"] != "b\n" || files["prod/p.yaml"] != "p\n" {
		t.Fatalf("remote has %v", files)
	}
}
