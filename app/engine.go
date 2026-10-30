package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/metadata"
)

// Pure runtime churn or always-derived types; never mirrored.
var noise = map[string]bool{
	"core/componentstatuses":   true,
	"core/endpoints":           true,
	"core/events":              true,
	"apps/controllerrevisions": true,
	"certificates.k8s.io/certificatesigningrequests": true,
	"coordination.k8s.io/leasecandidates":            true,
	"coordination.k8s.io/leases":                     true,
	"discovery.k8s.io/endpointslices":                true,
	"events.k8s.io/events":                           true,
	"networking.k8s.io/ipaddresses":                  true,
	"resource.k8s.io/resourceslices":                 true,
	"storage.k8s.io/csinodes":                        true,
	"storage.k8s.io/csistoragecapacities":            true,
	"storage.k8s.io/volumeattachments":               true,
}

func isNoise(group, resource string) bool {
	return noise[groupName(group)+"/"+resource] || group == "metrics.k8s.io"
}

type resource struct {
	gvr        schema.GroupVersionResource
	kind       string
	namespaced bool
	token      string
	// metaOnly: watched and listed as metadata only (see handleMeta).
	metaOnly bool
}

func (r *resource) id() string { return groupName(r.gvr.Group) + "/" + r.gvr.Resource }

func (r *resource) isSecret() bool { return r.gvr.Group == "" && r.gvr.Resource == "secrets" }

type entry struct {
	hash [32]byte
	res  string // resource id; "" if unknown (never auto-deleted)
	// src is the sourceHash the file was last rendered from; zero when not
	// known (loaded from the repo at startup).
	src [32]byte
}

type change struct {
	op      byte // 'A', 'M', 'D'
	manager string
}

type discoverer interface {
	ServerPreferredResources() ([]*metav1.APIResourceList, error)
}

type engine struct {
	cfg   *config
	dyn   dynamic.Interface
	disco discoverer
	// meta reads metadata only, for the types in cfg.MetadataWatch; nil:
	// every type is read whole.
	meta metadata.Interface
	git  *gitStore
	m    *metrics

	flushMu sync.Mutex

	mu         sync.Mutex
	reg        *kindRegistry
	tokens     map[string]string // token -> resource id
	active     map[string]*resource
	order      []*resource
	cancels    map[string]context.CancelFunc
	entries    map[string]entry
	aux        map[string][32]byte // _kinds.txt and _secrets-referenced.txt helper files
	idx        *secretIndex
	dirty      map[string]change
	unpushed   map[string]change
	final      string
	backfill   bool
	touched    map[string]bool   // paths written/deleted by watches during backfill: newer than backfill pages
	visited    map[string]bool   // paths seen by backfill
	backfilled map[string]bool   // resource ids fully backfilled
	brokenAPIs map[string]string // group/version -> why discovery failed for it
	excluded   map[string]bool   // resource ids excluded by PICKLE_EXCLUDE

	changed    chan struct{} // something is waiting to be committed
	rediscover chan struct{} // the set of served types may have changed
}

// Changes to these types can add or remove served resource types, so they
// trigger a rediscovery instead of a timer polling for new types.
var discoveryTriggers = map[string]bool{
	"apiextensions.k8s.io/customresourcedefinitions": true,
	"apiregistration.k8s.io/apiservices":             true,
}

const rediscoveryDelay = 5 * time.Second

func newEngine(cfg *config, dyn dynamic.Interface, disco discoverer, g *gitStore, m *metrics) (*engine, error) {
	e := &engine{
		cfg: cfg, dyn: dyn, disco: disco, git: g, m: m,
		reg: newKindRegistry(), tokens: map[string]string{},
		active: map[string]*resource{}, cancels: map[string]context.CancelFunc{},
		entries: map[string]entry{}, aux: map[string][32]byte{}, idx: newSecretIndex(),
		dirty: map[string]change{}, unpushed: map[string]change{},
		backfill: true, touched: map[string]bool{}, visited: map[string]bool{}, backfilled: map[string]bool{},
		excluded: map[string]bool{},
		changed:  make(chan struct{}, 1), rediscover: make(chan struct{}, 1),
	}
	if data, err := os.ReadFile(e.abs(path.Join(cfg.Cluster, kindsFile))); err == nil {
		if err := e.reg.load(data); err != nil {
			return nil, fmt.Errorf("parse %s: %w", kindsFile, err)
		}
		e.aux[path.Join(cfg.Cluster, kindsFile)] = sha256.Sum256(data)
	}
	return e, nil
}

// headerCounts reports whether a header-only change is a real change for files
// of resource res: only for Secrets masked without a fingerprint key.
func (e *engine) headerCounts(res string) bool {
	return res == secretsID && len(e.cfg.FingerprintKey) == 0
}

func (e *engine) abs(p string) string { return filepath.Join(e.cfg.RepoDir, filepath.FromSlash(p)) }

func (e *engine) run(ctx context.Context) error {
	res, err := e.discover()
	if err != nil {
		return fmt.Errorf("discovery: %w", err)
	}
	if err := e.loadExisting(); err != nil {
		return fmt.Errorf("load repo: %w", err)
	}

	commitCtx, stopCommits := context.WithCancel(context.Background())
	commitsDone := make(chan struct{})
	go func() { e.commitLoop(commitCtx); close(commitsDone) }()
	defer func() {
		stopCommits()
		<-commitsDone
		fctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		e.flush(fctx) // push whatever is pending before exiting
	}()

	e.activateInitial(ctx, res)
	if err := e.runBackfill(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	e.finishBackfill()

	var tick <-chan time.Time
	if e.cfg.RediscoveryInterval > 0 {
		t := time.NewTicker(e.cfg.RediscoveryInterval)
		defer t.Stop()
		tick = t.C
	}
	var settle <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-e.rediscover:
			settle = time.After(rediscoveryDelay) // a Helm chart often installs several CRDs at once
			continue
		case <-settle:
		case <-tick:
		}
		settle = nil
		if res, err := e.discover(); err != nil {
			slog.Warn("rediscovery failed", "err", err)
		} else {
			e.activateLater(ctx, res)
		}
		releaseMemory()
	}
}

func notify(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// ---- discovery & activation ----

func (e *engine) discover() ([]*resource, error) {
	lists, err := e.disco.ServerPreferredResources()
	var gdf *errPartialDiscovery
	if err != nil {
		if !errors.As(err, &gdf) || len(lists) == 0 {
			return nil, err
		}
		slog.Warn("partial discovery (some API groups unavailable)", "err", err)
	}
	broken := map[string]string{}
	if gdf != nil {
		for gv, gerr := range gdf.Groups {
			broken[gv.String()] = "discovery failed: " + gerr.Error()
		}
	}
	var out, excluded []*resource
	for _, l := range lists {
		gv, err := schema.ParseGroupVersion(l.GroupVersion)
		if err != nil {
			continue
		}
		for _, r := range l.APIResources {
			if strings.Contains(r.Name, "/") || !hasVerbs(r.Verbs, "list", "watch") || isNoise(gv.Group, r.Name) {
				continue
			}
			res := &resource{gvr: gv.WithResource(r.Name), kind: r.Kind, namespaced: r.Namespaced}
			res.metaOnly = e.meta != nil && e.cfg.MetadataWatch[res.id()]
			if e.cfg.excluded(gv.Group, r.Name, r.Kind) {
				excluded = append(excluded, res)
				continue
			}
			out = append(out, res)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		gi, gj := out[i].gvr.Group, out[j].gvr.Group
		if bi, bj := isBuiltinGroup(gi), isBuiltinGroup(gj); bi != bj {
			return bi
		}
		if gi != gj {
			return gi < gj
		}
		return out[i].gvr.Resource < out[j].gvr.Resource
	})

	e.mu.Lock()
	defer e.mu.Unlock()
	e.brokenAPIs = broken
	for _, r := range out {
		r.token = e.reg.token(r.kind, r.gvr.Group)
		e.tokens[r.token] = r.id()
	}
	for _, r := range excluded {
		if !e.excluded[r.id()] {
			slog.Info("excluded by PICKLE_EXCLUDE", "resource", r.id(), "kind", r.kind)
		}
		e.excluded[r.id()] = true
		if t, ok := e.reg.existing(r.kind, r.gvr.Group); ok {
			e.tokens[t] = r.id() // lets files mirrored before the exclusion be found and removed
		}
	}
	if e.reg.dirty {
		e.reg.dirty = false
		e.writeAuxLocked(path.Join(e.cfg.Cluster, kindsFile), e.reg.render())
	}
	return out, nil
}

func hasVerbs(have []string, want ...string) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			if h == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// probe checks read access with a 1-item list and returns the collection's current
// resourceVersion, or why the type can't be read.
func (e *engine) probe(ctx context.Context, r *resource) (rv, problem string) {
	one := int64(1)
	l, err := e.dyn.Resource(r.gvr).List(ctx, metav1.ListOptions{Limit: one})
	switch {
	case err == nil:
		return l.GetResourceVersion(), ""
	case apierrors.IsForbidden(err):
		return "", "forbidden"
	case apierrors.IsNotFound(err):
		return "", "not found"
	default:
		slog.Warn("probe failed", "resource", r.id(), "err", err)
		return "", firstLine(err.Error())
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// writeUnreadable keeps <cluster>/_unreadable.txt listing every type that exists
// but can't be read, so a gap is never silent: it shows up as a commit.
func (e *engine) writeUnreadable(problems map[string]string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	lines := make([]string, 0, len(problems)+len(e.brokenAPIs))
	for id, why := range problems {
		lines = append(lines, id+": "+why)
	}
	for gv, why := range e.brokenAPIs {
		lines = append(lines, gv+": "+firstLine(why))
	}
	p := path.Join(e.cfg.Cluster, unreadableFile)
	if len(lines) == 0 {
		e.writeAuxLocked(p, nil)
		return
	}
	sort.Strings(lines)
	content := "# Resource types that exist in this cluster but pickle could not read. Objects of these types are NOT mirrored.\n" +
		strings.Join(lines, "\n") + "\n"
	e.writeAuxLocked(p, []byte(content))
}

// activateInitial starts a live watch per readable type from "now"; the slow
// backfill then fills in existing objects without racing those watches.
func (e *engine) activateInitial(ctx context.Context, res []*resource) {
	problems := map[string]string{}
	for _, r := range res {
		rv, problem := e.probe(ctx, r)
		if problem != "" {
			problems[r.id()] = problem
			continue
		}
		e.startWatch(ctx, r, rv)
	}
	e.m.resourcesForbidden.Store(int64(len(problems)))
	e.writeUnreadable(problems)
	slog.Info("watching", "types", len(e.order), "unreadable", len(problems))
}

// activateLater handles types that appeared (new CRDs, new RBAC) or disappeared.
func (e *engine) activateLater(ctx context.Context, res []*resource) {
	seen := map[string]bool{}
	problems := map[string]string{}
	for _, r := range res {
		seen[r.id()] = true
		e.mu.Lock()
		_, running := e.active[r.id()]
		e.mu.Unlock()
		if running {
			continue
		}
		if _, problem := e.probe(ctx, r); problem != "" {
			problems[r.id()] = problem
			continue
		}
		slog.Info("new readable type", "resource", r.id())
		rv := e.resync(ctx, r) // canonical informer order: list, then watch from the list's RV
		if ctx.Err() != nil {
			return
		}
		e.startWatch(ctx, r, rv)
	}
	e.m.resourcesForbidden.Store(int64(len(problems)))
	e.writeUnreadable(problems)
	e.mu.Lock()
	var gone []*resource
	for id, r := range e.active {
		if !seen[id] {
			gone = append(gone, r)
		}
	}
	e.mu.Unlock()
	for _, r := range gone {
		slog.Info("type no longer served; stopping watch", "resource", r.id())
		e.deactivate(r)
	}
}

func (e *engine) startWatch(ctx context.Context, r *resource, rv string) {
	wctx, cancel := context.WithCancel(ctx)
	e.mu.Lock()
	e.active[r.id()] = r
	e.order = append(e.order, r)
	e.cancels[r.id()] = cancel
	e.m.resourcesWatched.Store(int64(len(e.active)))
	e.mu.Unlock()
	go e.watchLoop(wctx, r, rv)
}

func (e *engine) deactivate(r *resource) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c := e.cancels[r.id()]; c != nil {
		c()
	}
	delete(e.cancels, r.id())
	delete(e.active, r.id())
	for i, o := range e.order {
		if o == r {
			e.order = append(e.order[:i], e.order[i+1:]...)
			break
		}
	}
	e.m.resourcesWatched.Store(int64(len(e.active)))
}

// ---- watching ----

func (e *engine) watchLoop(ctx context.Context, r *resource, rv string) {
	backoff := time.Second
	for ctx.Err() == nil {
		timeout := int64(1800 + rand.IntN(1800)) // resumes from rv, so a reconnect costs one request; spread over 30-60 minutes
		opts := metav1.ListOptions{ResourceVersion: rv, AllowWatchBookmarks: true, TimeoutSeconds: &timeout}
		var w watch.Interface
		var err error
		if r.metaOnly {
			w, err = e.meta.Resource(r.gvr).Watch(ctx, opts)
		} else {
			w, err = e.dyn.Resource(r.gvr).Watch(ctx, opts)
		}
		if err != nil {
			switch {
			case ctx.Err() != nil:
				return
			case apierrors.IsGone(err) || apierrors.IsResourceExpired(err):
				rv = e.resync(ctx, r)
			case apierrors.IsForbidden(err) || apierrors.IsNotFound(err):
				slog.Warn("watch no longer allowed or type removed; stopping", "resource", r.id(), "err", err)
				e.deactivate(r)
				return
			default:
				slog.Warn("watch failed; retrying", "resource", r.id(), "err", err, "in", backoff)
				sleepCtx(ctx, backoff)
				backoff = min(backoff*2, 2*time.Minute)
			}
			continue
		}
		backoff = time.Second
		var expired bool
		rv, expired = e.consume(ctx, r, w, rv)
		w.Stop()
		if expired {
			rv = e.resync(ctx, r)
		} else {
			e.m.watchReconnects.Add(1)
		}
	}
}

func (e *engine) consume(ctx context.Context, r *resource, w watch.Interface, rv string) (string, bool) {
	for ev := range w.ResultChan() {
		switch ev.Type {
		case watch.Added, watch.Modified, watch.Deleted:
			switch o := ev.Object.(type) {
			case *unstructured.Unstructured:
				e.handle(r, o, ev.Type == watch.Deleted, true)
				rv = o.GetResourceVersion()
			case *metav1.PartialObjectMetadata:
				e.handleMeta(ctx, r, o, ev.Type == watch.Deleted, true)
				rv = o.ResourceVersion
			default:
				continue
			}
			if discoveryTriggers[r.id()] {
				notify(e.rediscover)
			}
		case watch.Bookmark:
			if o, ok := ev.Object.(metav1.Object); ok {
				rv = o.GetResourceVersion()
			}
		case watch.Error:
			err := apierrors.FromObject(ev.Object)
			if apierrors.IsGone(err) || apierrors.IsResourceExpired(err) {
				return rv, true
			}
			slog.Warn("watch error event", "resource", r.id(), "err", err)
			return rv, false
		}
	}
	return rv, false
}

// resync relists a whole type (paginated, rate-limited) and drops files of objects
// that no longer exist. Returns the resourceVersion to watch from.
func (e *engine) resync(ctx context.Context, r *resource) string {
	e.m.resyncs.Add(1)
	for ctx.Err() == nil {
		rv, seen, err := e.listAll(ctx, r, "", true)
		if err != nil {
			slog.Warn("resync list failed; retrying", "resource", r.id(), "err", err)
			sleepCtx(ctx, 10*time.Second)
			continue
		}
		e.mu.Lock()
		for p, en := range e.entries {
			if en.res == r.id() && !seen[p] {
				e.removeLocked(p, "")
				if e.backfill {
					e.touched[p] = true
				}
			}
		}
		e.mu.Unlock()
		releaseMemory()
		return rv
	}
	return ""
}

// listAll pages through a type (optionally one namespace) handling every object.
func (e *engine) listAll(ctx context.Context, r *resource, ns string, live bool) (string, map[string]bool, error) {
	if r.metaOnly {
		return e.listAllMeta(ctx, r, ns, live)
	}
	seen := map[string]bool{}
	var ri dynamic.ResourceInterface = e.dyn.Resource(r.gvr)
	if ns != "" {
		ri = e.dyn.Resource(r.gvr).Namespace(ns)
	}
	cont := ""
	for {
		l, err := ri.List(ctx, metav1.ListOptions{Limit: e.cfg.PageSize, Continue: cont})
		if err != nil {
			if cont != "" && apierrors.IsResourceExpired(err) {
				cont, seen = "", map[string]bool{} // continue token expired: start this list over
				continue
			}
			return "", nil, err
		}
		for i := range l.Items {
			seen[e.handle(r, &l.Items[i], false, live)] = true
		}
		if cont = l.GetContinue(); cont == "" {
			return l.GetResourceVersion(), seen, nil
		}
		sleepCtx(ctx, e.cfg.PagePause)
		if ctx.Err() != nil {
			return "", nil, ctx.Err()
		}
	}
}

// listAllMeta is listAll for a metadata-only type.
func (e *engine) listAllMeta(ctx context.Context, r *resource, ns string, live bool) (string, map[string]bool, error) {
	seen := map[string]bool{}
	var ri metadata.ResourceInterface = e.meta.Resource(r.gvr)
	if ns != "" {
		ri = e.meta.Resource(r.gvr).Namespace(ns)
	}
	cont := ""
	for {
		l, err := ri.List(ctx, metav1.ListOptions{Limit: e.cfg.PageSize, Continue: cont})
		if err != nil {
			if cont != "" && apierrors.IsResourceExpired(err) {
				cont, seen = "", map[string]bool{}
				continue
			}
			return "", nil, err
		}
		for i := range l.Items {
			seen[e.handleMeta(ctx, r, &l.Items[i], false, live)] = true
		}
		if cont = l.GetContinue(); cont == "" {
			return l.GetResourceVersion(), seen, nil
		}
		sleepCtx(ctx, e.cfg.PagePause)
		if ctx.Err() != nil {
			return "", nil, ctx.Err()
		}
	}
}

// handleMeta handles an object of a metadata-only type. An owned object is
// skipped from its metadata alone, as handle would skip it; one without an
// owner (a bare Pod, a Job no CronJob made) is read whole and handled as
// usual. Most such objects are owned and change all the time (a Pod's
// status), so this spares decoding their spec and status on every change.
// Returns the object's repo path, like handle.
func (e *engine) handleMeta(ctx context.Context, r *resource, m *metav1.PartialObjectMetadata, deleted, live bool) string {
	if deleted || len(m.OwnerReferences) > 0 {
		return e.handle(r, metaObject(m), deleted, live)
	}
	ri := e.dyn.Resource(r.gvr)
	var u *unstructured.Unstructured
	var err error
	if r.namespaced {
		u, err = ri.Namespace(m.Namespace).Get(ctx, m.Name, metav1.GetOptions{})
	} else {
		u, err = ri.Get(ctx, m.Name, metav1.GetOptions{})
	}
	switch {
	case err == nil:
		return e.handle(r, u, false, live)
	case apierrors.IsNotFound(err):
		// Deleted meanwhile: its delete event follows.
	default:
		slog.Warn("get failed", "resource", r.id(), "namespace", m.Namespace, "name", m.Name, "err", err)
	}
	// Counted as seen either way, so a resync never drops a file for an
	// object that exists.
	return objPath(e.cfg.Cluster, r.namespaced, m.Namespace, r.token, m.Name)
}

// metaObject is the part of an object handle reads to skip or remove it:
// name, namespace, owners, and who changed it last.
func metaObject(m *metav1.PartialObjectMetadata) *unstructured.Unstructured {
	md := map[string]any{"name": m.Name}
	if m.Namespace != "" {
		md["namespace"] = m.Namespace
	}
	if len(m.OwnerReferences) > 0 {
		owners := make([]any, len(m.OwnerReferences))
		for i, o := range m.OwnerReferences {
			owners[i] = map[string]any{"apiVersion": o.APIVersion, "kind": o.Kind, "name": o.Name, "uid": string(o.UID)}
		}
		md["ownerReferences"] = owners
	}
	if len(m.ManagedFields) > 0 {
		mfs := make([]any, 0, len(m.ManagedFields))
		for _, f := range m.ManagedFields {
			mf := map[string]any{"manager": f.Manager, "operation": string(f.Operation)}
			if f.Subresource != "" {
				mf["subresource"] = f.Subresource
			}
			if f.Time != nil {
				mf["time"] = f.Time.UTC().Format(time.RFC3339)
			}
			mfs = append(mfs, mf)
		}
		md["managedFields"] = mfs
	}
	return &unstructured.Unstructured{Object: map[string]any{"metadata": md}}
}

// ---- object handling ----

// handle mirrors one object (or its deletion) to disk. live=true for watch/resync
// events, which always win over backfill pages. Returns the object's repo path.
func (e *engine) handle(r *resource, u *unstructured.Unstructured, deleted, live bool) string {
	p := objPath(e.cfg.Cluster, r.namespaced, u.GetNamespace(), r.token, u.GetName())
	skip := len(u.GetOwnerReferences()) > 0 // derived from a parent we already mirror
	if r.namespaced && e.cfg.namespaceExcluded(u.GetNamespace()) {
		skip = true
	}
	if r.gvr.Group == "" && r.gvr.Resource == "namespaces" && e.cfg.namespaceExcluded(u.GetName()) {
		skip = true
	}
	if r.isSecret() && str(u.Object, "type") == "kubernetes.io/service-account-token" {
		skip = true
	}

	var src []byte
	var srcSum [32]byte
	var h header
	var refs refSet
	headerCounts := e.headerCounts(r.id())
	if !deleted && !skip {
		h = readHeader(u)
		n := normalize(u)
		if r.isSecret() {
			maskSecret(n, e.cfg.FingerprintKey)
		}
		var err error
		if src, err = json.Marshal(n.Object); err != nil {
			slog.Warn("render failed", "path", p, "err", err)
			return p
		}
		srcSum = sourceHash(h, src, headerCounts)
		refs = secretRefs(r.gvr.Group, r.kind, u.Object)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.backfill {
		if live {
			e.touched[p] = true
		} else if e.touched[p] {
			e.visited[p] = true
			return p // a watch already recorded a newer state
		}
		e.visited[p] = true
	}

	if r.namespaced && refKind(r.gvr.Group, r.kind) {
		if e.idx.set(u.GetNamespace(), refObjID(r.kind, u.GetName()), refs) {
			e.writeIndexLocked(u.GetNamespace())
		}
	}

	if deleted || skip {
		if _, tracked := e.entries[p]; tracked {
			e.removeLocked(p, readHeader(u).manager)
		}
		return p
	}
	old, existed := e.entries[p]
	if existed && old.src == srcSum {
		return p // rendered from exactly this before
	}
	content, err := renderJSON(h, src)
	if err != nil {
		slog.Warn("render failed", "path", p, "err", err)
		return p
	}
	sum := contentHash(content, headerCounts)
	if existed && old.hash == sum {
		old.src = srcSum
		e.entries[p] = old
		return p
	}
	if err := e.writeFile(p, content); err != nil {
		slog.Error("write failed", "path", p, "err", err)
		return p
	}
	e.entries[p] = entry{hash: sum, res: r.id(), src: srcSum}
	e.m.written.Add(1)
	op := byte('M')
	if !existed {
		op = 'A'
		e.m.objectsTracked.Add(1)
	}
	e.markLocked(p, change{op: op, manager: h.manager})
	return p
}

func (e *engine) removeLocked(p, manager string) {
	if _, ok := e.entries[p]; !ok {
		return
	}
	if err := os.Remove(e.abs(p)); err != nil && !os.IsNotExist(err) {
		slog.Error("remove failed", "path", p, "err", err)
		return
	}
	delete(e.entries, p)
	e.m.objectsTracked.Add(-1)
	e.m.deleted.Add(1)
	e.markLocked(p, change{op: 'D', manager: manager})
}

func (e *engine) markLocked(p string, c change) {
	if prev, ok := e.dirty[p]; ok && prev.op == 'A' && c.op == 'M' {
		c.op = 'A'
	}
	e.dirty[p] = c
	notify(e.changed)
}

func (e *engine) writeIndexLocked(ns string) {
	e.writeAuxLocked(path.Join(e.cfg.Cluster, ns, secretsIndexFile), e.idx.render(ns))
}

// writeAuxLocked writes (or with nil content removes) a generated helper file.
func (e *engine) writeAuxLocked(p string, content []byte) {
	old, existed := e.aux[p]
	if content == nil {
		if existed {
			if err := os.Remove(e.abs(p)); err != nil && !os.IsNotExist(err) {
				slog.Error("remove failed", "path", p, "err", err)
				return
			}
			delete(e.aux, p)
			e.markLocked(p, change{op: 'D'})
		}
		return
	}
	sum := sha256.Sum256(content)
	if existed && old == sum {
		return
	}
	if err := e.writeFile(p, content); err != nil {
		slog.Error("write failed", "path", p, "err", err)
		return
	}
	e.aux[p] = sum
	op := byte('M')
	if !existed {
		op = 'A'
	}
	e.markLocked(p, change{op: op})
}

// writeFile replaces a file atomically so git never stages a half-written file.
func (e *engine) writeFile(p string, content []byte) error {
	full := e.abs(p)
	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".pickle-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), full)
}

// loadExisting indexes files already in the repo so unchanged objects are not
// rewritten and objects deleted while pickle was down can be cleaned up.
func (e *engine) loadExisting() error {
	root := e.abs(e.cfg.Cluster)
	e.mu.Lock()
	defer e.mu.Unlock()
	err := filepath.WalkDir(root, func(full string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return filepath.SkipAll
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		base := d.Name()
		if strings.HasPrefix(base, ".pickle-") {
			return os.Remove(full) // leftover temp file from a crash
		}
		rel, _ := filepath.Rel(e.cfg.RepoDir, full)
		rel = filepath.ToSlash(rel)
		data, err := readUncached(full)
		if err != nil {
			return err
		}
		if strings.HasPrefix(base, "_") {
			e.aux[rel] = sha256.Sum256(data)
			return nil
		}
		if !strings.HasSuffix(base, ".yaml") {
			return nil
		}
		res := ""
		if t := tokenOf(base, e.tokens); t != "" {
			res = e.tokens[t]
		}
		e.entries[rel] = entry{hash: contentHash(data, e.headerCounts(res)), res: res}
		return nil
	})
	e.m.objectsTracked.Store(int64(len(e.entries)))
	slog.Info("existing repo state loaded", "objects", len(e.entries))
	return err
}

// ---- backfill ----

func (e *engine) runBackfill(ctx context.Context) error {
	e.mu.Lock()
	var clusterScoped, namespaced []*resource
	var nsRes *resource
	for _, r := range e.order {
		if r.namespaced {
			namespaced = append(namespaced, r)
		} else {
			clusterScoped = append(clusterScoped, r)
		}
		if r.gvr.Group == "" && r.gvr.Resource == "namespaces" {
			nsRes = r
		}
	}
	e.mu.Unlock()

	failed := map[string]bool{}
	slog.Info("backfill: cluster-scoped objects", "types", len(clusterScoped))
	for _, r := range clusterScoped {
		if _, _, err := e.listAll(ctx, r, "", false); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Warn("backfill list failed", "resource", r.id(), "err", err)
			failed[r.id()] = true
		}
	}

	var namespaces []string
	if nsRes != nil {
		var err error
		if namespaces, err = e.namespaceNames(ctx, nsRes); err != nil {
			return fmt.Errorf("list namespaces: %w", err)
		}
	} else {
		namespaces = []string{""} // cannot read namespaces: fall back to cluster-wide lists
		slog.Warn("no access to namespaces; backfilling namespaced types cluster-wide")
	}
	kept := namespaces[:0]
	for _, ns := range namespaces {
		if ns == "" || !e.cfg.namespaceExcluded(ns) {
			kept = append(kept, ns)
		}
	}
	namespaces = kept
	e.m.backfillNSTotal.Store(int64(len(namespaces)))
	for i, ns := range namespaces {
		for _, r := range namespaced {
			if _, _, err := e.listAll(ctx, r, ns, false); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				slog.Warn("backfill list failed", "resource", r.id(), "namespace", ns, "err", err)
				failed[r.id()] = true
			}
		}
		e.m.backfillNSDone.Store(int64(i + 1))
		slog.Info("backfill progress", "namespace", ns, "done", i+1, "total", len(namespaces), "objects", e.m.objectsTracked.Load())
	}

	e.mu.Lock()
	for _, r := range append(clusterScoped, namespaced...) {
		if !failed[r.id()] {
			e.backfilled[r.id()] = true
		}
	}
	e.mu.Unlock()
	return nil
}

func (e *engine) namespaceNames(ctx context.Context, nsRes *resource) ([]string, error) {
	var names []string
	cont := ""
	for {
		l, err := e.dyn.Resource(nsRes.gvr).List(ctx, metav1.ListOptions{Limit: 500, Continue: cont})
		if err != nil {
			return nil, err
		}
		for _, it := range l.Items {
			names = append(names, it.GetName())
		}
		if cont = l.GetContinue(); cont == "" {
			sort.Strings(names)
			return names, nil
		}
		sleepCtx(ctx, e.cfg.PagePause)
	}
}

func (e *engine) finishBackfill() {
	e.mu.Lock()
	stale := 0
	for p, en := range e.entries {
		switch {
		case en.res != "" && e.backfilled[en.res] && !e.visited[p] && !e.touched[p]:
		case e.excluded[en.res]:
		case e.cfg.namespaceExcluded(pathNamespace(p)):
		default:
			continue
		}
		e.removeLocked(p, "")
		stale++
	}
	for p := range e.aux {
		if strings.HasSuffix(p, "/"+secretsIndexFile) {
			ns := path.Base(path.Dir(p))
			if len(e.idx.byNS[ns]) == 0 {
				e.writeAuxLocked(p, nil)
			}
		}
	}
	e.backfill = false
	e.touched, e.visited = nil, nil
	e.final = "initial sync complete"
	tracked := len(e.entries)
	e.mu.Unlock()
	notify(e.changed)
	e.m.backfillComplete.Store(1)
	slog.Info("backfill complete", "objects", tracked, "staleRemoved", stale)
	releaseMemory()
}

// ---- committing ----

// commitLoop sleeps until something changes. It then waits for changes to
// settle (CommitDelay after the last one, at most CommitMaxDelay after the first),
// so a Helm upgrade touching many objects becomes one commit. Failed pushes are
// retried with backoff. Nothing runs while the cluster is quiet.
func (e *engine) commitLoop(ctx context.Context) {
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	var first, holdUntil time.Time
	backoff := e.cfg.CommitDelay
	schedule := func() {
		at := time.Now().Add(e.cfg.CommitDelay)
		if limit := first.Add(e.cfg.CommitMaxDelay); at.After(limit) {
			at = limit
		}
		if at.Before(holdUntil) {
			at = holdUntil
		}
		timer.Reset(time.Until(at))
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.changed:
			if first.IsZero() {
				first = time.Now()
			}
			schedule()
		case <-timer.C:
			if err := e.flush(ctx); err != nil {
				holdUntil = time.Now().Add(backoff)
				backoff = min(backoff*2, 5*time.Minute)
				schedule()
				continue
			}
			first, holdUntil, backoff = time.Time{}, time.Time{}, e.cfg.CommitDelay
		}
	}
}

// flush commits everything changed since the last successful push, on top of the
// latest remote head, and pushes. On failure the changes are kept for the next try.
func (e *engine) flush(ctx context.Context) error {
	e.flushMu.Lock()
	defer e.flushMu.Unlock()

	e.mu.Lock()
	idle := len(e.dirty) == 0 && len(e.unpushed) == 0 && e.final == ""
	e.mu.Unlock()
	if idle {
		return nil
	}

	head, err := e.git.fetch(ctx)
	if err != nil {
		e.m.pushFailures.Add(1)
		slog.Warn("git fetch failed; will retry", "err", err)
		return err
	}

	e.mu.Lock()
	changes := e.unpushed
	for p, c := range e.dirty {
		changes[p] = c
	}
	e.dirty, e.unpushed = map[string]change{}, map[string]change{}
	note := e.final
	e.final = ""
	paths := make([]string, 0, len(changes))
	for p := range changes {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	paths = e.settleLocked(head, paths, changes)
	msg := e.message(paths, changes, note)
	committed, err := e.git.commit(head, paths, msg, note != "")
	e.mu.Unlock()

	conflicts := 0
	if err == nil && committed {
		conflicts, err = e.git.push(ctx)
		e.m.pushConflicts.Add(int64(conflicts))
	}
	if err != nil {
		e.mu.Lock()
		for p, c := range changes {
			e.unpushed[p] = c
		}
		if note != "" && e.final == "" {
			e.final = note
		}
		e.mu.Unlock()
		e.m.pushFailures.Add(1)
		slog.Warn("commit/push failed; will retry", "changes", len(changes), "err", err)
		return err
	}
	if committed {
		e.m.commits.Add(1)
		e.m.lastPush.Store(time.Now().Unix())
		slog.Info("pushed", "changes", len(changes), "note", note, "conflicts", conflicts)
		e.git.dropCaches(paths)
		e.git.compact(ctx)
	}
	releaseMemory()
	return nil
}

// settleLocked compares each changed path with the head it is about to be
// committed on and drops what has no net change. An object can change and
// change back between two commits, e.g. an Argo CD sync or refresh request
// that its controller clears seconds later: the body ends up as it was and
// only the header has moved. Such a file is put back to the committed bytes,
// so neither the commit nor the next diff shows it. Add/modify ops are also
// corrected to what the head holds, e.g. an object deleted and recreated
// between commits is a modification, not an addition.
func (e *engine) settleLocked(head plumbing.Hash, paths []string, changes map[string]change) []string {
	tree, err := e.git.treeOf(head)
	if err != nil {
		slog.Warn("cannot read head tree; committing changes as they are", "err", err)
		return paths
	}
	kept := paths[:0]
	for _, p := range paths {
		old, inHead, err := readTreeFile(tree, p)
		if err != nil {
			slog.Warn("cannot read committed file; committing it as it is", "path", p, "err", err)
			kept = append(kept, p)
			continue
		}
		cur, err := os.ReadFile(e.abs(p))
		onDisk := err == nil
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			kept = append(kept, p)
			continue
		}
		c := changes[p]
		switch {
		case !onDisk && !inHead:
			delete(changes, p)
			continue
		case onDisk && inHead:
			if bytes.Equal(cur, old) {
				delete(changes, p)
				continue
			}
			if en, ok := e.entries[p]; ok && contentHash(cur, e.headerCounts(en.res)) == contentHash(old, e.headerCounts(en.res)) {
				if err := e.writeFile(p, old); err != nil {
					slog.Error("restore failed", "path", p, "err", err)
					kept = append(kept, p)
					continue
				}
				delete(changes, p)
				continue
			}
			c.op = 'M'
		case onDisk:
			c.op = 'A'
		default:
			c.op = 'D'
		}
		changes[p] = c
		kept = append(kept, p)
	}
	return kept
}

// pathNamespace returns the namespace directory of a repo path
// (<cluster>/<namespace>/<file>), or "" for cluster-scoped and helper files.
func pathNamespace(p string) string {
	parts := strings.Split(p, "/")
	if len(parts) != 3 || parts[1] == clusterScopedDir {
		return ""
	}
	return parts[1]
}

func (e *engine) message(paths []string, changes map[string]change, note string) string {
	count := map[byte]int{}
	for _, c := range changes {
		count[c.op]++
	}
	var b strings.Builder
	if e.cfg.Color != "" {
		b.WriteString(e.cfg.Color + " ")
	}
	b.WriteString("[" + e.cfg.Cluster + "] ")
	if note != "" && len(changes) == 0 {
		b.WriteString(note)
	} else {
		if e.backfill {
			b.WriteString("backfill: ")
		}
		var parts []string
		for _, op := range []struct {
			op   byte
			word string
		}{{'A', "added"}, {'M', "modified"}, {'D', "deleted"}} {
			if n := count[op.op]; n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n, op.word))
			}
		}
		b.WriteString(strings.Join(parts, ", "))
		if note != "" {
			b.WriteString(" (" + note + ")")
		}
	}
	b.WriteString("\n\n")
	const maxLines = 200
	for i, p := range paths {
		if i == maxLines {
			fmt.Fprintf(&b, "... and %d more\n", len(paths)-maxLines)
			break
		}
		c := changes[p]
		b.WriteString(string(c.op) + " " + strings.TrimPrefix(p, e.cfg.Cluster+"/"))
		if c.manager != "" {
			b.WriteString("  [" + c.manager + "]")
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
