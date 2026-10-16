package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
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
	g, err := openGitStore(&config{RepoDir: dir, GitURL: remote, GitBranch: "master", AuthorName: "p", AuthorEmail: "p@p"})
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
	if err := g.push(ctx); err != nil {
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
