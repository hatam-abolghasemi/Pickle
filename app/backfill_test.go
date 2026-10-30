package main

import (
	"context"
	"os"
	"path"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestSecretsIndexWrittenOnlyOnceBackfillIsComplete(t *testing.T) {
	cfg := &config{Cluster: "stg", RepoDir: t.TempDir()}
	e, err := newEngine(cfg, nil, testDisco, nil, newMetrics("stg"))
	if err != nil {
		t.Fatal(err)
	}
	deps := &resource{gvr: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, kind: "Deployment", namespaced: true, token: "deployment"}
	dep := func(name, secret string) *unstructured.Unstructured {
		return obj(t, `{apiVersion: apps/v1, kind: Deployment, metadata: {name: `+name+`, namespace: team},
		  spec: {template: {spec: {containers: [{name: c, envFrom: [{secretRef: {name: `+secret+`}}]}]}}}}`)
	}
	idx := path.Join("stg", "team", secretsIndexFile)

	// A live event during the backfill, then the backfill's own pages: the
	// index must not be written while it only knows part of the namespace.
	e.handle(deps, dep("web", "web-env"), false, true)
	e.handle(deps, dep("worker", "worker-env"), false, false)
	if _, err := os.Stat(e.abs(idx)); err == nil {
		t.Fatal("index written before the backfill was complete")
	}
	if _, pending := e.dirty[idx]; pending {
		t.Fatal("partial index queued for commit")
	}

	e.finishBackfill()
	b, err := os.ReadFile(e.abs(idx))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"web-env", "worker-env", "deployment/web", "deployment/worker"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("index lacks %s:\n%s", want, b)
		}
	}

	// After the backfill, changes update it right away.
	e.handle(deps, dep("cron", "cron-env"), false, true)
	if b, _ := os.ReadFile(e.abs(idx)); !strings.Contains(string(b), "cron-env") {
		t.Errorf("index not updated after the backfill:\n%s", b)
	}
}

func TestBackfillListsEachTypeOnceAcrossNamespaces(t *testing.T) {
	var objs []runtime.Object
	for _, ns := range []string{"a", "b", "c", "d", "e"} {
		objs = append(objs, obj(t, `{apiVersion: v1, kind: Namespace, metadata: {name: `+ns+`}}`),
			obj(t, `{apiVersion: v1, kind: ConfigMap, metadata: {name: cm, namespace: `+ns+`}}`))
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		{Version: "v1", Resource: "namespaces"}: "NamespaceList",
		{Version: "v1", Resource: "configmaps"}: "ConfigMapList",
	}, objs...)
	cfg := &config{Cluster: "stg", RepoDir: t.TempDir(), PageSize: 50}
	e, err := newEngine(cfg, dyn, claimDisco, nil, newMetrics("stg"))
	if err != nil {
		t.Fatal(err)
	}
	e.order = []*resource{
		{gvr: schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}, kind: "Namespace", token: "namespace"},
		{gvr: schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, kind: "ConfigMap", namespaced: true, token: "configmap"},
	}
	if err := e.runBackfill(context.Background()); err != nil {
		t.Fatal(err)
	}
	lists := 0
	for _, a := range dyn.Actions() {
		if l, ok := a.(clienttesting.ListAction); ok {
			lists++
			if l.GetNamespace() != "" {
				t.Errorf("listed %s in namespace %s", l.GetResource().Resource, l.GetNamespace())
			}
		}
	}
	if lists != 2 {
		t.Errorf("%d lists for 2 types, want 2", lists)
	}
	if len(e.entries) != 10 {
		t.Errorf("%d objects mirrored, want 10", len(e.entries))
	}
	if !e.backfilled["core/configmaps"] {
		t.Error("configmaps not marked as backfilled")
	}
}
