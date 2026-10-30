package main

import (
	"context"
	"os"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestHandleMetaSkipsOwnedAndReadsBareObjects(t *testing.T) {
	podsGVR := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	bare := obj(t, `{apiVersion: v1, kind: Pod, metadata: {name: debug, namespace: team,
	  managedFields: [{manager: kubectl, operation: Update, time: "2026-10-25T03:00:00Z"}]},
	  spec: {containers: [{name: c, image: busybox}]}, status: {phase: Running}}`)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{podsGVR: "PodList"}, bare)
	cfg := &config{Cluster: "stg", RepoDir: t.TempDir()}
	e, err := newEngine(cfg, dyn, testDisco, nil, newMetrics("stg"))
	if err != nil {
		t.Fatal(err)
	}
	e.backfill = false
	pods := &resource{gvr: podsGVR, kind: "Pod", namespaced: true, token: "pod", metaOnly: true}
	ctx := context.Background()
	meta := func(name string, owned bool) *metav1.PartialObjectMetadata {
		m := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team", ResourceVersion: "7"}}
		if owned {
			m.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "web-abc", UID: "u1"}}
		}
		return m
	}

	// Owned: skipped from metadata alone, nothing read, nothing written.
	p := e.handleMeta(ctx, pods, meta("web-abc-1", true), false, true)
	if _, err := os.Stat(e.abs(p)); err == nil || len(e.entries) != 0 {
		t.Fatal("an owned Pod was mirrored")
	}
	if n := len(dyn.Actions()); n != 0 {
		t.Fatalf("an owned Pod cost %d API calls, want 0", n)
	}

	// Bare: read whole and mirrored as usual.
	p = e.handleMeta(ctx, pods, meta("debug", false), false, true)
	b, err := os.ReadFile(e.abs(p))
	if err != nil {
		t.Fatalf("a bare Pod wasn't mirrored: %v", err)
	}
	want, _ := render(readHeader(bare), normalize(bare))
	if string(b) != string(want) {
		t.Fatalf("mirrored differently from a full watch:\n%s\nwant:\n%s", b, want)
	}

	// Gone before it could be read: counted as seen, nothing written.
	if p := e.handleMeta(ctx, pods, meta("vanished", false), false, true); p == "" {
		t.Fatal("no path for a vanished object")
	}
	if len(e.entries) != 1 {
		t.Fatalf("%d entries, want 1", len(e.entries))
	}

	// Deleted: removed using metadata alone.
	calls := len(dyn.Actions())
	e.handleMeta(ctx, pods, meta("debug", false), true, true)
	if _, err := os.Stat(e.abs(p)); err == nil || len(e.entries) != 0 {
		t.Fatal("a deleted bare Pod wasn't removed")
	}
	if len(dyn.Actions()) != calls {
		t.Fatal("a delete shouldn't read the object")
	}
	if c := e.dirty[p]; c.op != 'D' || c.manager != "" {
		// metadata of a deleted object carries no managedFields in this test
		t.Fatalf("delete recorded as %+v", c)
	}
}

func TestParseMetadataWatch(t *testing.T) {
	if m, err := parseMetadataWatch("", false); err != nil || !m["core/pods"] || !m["apps/replicasets"] || !m["batch/jobs"] {
		t.Fatalf("default: %v %v", m, err)
	}
	if m, err := parseMetadataWatch("", true); err != nil || len(m) != 0 {
		t.Fatalf("empty turns it off: %v %v", m, err)
	}
	for _, bad := range []string{"pods", "core/secrets", "/pods", "a/b/c"} {
		if _, err := parseMetadataWatch(bad, true); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
