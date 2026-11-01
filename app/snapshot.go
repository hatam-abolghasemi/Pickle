package main

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"container/list"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"sort"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// The first download of a branch is a depth-1 pack of its whole tree: every
// cluster's directory, of which pickle keeps only its own (see sparseStore).
// go-git's pack parser holds every delta base it resolves, up to 96 MiB, and
// a few hundred bytes per object, so storing that pack took about as much
// memory as the uncompressed repo. Instead the pack is streamed to a file,
// indexed here with about 40 bytes per object and a small cache, and only the
// objects pickle reads are written to the store.

// snapshotCacheBytes bounds the resolved delta bases kept while indexing.
const snapshotCacheBytes = 4 << 20

// downloadSnapshot fetches the head of the branch (depth 1) into st, keeping
// the commit, its root tree and this cluster's directory, and points the
// branch and its remote-tracking ref at it. Returns ZeroHash for an empty
// remote or a missing branch. tmp is a scratch file path for the pack.
func (g *gitStore) downloadSnapshot(ctx context.Context, st *filesystem.Storage, tmp string) (plumbing.Hash, error) {
	sess, ar, err := g.uploadSession(ctx)
	if err != nil || ar == nil {
		return plumbing.ZeroHash, err
	}
	defer sess.Close()
	refs, err := ar.AllReferences()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	ref, err := refs.Reference(plumbing.NewBranchReferenceName(g.branch))
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return plumbing.ZeroHash, nil
	}
	if err != nil {
		return plumbing.ZeroHash, err
	}
	head := ref.Hash()

	req := packp.NewUploadPackRequestFromCapabilities(ar.Capabilities)
	req.Wants = []plumbing.Hash{head}
	req.Depth = packp.DepthCommits(1)
	if err := req.Capabilities.Set(capability.Shallow); err != nil {
		return plumbing.ZeroHash, err
	}
	if ar.Capabilities.Supports(capability.NoProgress) {
		_ = req.Capabilities.Set(capability.NoProgress)
	}
	resp, err := sess.UploadPack(ctx, req)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		resp.Close()
		return plumbing.ZeroHash, err
	}
	defer os.Remove(tmp)
	defer f.Close()
	_, err = io.Copy(f, demux(req.Capabilities, resp))
	resp.Close()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("download: %w", err)
	}
	dropCache(tmp)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return plumbing.ZeroHash, err
	}
	idx, err := indexPack(f)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("index: %w", err)
	}
	keep, err := idx.sparse(head, g.cluster)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if err := idx.writeTo(st, keep); err != nil {
		return plumbing.ZeroHash, err
	}
	for _, name := range []plumbing.ReferenceName{plumbing.NewBranchReferenceName(g.branch), plumbing.NewRemoteReferenceName("origin", g.branch)} {
		if err := st.SetReference(plumbing.NewHashReference(name, head)); err != nil {
			return plumbing.ZeroHash, err
		}
	}
	return head, st.SetShallow([]plumbing.Hash{head})
}

func demux(caps *capability.List, r io.Reader) io.Reader {
	switch {
	case caps.Supports(capability.Sideband64k):
		return sideband.NewDemuxer(sideband.Sideband64k, r)
	case caps.Supports(capability.Sideband):
		return sideband.NewDemuxer(sideband.Sideband, r)
	}
	return r
}

// packIndex locates the objects of a pack file by hash.
type packIndex struct {
	sc     *packfile.Scanner
	offs   []int64               // object offsets, ascending (pack order)
	types  []plumbing.ObjectType // resolved types
	bases  []int32               // the delta base of each object, -1 for none
	kids   []uint16              // deltas based on each object
	byHash map[plumbing.Hash]int32
	cache  *bytesLRU
}

func indexPack(r io.ReadSeeker) (*packIndex, error) {
	sc := packfile.NewScanner(r)
	_, n, err := sc.Header()
	if err != nil {
		return nil, err
	}
	p := &packIndex{
		sc:     sc,
		offs:   make([]int64, 0, n),
		types:  make([]plumbing.ObjectType, 0, n),
		bases:  make([]int32, 0, n),
		kids:   make([]uint16, n),
		byHash: make(map[plumbing.Hash]int32, n),
		cache:  newBytesLRU(snapshotCacheBytes),
	}
	refBases := map[int32]plumbing.Hash{}
	for i := int32(0); i < int32(n); i++ {
		oh, err := sc.NextObjectHeader()
		if err != nil {
			return nil, err
		}
		p.offs = append(p.offs, oh.Offset)
		p.types = append(p.types, oh.Type)
		switch oh.Type {
		case plumbing.OFSDeltaObject:
			b := p.at(oh.OffsetReference)
			if b < 0 {
				return nil, fmt.Errorf("delta base at %d not found", oh.OffsetReference)
			}
			p.bases = append(p.bases, b)
			p.kids[b]++
		case plumbing.REFDeltaObject:
			p.bases = append(p.bases, -1)
			refBases[i] = oh.Reference
		default:
			p.bases = append(p.bases, -1)
			h := plumbing.NewHasher(oh.Type, oh.Length)
			if _, _, err := sc.NextObject(h); err != nil {
				return nil, err
			}
			p.byHash[h.Sum()] = i
		}
	}
	for i, h := range refBases {
		b, ok := p.byHash[h]
		if !ok {
			return nil, fmt.Errorf("delta base %s not in the pack", h)
		}
		p.bases[i] = b
		p.kids[b]++
	}
	for i := range p.offs {
		if !p.types[i].IsDelta() {
			continue
		}
		t, content, err := p.resolve(int32(i))
		if err != nil {
			return nil, err
		}
		p.byHash[plumbing.ComputeHash(t, content)] = int32(i)
	}
	p.cache = newBytesLRU(snapshotCacheBytes)
	return p, nil
}

// at returns the index of the object at offset off, or -1.
func (p *packIndex) at(off int64) int32 {
	i := sort.Search(len(p.offs), func(i int) bool { return p.offs[i] >= off })
	if i < len(p.offs) && p.offs[i] == off {
		return int32(i)
	}
	return -1
}

// resolve returns the type and content of object i. Contents that other
// deltas build on are cached; the rest are read again when needed.
func (p *packIndex) resolve(i int32) (plumbing.ObjectType, []byte, error) {
	if b, ok := p.cache.get(i); ok {
		return p.types[i], b, nil
	}
	if _, err := p.sc.SeekObjectHeader(p.offs[i]); err != nil {
		return 0, nil, err
	}
	var buf bytes.Buffer
	if _, _, err := p.sc.NextObject(&buf); err != nil {
		return 0, nil, err
	}
	content := buf.Bytes()
	if b := p.bases[i]; b >= 0 {
		t, base, err := p.resolve(b)
		if err != nil {
			return 0, nil, err
		}
		if content, err = packfile.PatchDelta(base, content); err != nil {
			return 0, nil, err
		}
		p.types[i] = t
	}
	if p.kids[i] > 0 {
		p.cache.put(i, content)
	}
	return p.types[i], content, nil
}

func (p *packIndex) object(h plumbing.Hash) (plumbing.ObjectType, []byte, error) {
	i, ok := p.byHash[h]
	if !ok {
		return 0, nil, fmt.Errorf("%w: %s", plumbing.ErrObjectNotFound, h)
	}
	return p.resolve(i)
}

func (p *packIndex) tree(h plumbing.Hash) (*object.Tree, error) {
	t, content, err := p.object(h)
	if err != nil {
		return nil, err
	}
	if t != plumbing.TreeObject {
		return nil, fmt.Errorf("%s is a %s, not a tree", h, t)
	}
	obj := &plumbing.MemoryObject{}
	obj.SetType(t)
	obj.Write(content)
	tr := &object.Tree{}
	return tr, tr.Decode(obj)
}

// sparse lists the objects pickle keeps of commit head: the commit, its root
// tree, and every tree and blob under the cluster's directory.
func (p *packIndex) sparse(head plumbing.Hash, cluster string) ([]plumbing.Hash, error) {
	t, content, err := p.object(head)
	if err != nil {
		return nil, err
	}
	if t != plumbing.CommitObject {
		return nil, fmt.Errorf("%s is a %s, not a commit", head, t)
	}
	obj := &plumbing.MemoryObject{}
	obj.SetType(t)
	obj.Write(content)
	var c object.Commit
	if err := c.Decode(obj); err != nil {
		return nil, err
	}
	keep := []plumbing.Hash{head, c.TreeHash}
	root, err := p.tree(c.TreeHash)
	if err != nil {
		return nil, err
	}
	var walk func(h plumbing.Hash) error
	walk = func(h plumbing.Hash) error {
		keep = append(keep, h)
		tr, err := p.tree(h)
		if err != nil {
			return err
		}
		for _, e := range tr.Entries {
			switch {
			case e.Mode == filemode.Dir:
				if err := walk(e.Hash); err != nil {
					return err
				}
			case e.Mode.IsFile():
				keep = append(keep, e.Hash)
			}
		}
		return nil
	}
	for _, e := range root.Entries {
		if e.Name == cluster && e.Mode == filemode.Dir {
			if err := walk(e.Hash); err != nil {
				return nil, err
			}
		}
	}
	return keep, nil
}

// writeTo stores the objects hs (see writePack).
func (p *packIndex) writeTo(st *filesystem.Storage, hs []plumbing.Hash) error {
	return writePack(st, hs, p.object)
}

// writePack stores the objects hs, read with get, as one pack of whole
// (undeltified) objects, one object in memory at a time.
func writePack(st *filesystem.Storage, hs []plumbing.Hash, get func(plumbing.Hash) (plumbing.ObjectType, []byte, error)) error {
	seen := make(map[plumbing.Hash]bool, len(hs))
	uniq := make([]plumbing.Hash, 0, len(hs))
	for _, h := range hs {
		if !seen[h] {
			seen[h] = true
			uniq = append(uniq, h)
		}
	}
	if len(uniq) == 0 {
		return nil
	}
	w, err := st.PackfileWriter()
	if err != nil {
		return err
	}
	pw := newPackWriter(w)
	err = pw.header(uint32(len(uniq)))
	for _, h := range uniq {
		if err != nil {
			break
		}
		var t plumbing.ObjectType
		var content []byte
		if t, content, err = get(h); err == nil {
			err = pw.object(t, content)
		}
	}
	if err == nil {
		err = pw.close()
	}
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	return err
}

// packWriter writes a version 2 pack of whole objects.
type packWriter struct {
	w   *bufio.Writer
	out io.Writer
	sum hash.Hash
	z   *zlib.Writer
}

func newPackWriter(w io.Writer) *packWriter {
	sum := sha1.New()
	out := io.MultiWriter(w, sum)
	bw := bufio.NewWriterSize(out, 32<<10)
	return &packWriter{w: bw, out: w, sum: sum, z: zlib.NewWriter(bw)}
}

func (pw *packWriter) header(n uint32) error {
	var b [12]byte
	copy(b[:], "PACK")
	binary.BigEndian.PutUint32(b[4:], 2)
	binary.BigEndian.PutUint32(b[8:], n)
	_, err := pw.w.Write(b[:])
	return err
}

func (pw *packWriter) object(t plumbing.ObjectType, content []byte) error {
	size := uint64(len(content))
	var hdr [16]byte
	n := 0
	c := byte(t)<<4 | byte(size&0x0f)
	size >>= 4
	for size > 0 {
		hdr[n] = c | 0x80
		n++
		c = byte(size & 0x7f)
		size >>= 7
	}
	hdr[n] = c
	n++
	if _, err := pw.w.Write(hdr[:n]); err != nil {
		return err
	}
	pw.z.Reset(pw.w)
	if _, err := pw.z.Write(content); err != nil {
		return err
	}
	return pw.z.Close()
}

func (pw *packWriter) close() error {
	if err := pw.w.Flush(); err != nil {
		return err
	}
	_, err := pw.out.Write(pw.sum.Sum(nil))
	return err
}

// bytesLRU is a least-recently-used cache of contents by object index,
// bounded in bytes.
type bytesLRU struct {
	max, size int
	ll        *list.List
	items     map[int32]*list.Element
}

type lruItem struct {
	key int32
	val []byte
}

func newBytesLRU(max int) *bytesLRU {
	return &bytesLRU{max: max, ll: list.New(), items: map[int32]*list.Element{}}
}

func (c *bytesLRU) get(k int32) ([]byte, bool) {
	if e, ok := c.items[k]; ok {
		c.ll.MoveToFront(e)
		return e.Value.(*lruItem).val, true
	}
	return nil, false
}

func (c *bytesLRU) put(k int32, v []byte) {
	if len(v) > c.max/4 {
		return
	}
	if _, ok := c.items[k]; ok {
		return
	}
	c.items[k] = c.ll.PushFront(&lruItem{k, v})
	c.size += len(v)
	for c.size > c.max {
		e := c.ll.Back()
		it := e.Value.(*lruItem)
		c.ll.Remove(e)
		delete(c.items, it.key)
		c.size -= len(it.val)
	}
}
