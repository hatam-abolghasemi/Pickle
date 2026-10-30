package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	gitcfg "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

type stubDisco struct{ lists []*metav1.APIResourceList }

func (s stubDisco) ServerPreferredResources() ([]*metav1.APIResourceList, error) { return s.lists, nil }

var rw = metav1.Verbs{"get", "list", "watch"}

var testDisco = stubDisco{lists: []*metav1.APIResourceList{
	{GroupVersion: "v1", APIResources: []metav1.APIResource{
		{Name: "namespaces", Kind: "Namespace", Verbs: rw},
		{Name: "configmaps", Kind: "ConfigMap", Namespaced: true, Verbs: rw},
		{Name: "secrets", Kind: "Secret", Namespaced: true, Verbs: rw},
		{Name: "events", Kind: "Event", Namespaced: true, Verbs: rw},
		{Name: "pods/log", Kind: "Pod", Namespaced: true, Verbs: metav1.Verbs{"get"}},
	}},
	{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{
		{Name: "deployments", Kind: "Deployment", Namespaced: true, Verbs: rw},
		{Name: "replicasets", Kind: "ReplicaSet", Namespaced: true, Verbs: rw},
	}},
	{GroupVersion: "rbac.authorization.k8s.io/v1", APIResources: []metav1.APIResource{
		{Name: "clusterroles", Kind: "ClusterRole", Verbs: rw},
	}},
	{GroupVersion: "example.com/v1", APIResources: []metav1.APIResource{
		{Name: "widgets", Kind: "Widget", Namespaced: true, Verbs: rw},
	}},
}}

func pushFile(t *testing.T, remote, path, content, msg string) {
	t.Helper()
	dir := t.TempDir()
	r, err := git.PlainClone(dir, false, &git.CloneOptions{URL: remote})
	if err != nil {
		if r, err = git.PlainInit(dir, false); err != nil {
			t.Fatal(err)
		}
		if _, err := r.CreateRemote(&gitcfg.RemoteConfig{Name: "origin", URLs: []string{remote}}); err != nil {
			t.Fatal(err)
		}
	}
	full := filepath.Join(dir, path)
	os.MkdirAll(filepath.Dir(full), 0o755)
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	w, _ := r.Worktree()
	w.Add(path)
	sig := &object.Signature{Name: "t", Email: "t@t", When: time.Now()}
	if _, err := w.Commit(msg, &git.CommitOptions{Author: sig}); err != nil {
		t.Fatal(err)
	}
	if err := r.Push(&git.PushOptions{}); err != nil {
		t.Fatal(err)
	}
}

func remoteFiles(t *testing.T, remote string) map[string]string {
	t.Helper()
	dir := t.TempDir()
	if _, err := git.PlainClone(dir, false, &git.CloneOptions{URL: remote}); err != nil {
		return nil
	}
	out := map[string]string{}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			b, _ := os.ReadFile(p)
			out[filepath.ToSlash(rel)] = string(b)
		}
		return nil
	})
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestEngineEndToEnd(t *testing.T) {
	remote := t.TempDir()
	if _, err := git.PlainInit(remote, true); err != nil {
		t.Fatal(err)
	}
	// State from a previous run plus another cluster's directory on the same branch.
	pushFile(t, remote, "prod/payments/configmap-keep.yaml", "kind: ConfigMap\n", "prod")
	pushFile(t, remote, "stg/payments/configmap-gone.yaml", "kind: ConfigMap\n", "old stg")

	cfg := &config{
		Cluster: "stg", RepoDir: filepath.Join(t.TempDir(), "repo"), GitURL: remote, GitBranch: "master",
		AuthorName: "pickle", AuthorEmail: "pickle@stg.local",
		CommitDelay: 100 * time.Millisecond, CommitMaxDelay: time.Second,
		PageSize: 1, PagePause: time.Millisecond,
	}

	listKinds := map[schema.GroupVersionResource]string{
		{Version: "v1", Resource: "namespaces"}:                                       "NamespaceList",
		{Version: "v1", Resource: "configmaps"}:                                       "ConfigMapList",
		{Version: "v1", Resource: "secrets"}:                                          "SecretList",
		{Version: "v1", Resource: "events"}:                                           "EventList",
		{Group: "apps", Version: "v1", Resource: "deployments"}:                       "DeploymentList",
		{Group: "apps", Version: "v1", Resource: "replicasets"}:                       "ReplicaSetList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}: "ClusterRoleList",
		{Group: "example.com", Version: "v1", Resource: "widgets"}:                    "WidgetList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds,
		obj(t, `{apiVersion: v1, kind: Namespace, metadata: {name: kube-system, uid: 0d4c1f3e-stg}}`),
		obj(t, `{apiVersion: v1, kind: Namespace, metadata: {name: payments}}`),
		obj(t, `{apiVersion: v1, kind: ConfigMap, metadata: {name: app-config, namespace: payments}, data: {LOG_LEVEL: info}}`),
		obj(t, `{apiVersion: v1, kind: Secret, type: Opaque, metadata: {name: db, namespace: payments}, data: {PASSWORD: c3VwZXJzZWNyZXQ=}}`),
		obj(t, `{apiVersion: v1, kind: Secret, type: kubernetes.io/service-account-token, metadata: {name: tok, namespace: payments}, data: {token: eHh4}}`),
		obj(t, `{apiVersion: v1, kind: Event, metadata: {name: ev1, namespace: payments}}`),
		obj(t, `{apiVersion: apps/v1, kind: Deployment, metadata: {name: api, namespace: payments},
		       spec: {template: {spec: {containers: [{name: c, env: [{name: P, valueFrom: {secretKeyRef: {name: db, key: PASSWORD}}}]}]}}}}`),
		obj(t, `{apiVersion: apps/v1, kind: ReplicaSet, metadata: {name: api-123, namespace: payments, ownerReferences: [{apiVersion: apps/v1, kind: Deployment, name: api, uid: x}]}}`),
		obj(t, `{apiVersion: rbac.authorization.k8s.io/v1, kind: ClusterRole, metadata: {name: "system:view"}, rules: []}`),
	)

	// An unreadable type must never be missed silently.
	dyn.PrependReactor("list", "widgets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "example.com", Resource: "widgets"}, "", errors.New("no"))
	})

	store, err := openGitStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := newMetrics("stg")
	e, err := newEngine(cfg, dyn, testDisco, store, m)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.run(ctx) }()

	waitFor(t, "initial sync pushed", func() bool {
		f := remoteFiles(t, remote)
		_, gone := f["stg/payments/configmap-gone.yaml"]
		return m.backfillComplete.Load() == 1 && f["stg/payments/deployment-api.yaml"] != "" && !gone
	})
	f := remoteFiles(t, remote)
	for _, p := range []string{
		"stg/_kinds.txt", "stg/_cluster/namespace-payments.yaml", "stg/_cluster/clusterrole-system_view.yaml",
		"stg/payments/configmap-app-config.yaml", "stg/payments/secret-db.yaml", "stg/payments/_secrets-referenced.txt",
		"prod/payments/configmap-keep.yaml", "stg/_unreadable.txt", "stg/_cluster-id.txt",
	} {
		if f[p] == "" {
			t.Errorf("remote missing %s; have %v", p, keys(f))
		}
	}
	for _, p := range []string{"stg/payments/secret-tok.yaml", "stg/payments/event-ev1.yaml", "stg/payments/replicaset-api-123.yaml"} {
		if _, ok := f[p]; ok {
			t.Errorf("%s should not be mirrored", p)
		}
	}
	if s := f["stg/payments/secret-db.yaml"]; strings.Contains(s, "c3VwZXJzZWNyZXQ=") || !strings.Contains(s, "PASSWORD: '****'") {
		t.Errorf("secret not masked:\n%s", s)
	}
	if s := f["stg/_unreadable.txt"]; !strings.Contains(s, "example.com/widgets: forbidden") {
		t.Errorf("bad _unreadable.txt:\n%s", s)
	}
	if s := f["stg/payments/_secrets-referenced.txt"]; !strings.Contains(s, "PASSWORD") || !strings.Contains(s, "deployment/api") {
		t.Errorf("bad secrets index:\n%s", s)
	}

	// Another cluster pushes meanwhile; pickle must build on top of it.
	pushFile(t, remote, "prod/payments/configmap-new.yaml", "kind: ConfigMap\n", "prod 2")

	// Live changes arrive through the watch.
	cms := dyn.Resource(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}).Namespace("payments")
	if _, err := cms.Create(ctx, obj(t, `{apiVersion: v1, kind: ConfigMap, metadata: {name: hotfix, namespace: payments}, data: {A: "1"}}`), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	deps := dyn.Resource(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}).Namespace("payments")
	if err := deps.Delete(ctx, "api", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "live changes pushed", func() bool {
		f := remoteFiles(t, remote)
		_, dep := f["stg/payments/deployment-api.yaml"]
		_, idx := f["stg/payments/_secrets-referenced.txt"]
		return f["stg/payments/configmap-hotfix.yaml"] != "" && !dep && !idx
	})
	f = remoteFiles(t, remote)
	if f["prod/payments/configmap-new.yaml"] == "" || f["prod/payments/configmap-keep.yaml"] == "" {
		t.Error("other cluster's files were lost")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("engine did not stop")
	}

	// Restart with exclusions: files mirrored before must be removed, and
	// excluded kinds and namespaces must not come back.
	cfg2 := *cfg
	cfg2.RepoDir = filepath.Join(t.TempDir(), "repo")
	cfg2.Exclude, _ = parseExclude("ClusterRole")
	cfg2.ExcludeNamespaces = []string{"pay*"}
	store2, err := openGitStore(&cfg2)
	if err != nil {
		t.Fatal(err)
	}
	m2 := newMetrics("stg")
	e2, err := newEngine(&cfg2, dyn, testDisco, store2, m2)
	if err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan error, 1)
	go func() { done2 <- e2.run(ctx2) }()
	waitFor(t, "exclusions applied", func() bool {
		f := remoteFiles(t, remote)
		for p := range f {
			if strings.HasPrefix(p, "stg/payments/") || strings.HasPrefix(p, "stg/_cluster/clusterrole-") || p == "stg/_cluster/namespace-payments.yaml" {
				return false
			}
		}
		return m2.backfillComplete.Load() == 1
	})
	if f := remoteFiles(t, remote); f["prod/payments/configmap-keep.yaml"] == "" {
		t.Error("exclusions must only touch this cluster's directory")
	}
	cancel2()
	<-done2
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestDiscoveryTriggers(t *testing.T) {
	cfg := &config{Cluster: "stg", RepoDir: t.TempDir()}
	e, err := newEngine(cfg, nil, testDisco, nil, newMetrics("stg"))
	if err != nil {
		t.Fatal(err)
	}
	e.backfill = false
	crd := &resource{gvr: schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}, kind: "CustomResourceDefinition", token: "customresourcedefinition"}
	cm := &resource{gvr: schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, kind: "ConfigMap", namespaced: true, token: "configmap"}

	send := func(r *resource, o string) {
		w := watch.NewFake()
		go func() { w.Add(obj(t, o)); w.Stop() }()
		e.consume(context.Background(), r, w, "")
	}
	send(cm, `{apiVersion: v1, kind: ConfigMap, metadata: {name: a, namespace: p}}`)
	select {
	case <-e.rediscover:
		t.Fatal("an ordinary object must not trigger rediscovery")
	default:
	}
	send(crd, `{apiVersion: apiextensions.k8s.io/v1, kind: CustomResourceDefinition, metadata: {name: widgets.example.com}}`)
	select {
	case <-e.rediscover:
	default:
		t.Fatal("a CRD change must trigger rediscovery")
	}
	select {
	case <-e.changed:
	default:
		t.Fatal("a written object must wake the commit loop")
	}
}

func TestMessageColor(t *testing.T) {
	changes := map[string]change{"stg/default/a.yaml": {op: 'A'}}
	paths := []string{"stg/default/a.yaml"}
	e := &engine{cfg: &config{Cluster: "stg"}}
	if got := e.message(paths, changes, ""); !strings.HasPrefix(got, "[stg] 1 added\n") {
		t.Fatalf("no color: %q", got)
	}
	e.cfg.Color = "🟢"
	if got := e.message(paths, changes, ""); !strings.HasPrefix(got, "🟢 [stg] 1 added\n") {
		t.Fatalf("with color: %q", got)
	}
}

func TestHeaderOnlyChangeIsNotWritten(t *testing.T) {
	cfg := &config{Cluster: "stg", RepoDir: t.TempDir()}
	m := newMetrics("stg")
	e, err := newEngine(cfg, nil, testDisco, nil, m)
	if err != nil {
		t.Fatal(err)
	}
	e.backfill = false
	app := &resource{gvr: schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}, kind: "Application", namespaced: true, token: "application"}
	sec := &resource{gvr: schema.GroupVersionResource{Version: "v1", Resource: "secrets"}, kind: "Secret", namespaced: true, token: "secret"}

	at := func(ts, body string) *unstructured.Unstructured {
		return obj(t, `{apiVersion: argoproj.io/v1alpha1, kind: Application, metadata: {name: karma, namespace: argocd,
		  managedFields: [{manager: argocd-application-controller, operation: Update, time: "`+ts+`"}]},
		  spec: {project: `+body+`}, status: {reconciledAt: "`+ts+`"}}`)
	}
	p := e.handle(app, at("2026-10-25T03:13:57Z", "default"), false, true)
	delete(e.dirty, p)
	e.handle(app, at("2026-10-25T03:20:20Z", "default"), false, true)
	if _, ok := e.dirty[p]; ok {
		t.Error("a status-only reconcile rewrote the file")
	}
	b, _ := os.ReadFile(e.abs(p))
	if !strings.Contains(string(b), "03:13:57Z") {
		t.Errorf("header should keep the last real change:\n%s", b)
	}
	e.handle(app, at("2026-10-25T03:25:00Z", "platform"), false, true)
	if _, ok := e.dirty[p]; !ok {
		t.Error("a spec change was not written")
	}
	b, _ = os.ReadFile(e.abs(p))
	if !strings.Contains(string(b), "03:25:00Z") {
		t.Errorf("header not refreshed with the body:\n%s", b)
	}

	st := func(ts string) *unstructured.Unstructured {
		return obj(t, `{apiVersion: v1, kind: Secret, type: Opaque, metadata: {name: db, namespace: payments,
		  managedFields: [{manager: kubectl, operation: Update, time: "`+ts+`"}]}, data: {PASSWORD: eHh4}}`)
	}
	p = e.handle(sec, st("2026-10-25T03:00:00Z"), false, true)
	delete(e.dirty, p)
	e.handle(sec, st("2026-10-25T04:00:00Z"), false, true)
	if _, ok := e.dirty[p]; !ok {
		t.Error("a Secret value change, visible only in the header, was not written")
	}
}

func TestSecretFingerprintChanges(t *testing.T) {
	cfg := &config{Cluster: "stg", RepoDir: t.TempDir(), FingerprintKey: []byte("0123456789abcdef0123456789abcdef")}
	e, err := newEngine(cfg, nil, testDisco, nil, newMetrics("stg"))
	if err != nil {
		t.Fatal(err)
	}
	e.backfill = false
	sec := &resource{gvr: schema.GroupVersionResource{Version: "v1", Resource: "secrets"}, kind: "Secret", namespaced: true, token: "secret"}
	st := func(ts, pw string) *unstructured.Unstructured {
		return obj(t, `{apiVersion: v1, kind: Secret, type: Opaque, metadata: {name: db, namespace: payments,
		  managedFields: [{manager: kubectl, operation: Update, time: "`+ts+`"}]}, data: {PASSWORD: `+pw+`, USER: YWRtaW4=}}`)
	}
	p := e.handle(sec, st("2026-10-25T03:00:00Z", "eHh4"), false, true)
	delete(e.dirty, p)
	before, _ := os.ReadFile(e.abs(p))
	e.handle(sec, st("2026-10-25T04:00:00Z", "eHh4"), false, true)
	if _, ok := e.dirty[p]; ok {
		t.Error("a Secret whose values did not change was rewritten")
	}
	e.handle(sec, st("2026-10-25T05:00:00Z", "eXl5"), false, true)
	if _, ok := e.dirty[p]; !ok {
		t.Fatal("a Secret value change was not written")
	}
	after, _ := os.ReadFile(e.abs(p))
	var changed []string
	bl, al := strings.Split(string(before), "\n"), strings.Split(string(after), "\n")
	for i := range bl {
		if i < len(al) && bl[i] != al[i] && !strings.HasPrefix(bl[i], "#") {
			changed = append(changed, strings.TrimSpace(al[i]))
		}
	}
	if len(changed) != 1 || !strings.HasPrefix(changed[0], "PASSWORD: ") {
		t.Errorf("only the PASSWORD line should change, got %q", changed)
	}
}

func TestChangedAndBackIsNotCommitted(t *testing.T) {
	remote := newRemote(t)
	dir := t.TempDir()
	g, err := openGitStore(&config{Cluster: "stg", RepoDir: dir, GitURL: remote, GitBranch: "master", AuthorName: "p", AuthorEmail: "p@p"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config{Cluster: "stg", RepoDir: dir}
	m := newMetrics("stg")
	e, err := newEngine(cfg, nil, testDisco, g, m)
	if err != nil {
		t.Fatal(err)
	}
	e.backfill = false
	app := &resource{gvr: schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}, kind: "Application", namespaced: true, token: "application"}
	at := func(ts, project string) *unstructured.Unstructured {
		return obj(t, `{apiVersion: argoproj.io/v1alpha1, kind: Application, metadata: {name: crm, namespace: argocd,
		  managedFields: [{manager: argocd-application-controller, operation: Update, time: "`+ts+`"}]},
		  spec: {project: `+project+`}}`)
	}
	ctx := context.Background()
	p := e.handle(app, at("2026-10-28T05:15:58Z", "default"), false, true)
	if err := e.flush(ctx); err != nil {
		t.Fatal(err)
	}
	if m.commits.Load() != 1 {
		t.Fatalf("commits = %d, want 1", m.commits.Load())
	}
	committed, _ := os.ReadFile(e.abs(p))

	e.handle(app, at("2026-10-28T05:20:58Z", "busy"), false, true)
	e.handle(app, at("2026-10-28T05:20:59Z", "default"), false, true)
	if err := e.flush(ctx); err != nil {
		t.Fatal(err)
	}
	if m.commits.Load() != 1 {
		t.Error("a change that was undone before the commit produced a header-only commit")
	}
	if b, _ := os.ReadFile(e.abs(p)); string(b) != string(committed) {
		t.Errorf("file must be back to the committed bytes:\n%s", b)
	}
	if len(e.dirty)+len(e.unpushed) != 0 {
		t.Error("nothing should be left pending")
	}

	e.handle(app, at("2026-10-28T05:30:00Z", "platform"), false, true)
	if err := e.flush(ctx); err != nil {
		t.Fatal(err)
	}
	if m.commits.Load() != 2 {
		t.Fatal("a real change was not committed")
	}
	if s := remoteFiles(t, remote)[p]; !strings.Contains(s, "05:30:00Z") || !strings.Contains(s, "project: platform") {
		t.Errorf("real change not pushed:\n%s", s)
	}
}
