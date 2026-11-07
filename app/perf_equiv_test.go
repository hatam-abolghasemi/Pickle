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

// TestRenderRoundTrips: whatever the order, the YAML reads back as exactly
// the object, tricky strings and numbers included.
func TestRenderRoundTrips(t *testing.T) {
	n := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "a", "namespace": "b", "labels": map[string]any{"z": "1", "a": "2"}},
		"data":     map[string]any{"k": "multi\nline\n", "n": "0123", "t": "true", "y": "yes", "o": "0x1F", "e": "", "s": "*star", "c": "a: b", "h": "#x", "sp": " lead", "u": "سلام", "on": "on", "Off": "Off", "Y": "Y", "b60": "1:30", "b60f": "190:20:30.15", "exp": "1e3", "nul": "null", "tilde": "~", "inf": ".inf", "oct": "0o17", "plus": "+1", "dash": "-", "colon": "a:b", "tab": "a\tb", "trail": "x ", "quote": "'q'", "dq": "\"x\"", "crlf": "a\r\nb", "bad": "\xff"},
		"spec":     map[string]any{"i": int64(3), "big": int64(1) << 53, "f": 1.5, "neg": int64(-7), "l": []any{"x", int64(1), true}, "m": map[string]any{}, "z": []any{}}}}
	got, err := render(header{manager: "kubectl"}, n)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := yaml.Unmarshal(got, &back); err != nil {
		t.Fatal(err)
	}
	want, _ := yaml.Marshal(n.Object)
	again, _ := yaml.Marshal(back)
	if !bytes.Equal(want, again) {
		t.Fatalf("round trip changed the object:\n%s\nfrom:\n%s", again, want)
	}
}
