package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

var claimDisco = stubDisco{lists: []*metav1.APIResourceList{
	{GroupVersion: "v1", APIResources: []metav1.APIResource{
		{Name: "namespaces", Kind: "Namespace", Verbs: rw},
		{Name: "configmaps", Kind: "ConfigMap", Namespaced: true, Verbs: rw},
	}},
}}

func claimCluster(t *testing.T, uid string, objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	t.Helper()
	objs = append(objs, obj(t, `{apiVersion: v1, kind: Namespace, metadata: {name: kube-system, uid: `+uid+`}}`))
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		{Version: "v1", Resource: "namespaces"}: "NamespaceList",
		{Version: "v1", Resource: "configmaps"}: "ConfigMapList",
	}, objs...)
}

func claimEngine(t *testing.T, remote string, dyn *dynamicfake.FakeDynamicClient) (*engine, *metrics) {
	t.Helper()
	cfg := &config{
		Cluster: "stg", RepoDir: filepath.Join(t.TempDir(), "repo"), GitURL: remote, GitBranch: "master",
		AuthorName: "pickle", AuthorEmail: "pickle@stg.local",
		CommitDelay: 50 * time.Millisecond, CommitMaxDelay: 500 * time.Millisecond,
		PageSize: 50, PagePause: time.Millisecond,
	}
	store, err := openGitStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := newMetrics("stg")
	e, err := newEngine(cfg, dyn, claimDisco, store, m)
	if err != nil {
		t.Fatal(err)
	}
	return e, m
}

func TestClaimRefusesAnotherClustersDirectory(t *testing.T) {
	remote := newRemote(t)
	pushFile(t, remote, "stg/"+clusterIDFile, string(renderClaim("uid-real-stg")), "real stg")
	pushFile(t, remote, "stg/team/configmap-real.yaml", "kind: ConfigMap\n", "real stg")
	before := remoteFiles(t, remote)

	dyn := claimCluster(t, "uid-mgmt",
		obj(t, `{apiVersion: v1, kind: Namespace, metadata: {name: team}}`))
	e, _ := claimEngine(t, remote, dyn)
	err := e.run(context.Background())
	if !errors.Is(err, errClusterClaimed) {
		t.Fatalf("run = %v, want a claim error", err)
	}
	if !strings.Contains(err.Error(), "uid-real-stg") || !strings.Contains(err.Error(), "uid-mgmt") {
		t.Errorf("error should name both clusters: %v", err)
	}
	after := remoteFiles(t, remote)
	if len(after) != len(before) {
		t.Fatalf("the remote changed: %v", keys(after))
	}
}

func TestClaimStopsWhenAnotherClusterTakesTheDirectory(t *testing.T) {
	remote := newRemote(t)
	dyn := claimCluster(t, "uid-stg",
		obj(t, `{apiVersion: v1, kind: ConfigMap, metadata: {name: a, namespace: team}, data: {k: v}}`))
	e, m := claimEngine(t, remote, dyn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.run(ctx) }()

	waitFor(t, "initial sync with the claim", func() bool {
		f := remoteFiles(t, remote)
		return m.backfillComplete.Load() == 1 && parseClaim([]byte(f["stg/"+clusterIDFile])) == "uid-stg" && f["stg/team/configmap-a.yaml"] != ""
	})

	// A pickle in another cluster, configured with the same name, claims it.
	pushFile(t, remote, "stg/"+clusterIDFile, string(renderClaim("uid-other")), "other cluster")
	cms := dyn.Resource(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}).Namespace("team")
	if _, err := cms.Create(ctx, obj(t, `{apiVersion: v1, kind: ConfigMap, metadata: {name: b, namespace: team}}`), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, errClusterClaimed) {
			t.Fatalf("run = %v, want a claim error", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("pickle kept running in a directory another cluster claimed")
	}
	f := remoteFiles(t, remote)
	if _, ok := f["stg/team/configmap-b.yaml"]; ok {
		t.Error("pushed into another cluster's directory")
	}
	if parseClaim([]byte(f["stg/"+clusterIDFile])) != "uid-other" {
		t.Error("overwrote the other cluster's claim")
	}
}

func TestClaimIsRestoredWhenRemovedFromTheRepo(t *testing.T) {
	remote := newRemote(t)
	dyn := claimCluster(t, "uid-stg")
	e, m := claimEngine(t, remote, dyn)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.run(ctx) }()
	waitFor(t, "initial sync", func() bool {
		return m.backfillComplete.Load() == 1 && remoteFiles(t, remote)["stg/"+clusterIDFile] != ""
	})
	pushFile(t, remote, "stg/"+clusterIDFile, "\n", "wiped by hand")
	cms := dyn.Resource(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}).Namespace("team")
	if _, err := cms.Create(ctx, obj(t, `{apiVersion: v1, kind: ConfigMap, metadata: {name: c, namespace: team}}`), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "claim restored", func() bool {
		f := remoteFiles(t, remote)
		return f["stg/team/configmap-c.yaml"] != "" && parseClaim([]byte(f["stg/"+clusterIDFile])) == "uid-stg"
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestGitStoreRebaseRefusesAnotherClaim(t *testing.T) {
	remote := newRemote(t)
	pushFile(t, remote, "prod/x.yaml", "x\n", "seed")
	mine, theirs := clusterStore(t, remote, "stg"), clusterStore(t, remote, "stg")
	ctx := context.Background()

	// Both build on the same head; theirs gets there first.
	stage(t, mine, map[string]string{"stg/" + clusterIDFile: string(renderClaim("uid-mine")), "stg/ns/a.yaml": "a\n"})
	stage(t, theirs, map[string]string{"stg/" + clusterIDFile: string(renderClaim("uid-theirs")), "stg/ns/b.yaml": "b\n"})
	if _, err := theirs.push(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := mine.push(ctx)
	if !errors.Is(err, errClusterClaimed) {
		t.Fatalf("push = %v, want a claim error", err)
	}
	f := remoteFiles(t, remote)
	if f["stg/ns/b.yaml"] != "b\n" || f["stg/ns/a.yaml"] != "" {
		t.Fatalf("the other cluster's directory was replaced: %v", keys(f))
	}
}

func TestParseClaim(t *testing.T) {
	if got := parseClaim(renderClaim("abc-123")); got != "abc-123" {
		t.Fatalf("got %q", got)
	}
	if got := parseClaim([]byte("# only a comment\n\n")); got != "" {
		t.Fatalf("got %q", got)
	}
}
