package main

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func obj(t *testing.T, y string) *unstructured.Unstructured {
	t.Helper()
	m := map[string]any{}
	if err := yaml.Unmarshal([]byte(y), &m); err != nil {
		t.Fatal(err)
	}
	return &unstructured.Unstructured{Object: m}
}

const deployYAML = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: payments-api
  namespace: payments
  uid: 123
  resourceVersion: "999"
  generation: 4
  creationTimestamp: "2026-01-01T00:00:00Z"
  finalizers: [foo]
  labels: {app: payments}
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: "{...}"
    deployment.kubernetes.io/revision: "7"
    team: core
  managedFields:
  - manager: argocd-controller
    operation: Apply
    time: "2026-10-01T10:00:00Z"
  - manager: kubectl-edit
    operation: Update
    time: "2026-10-14T17:52:03Z"
  - manager: kube-controller-manager
    operation: Update
    subresource: status
    time: "2026-10-14T18:00:00Z"
spec:
  replicas: 3
status:
  readyReplicas: 3
`

func TestNormalizeDeployment(t *testing.T) {
	u := obj(t, deployYAML)
	out, err := render(readHeader(u), normalize(u))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"# last-manager: kubectl-edit (Update)\n",
		"# last-changed: 2026-10-14T17:52:03Z\n",
		"team: core", "app: payments", "replicas: 3", "namespace: payments",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	for _, bad := range []string{"uid", "resourceVersion", "generation", "creationTimestamp", "finalizers",
		"managedFields", "last-applied", "revision", "status", "readyReplicas"} {
		if strings.Contains(s, bad) {
			t.Errorf("should not contain %q:\n%s", bad, s)
		}
	}
	// argo managed but no tracking annotation and no instance label -> no argocd-app line
	if strings.Contains(s, "argocd-app") {
		t.Errorf("unexpected argocd-app line")
	}
}

func TestArgoTrackingID(t *testing.T) {
	u := obj(t, `
apiVersion: v1
kind: ConfigMap
metadata:
  name: x
  annotations: {argocd.argoproj.io/tracking-id: "payments-api:/ConfigMap:payments/x"}
`)
	if got := readHeader(u).argoApp; got != "payments-api" {
		t.Fatalf("argoApp = %q", got)
	}
}

func TestServiceIPs(t *testing.T) {
	normal := normalize(obj(t, `{apiVersion: v1, kind: Service, metadata: {name: a}, spec: {clusterIP: 10.0.0.1, clusterIPs: [10.0.0.1], ports: [{port: 80}]}}`))
	if _, ok, _ := unstructured.NestedFieldNoCopy(normal.Object, "spec", "clusterIP"); ok {
		t.Error("allocated clusterIP should be stripped")
	}
	headless := normalize(obj(t, `{apiVersion: v1, kind: Service, metadata: {name: a}, spec: {clusterIP: None}}`))
	if ip, _, _ := unstructured.NestedString(headless.Object, "spec", "clusterIP"); ip != "None" {
		t.Error("headless clusterIP: None must be kept")
	}
}

func TestMaskSecret(t *testing.T) {
	n := normalize(obj(t, `{apiVersion: v1, kind: Secret, type: Opaque, metadata: {name: db, namespace: p},
		data: {PASSWORD: c2VjcmV0, USER: YWRtaW4=}, stringData: {X: plain}}`))
	maskSecret(n)
	data, _, _ := unstructured.NestedStringMap(n.Object, "data")
	if len(data) != 2 || data["PASSWORD"] != "****" || data["USER"] != "****" {
		t.Fatalf("keys must stay, values must be ****: %v", data)
	}
	if _, ok := n.Object["stringData"]; ok {
		t.Fatal("stringData must be dropped")
	}
}

func TestMaskSecretFailsClosed(t *testing.T) {
	n := normalize(obj(t, `{apiVersion: v1, kind: Secret, type: Opaque, immutable: true, metadata: {name: db, namespace: p},
		data: {A: c2VjcmV0}, futureField: {token: plain}}`))
	maskSecret(n)
	if _, ok := n.Object["futureField"]; ok {
		t.Fatal("unknown Secret fields must be dropped")
	}
	if n.Object["immutable"] != true || n.Object["type"] != "Opaque" {
		t.Fatalf("known fields must stay: %v", n.Object)
	}

	odd := normalize(obj(t, `{apiVersion: v1, kind: Secret, metadata: {name: db, namespace: p}, data: plainstring}`))
	maskSecret(odd)
	if _, ok := odd.Object["data"]; ok {
		t.Fatal("data that is not a map must be dropped, not written")
	}
}
