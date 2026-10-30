package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Page cache of the repo.
//
// The kernel charges a file page to the cgroup that first reads or writes it,
// so every file pickle writes into the emptyDir, the mirrored YAML and git's
// objects, stays charged to its pod as page cache, and once read back (a
// commit reads the files it stores, a push reads the objects it sends)
// counts in the pod's working set: in a lab cluster of 6,600 objects, 77 MiB
// of "pickle memory" next to 17 MiB pickle really held, growing with the
// cluster. None of it is read again soon: pickle keeps a hash per object,
// not the objects. So once files are committed and pushed, their pages are
// handed back (fadvise DONTNEED); the next read, if any, comes from disk.

// dropDelay is how long after a write its pages are dropped: the kernel
// keeps pages not yet written back (dirty_expire_centisecs, 30s by default),
// so dropping them right after the write would leave most of them.
var dropDelay = 40 * time.Second

// dropper drops the pages of files some time after they were written. Each
// batch waits dropDelay; one timer serves every pending batch.
type dropper struct {
	mu      sync.Mutex
	pending []dropBatch
	timer   *time.Timer
}

type dropBatch struct {
	due   time.Time
	files []string // dropped one by one
	trees []string // walked when dropped (their files may change meanwhile)
}

// later drops files and every file under trees after dropDelay.
func (d *dropper) later(files, trees []string) {
	if len(files) == 0 && len(trees) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pending = append(d.pending, dropBatch{due: time.Now().Add(dropDelay), files: files, trees: trees})
	if d.timer == nil {
		d.timer = time.AfterFunc(dropDelay, d.run)
	}
}

func (d *dropper) run() {
	d.mu.Lock()
	now := time.Now()
	var due []dropBatch
	keep := d.pending[:0]
	for _, b := range d.pending {
		if b.due.After(now) {
			keep = append(keep, b)
		} else {
			due = append(due, b)
		}
	}
	d.pending = keep
	if len(keep) > 0 {
		d.timer = time.AfterFunc(time.Until(keep[0].due), d.run)
	} else {
		d.timer = nil
	}
	d.mu.Unlock()
	for _, b := range due {
		for _, f := range b.files {
			dropCache(f)
		}
		for _, t := range b.trees {
			dropCacheTree(t)
		}
	}
}

// dropCache asks the kernel to drop path's cached pages. Pages not yet
// written back stay until they are; best effort, errors ignored.
func dropCache(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	_ = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
	f.Close()
}

// dropCacheTree drops the cached pages of every file under dir.
func dropCacheTree(dir string) {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			dropCache(p)
		}
		return nil
	})
}

// readUncached reads a whole file and drops its pages: for files read once,
// such as the repo state loaded at startup.
func readUncached(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		dropCache(path)
	}
	return b, err
}
