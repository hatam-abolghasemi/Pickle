package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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
	"argocd.argoproj.io/refresh":                       true, // a one-shot request the Argo CD controller removes within seconds
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
	if n.GetKind() == "Application" && strings.HasPrefix(n.GetAPIVersion(), "argoproj.io/") {
		// An in-flight sync request: the controller clears it when the sync ends,
		// and re-applying it on restore would start a sync nobody asked for.
		delete(out, "operation")
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
// rendering, so real values never touch disk. With a fingerprint key, each
// value becomes "****:" plus a short keyed hash of it, so a changed value shows
// up in Git as a changed line under its own key, without revealing the value.
// Without a key every value is "****" and a value change only shows up through
// the header (last-manager / last-changed).
const secretMask = "****"

// fingerprintLen is the number of hex digits kept from the HMAC: enough that
// two different values of one key never look the same in practice.
const fingerprintLen = 12

// secretFields are the only top-level Secret fields that are ever written.
// Anything else, including fields a future API version might add, is dropped:
// masking fails closed rather than trusting a field to be harmless.
var secretFields = map[string]bool{"apiVersion": true, "kind": true, "metadata": true, "type": true, "immutable": true, "data": true}

func maskSecret(n *unstructured.Unstructured, key []byte) {
	for k := range n.Object {
		if !secretFields[k] {
			delete(n.Object, k)
		}
	}
	data, ok := n.Object["data"].(map[string]any)
	if !ok || len(data) == 0 {
		delete(n.Object, "data")
		return
	}
	masked := make(map[string]any, len(data))
	for k, v := range data {
		masked[k] = maskValue(v, key)
	}
	n.Object["data"] = masked
}

func maskValue(v any, key []byte) string {
	s, ok := v.(string)
	if len(key) == 0 || !ok {
		return secretMask
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(s))
	return secretMask + ":" + hex.EncodeToString(mac.Sum(nil))[:fingerprintLen]
}

const secretsID = "core/secrets"

// contentHash decides whether a file needs rewriting. The header alone is not
// a change: controllers such as the Argo CD application controller write
// status through the main resource, which bumps managedFields time on every
// reconcile while the mirrored body stays the same. headerCounts is only set
// for Secrets masked without a fingerprint key, where the header is the only
// sign a value changed.
func contentHash(content []byte, headerCounts bool) [32]byte {
	if headerCounts {
		return sha256.Sum256(content)
	}
	return sha256.Sum256(stripHeader(content))
}

func stripHeader(content []byte) []byte {
	for len(content) > 0 && content[0] == '#' {
		i := bytes.IndexByte(content, '\n')
		if i < 0 {
			return nil
		}
		content = content[i+1:]
	}
	return content
}

// render produces the exact bytes written to the repo.
func render(h header, n *unstructured.Unstructured) ([]byte, error) {
	body, err := yaml.Marshal(n.Object)
	if err != nil {
		return nil, err
	}
	return append([]byte(h.render()), body...), nil
}
