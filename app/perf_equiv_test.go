package main

import (
	"bytes"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// headerViaTyped is readHeader as it was, through GetManagedFields.
func headerViaTyped(u *unstructured.Unstructured) header {
	var h header
	var latest time.Time
	argoManaged := false
	for _, mf := range u.GetManagedFields() {
		if mf.Manager == "argocd-controller" || mf.Manager == "argocd-application-controller" {
			argoManaged = true
		}
		if mf.Subresource == "status" || mf.Time == nil {
			continue
		}
		if t := mf.Time.Time; h.manager == "" || t.After(latest) {
			latest, h.manager, h.operation = t, mf.Manager, string(mf.Operation)
		}
	}
	if !latest.IsZero() {
		h.changed = latest.UTC().Format(time.RFC3339)
	}
	if id := u.GetAnnotations()["argocd.argoproj.io/tracking-id"]; id == "" && argoManaged {
		h.argoApp = u.GetLabels()["app.kubernetes.io/instance"]
	}
	return h
}

func TestReadHeaderMatchesTypedManagedFields(t *testing.T) {
	mf := func(manager, op, sub, ts string) map[string]any {
		m := map[string]any{"manager": manager, "operation": op, "apiVersion": "apps/v1", "fieldsType": "FieldsV1",
			"fieldsV1": map[string]any{"f:spec": map[string]any{"f:replicas": map[string]any{}}}}
		if sub != "" {
			m["subresource"] = sub
		}
		if ts != "" {
			m["time"] = ts
		}
		return m
	}
	for i, fields := range [][]any{
		nil,
		{mf("kubectl-client-side-apply", "Update", "", "2026-10-01T10:00:00Z")},
		{mf("helm", "Update", "", "2026-10-01T10:00:00Z"), mf("kube-controller-manager", "Update", "status", "2026-10-02T10:00:00Z"), mf("kubectl", "Apply", "", "2026-10-03T09:00:00Z")},
		{mf("argocd-controller", "Apply", "", "2026-10-01T10:00:00Z"), mf("hpa", "Update", "scale", "2026-10-04T10:00:00Z")},
		{mf("a", "Update", "", ""), mf("b", "Update", "", "2026-10-01T10:00:00Z")},
		{mf("b", "Update", "", "2026-10-01T10:00:00Z"), mf("c", "Update", "", "2026-10-01T10:00:00Z")}, // same time: first wins
	} {
		u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]any{"name": "x", "namespace": "y", "managedFields": fields, "labels": map[string]any{"app.kubernetes.io/instance": "shop"}}}}
		if got, want := readHeader(u), headerViaTyped(u); got != want {
			t.Errorf("case %d: got %+v, want %+v", i, got, want)
		}
	}
}

func TestRenderJSONMatchesYAMLMarshal(t *testing.T) {
	n := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "a", "namespace": "b", "labels": map[string]any{"z": "1", "a": "2"}},
		"data":     map[string]any{"k": "multi\nline", "n": "0123", "t": "true"}, "spec": map[string]any{"i": int64(3), "f": 1.5, "l": []any{"x", nil}}}}
	h := header{manager: "kubectl", operation: "Apply", changed: "2026-10-01T10:00:00Z"}
	got, err := render(h, n)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := yaml.Marshal(n.Object)
	if want := append([]byte(h.render()), body...); !bytes.Equal(got, want) {
		t.Fatalf("render changed:\n%s\nwant:\n%s", got, want)
	}
}
