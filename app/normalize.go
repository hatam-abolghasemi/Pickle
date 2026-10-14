package main

import (
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"
)

// Annotations that are noise or cluster-bound; everything else is kept.
var droppedAnnotations = map[string]bool{
	"kubectl.kubernetes.io/last-applied-configuration": true, // duplicates the object, and holds plaintext for Secrets
	"deployment.kubernetes.io/revision":                true,
	"control-plane.alpha.kubernetes.io/leader":         true,
	"volume.kubernetes.io/selected-node":               true,
}

var droppedAnnotationPrefixes = []string{"pv.kubernetes.io/"}

// header is the informative metadata rendered as YAML comments on top of each file.
type header struct {
	manager   string
	operation string
	changed   string
	argoApp   string
}

func readHeader(u *unstructured.Unstructured) header {
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
	if id := u.GetAnnotations()["argocd.argoproj.io/tracking-id"]; id != "" {
		if i := strings.Index(id, ":"); i > 0 {
			h.argoApp = id[:i]
		}
	} else if argoManaged {
		h.argoApp = u.GetLabels()["app.kubernetes.io/instance"]
	}
	return h
}

func (h header) render() string {
	var b strings.Builder
	if h.manager != "" {
		b.WriteString("# last-manager: " + h.manager)
		if h.operation != "" {
			b.WriteString(" (" + h.operation + ")")
		}
		b.WriteByte('\n')
	}
	if h.changed != "" {
		b.WriteString("# last-changed: " + h.changed + "\n")
	}
	if h.argoApp != "" {
		b.WriteString("# argocd-app: " + h.argoApp + "\n")
	}
	return b.String()
}

// normalize returns a copy of u that can be re-applied to a fresh cluster:
// server-populated, runtime and cluster-bound fields are removed.
func normalize(u *unstructured.Unstructured) *unstructured.Unstructured {
	out := make(map[string]any, len(u.Object))
	for k, v := range u.Object {
		if k == "status" || k == "metadata" {
			continue
		}
		out[k] = runtime.DeepCopyJSONValue(v)
	}

	md := map[string]any{"name": u.GetName()}
	if ns := u.GetNamespace(); ns != "" {
		md["namespace"] = ns
	}
	if l := u.GetLabels(); len(l) > 0 {
		md["labels"] = stringMap(l)
	}
	ann := map[string]any{}
	for k, v := range u.GetAnnotations() {
		if keepAnnotation(k) {
			ann[k] = v
		}
	}
	if len(ann) > 0 {
		md["annotations"] = ann
	}
	out["metadata"] = md

	n := &unstructured.Unstructured{Object: out}
	if n.GetAPIVersion() == "v1" {
		switch n.GetKind() {
		case "Service":
			// Allocated IPs are cluster-bound; "None" (headless) is a real setting and stays.
			if ip, _, _ := unstructured.NestedString(out, "spec", "clusterIP"); ip != "None" {
				unstructured.RemoveNestedField(out, "spec", "clusterIP")
				unstructured.RemoveNestedField(out, "spec", "clusterIPs")
			}
		case "PersistentVolumeClaim":
			unstructured.RemoveNestedField(out, "spec", "volumeName")
		case "PersistentVolume":
			unstructured.RemoveNestedField(out, "spec", "claimRef", "uid")
			unstructured.RemoveNestedField(out, "spec", "claimRef", "resourceVersion")
		}
	}
	return n
}

func keepAnnotation(k string) bool {
	if droppedAnnotations[k] {
		return false
	}
	for _, p := range droppedAnnotationPrefixes {
		if strings.HasPrefix(k, p) {
			return false
		}
	}
	return true
}

func stringMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// secretMask replaces every Secret value. Masking happens in memory before
// rendering, so real values never touch disk. A value change still shows up in Git through
// the header (last-manager / last-changed), and added, removed or renamed keys
// show up as ordinary diff lines.
const secretMask = "****"

func maskSecret(n *unstructured.Unstructured) {
	data, _, _ := unstructured.NestedMap(n.Object, "data")
	if len(data) > 0 {
		masked := make(map[string]any, len(data))
		for k := range data {
			masked[k] = secretMask
		}
		n.Object["data"] = masked
	}
	delete(n.Object, "stringData")
}

// render produces the exact bytes written to the repo.
func render(h header, n *unstructured.Unstructured) ([]byte, error) {
	body, err := yaml.Marshal(n.Object)
	if err != nil {
		return nil, err
	}
	return append([]byte(h.render()), body...), nil
}
