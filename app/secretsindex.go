package main

import (
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// Without any access to Secrets, pickle still knows which Secrets (and keys)
// each namespace needs: workloads declare them. This is the restore checklist.

type refSet map[string]map[string]bool // secret name -> keys (may be empty)

func (r refSet) add(secret, key string) {
	if secret == "" {
		return
	}
	if r[secret] == nil {
		r[secret] = map[string]bool{}
	}
	if key != "" {
		r[secret][key] = true
	}
}

// secretRefs extracts Secret references from built-in kinds; nil for other kinds.
func secretRefs(group, kind string, obj map[string]any) refSet {
	var podPath []string
	switch {
	case group == "" && (kind == "Pod"):
		podPath = []string{"spec"}
	case group == "" && kind == "ReplicationController",
		group == "apps" && (kind == "Deployment" || kind == "StatefulSet" || kind == "DaemonSet" || kind == "ReplicaSet"),
		group == "batch" && kind == "Job":
		podPath = []string{"spec", "template", "spec"}
	case group == "batch" && kind == "CronJob":
		podPath = []string{"spec", "jobTemplate", "spec", "template", "spec"}
	case group == "networking.k8s.io" && kind == "Ingress":
		r := refSet{}
		for _, t := range slice(obj, "spec", "tls") {
			r.add(str(t, "secretName"), "")
		}
		return r
	case group == "" && kind == "ServiceAccount":
		r := refSet{}
		for _, s := range slice(obj, "imagePullSecrets") {
			r.add(str(s, "name"), "")
		}
		for _, s := range slice(obj, "secrets") {
			r.add(str(s, "name"), "")
		}
		return r
	default:
		return nil
	}

	r := refSet{}
	spec, ok, _ := unstructured.NestedMap(obj, podPath...)
	if !ok {
		return r
	}
	for _, s := range slice(spec, "imagePullSecrets") {
		r.add(str(s, "name"), "")
	}
	for _, field := range []string{"initContainers", "containers", "ephemeralContainers"} {
		for _, c := range slice(spec, field) {
			for _, e := range slice(c, "env") {
				if ref, ok := nested(e, "valueFrom", "secretKeyRef"); ok {
					r.add(str(ref, "name"), str(ref, "key"))
				}
			}
			for _, e := range slice(c, "envFrom") {
				if ref, ok := nested(e, "secretRef"); ok {
					r.add(str(ref, "name"), "")
				}
			}
		}
	}
	for _, v := range slice(spec, "volumes") {
		if s, ok := nested(v, "secret"); ok {
			addWithItems(r, str(s, "secretName"), s)
		}
		if p, ok := nested(v, "projected"); ok {
			for _, src := range slice(p, "sources") {
				if s, ok := nested(src, "secret"); ok {
					addWithItems(r, str(s, "name"), s)
				}
			}
		}
	}
	return r
}

// refKind reports whether objects of this kind can reference Secrets.
func refKind(group, kind string) bool { return secretRefs(group, kind, nil) != nil }

func addWithItems(r refSet, name string, src map[string]any) {
	r.add(name, "")
	for _, it := range slice(src, "items") {
		r.add(name, str(it, "key"))
	}
}

// secretIndex: namespace -> "kind/name" of the referencing object -> refs.
type secretIndex struct {
	byNS map[string]map[string]refSet
}

func newSecretIndex() *secretIndex { return &secretIndex{byNS: map[string]map[string]refSet{}} }

// set records refs for one object (nil/empty removes it); reports whether anything changed.
func (x *secretIndex) set(ns, obj string, refs refSet) bool {
	cur := x.byNS[ns]
	old, had := cur[obj]
	if len(refs) == 0 {
		if !had {
			return false
		}
		delete(cur, obj)
		if len(cur) == 0 {
			delete(x.byNS, ns)
		}
		return true
	}
	if had && equalRefs(old, refs) {
		return false
	}
	if cur == nil {
		cur = map[string]refSet{}
		x.byNS[ns] = cur
	}
	cur[obj] = refs
	return true
}

type indexEntry struct {
	Keys   []string `json:"keys,omitempty"`
	UsedBy []string `json:"used-by"`
}

// render returns the namespace's index file, or nil when nothing references a Secret.
func (x *secretIndex) render(ns string) []byte {
	objs := x.byNS[ns]
	if len(objs) == 0 {
		return nil
	}
	merged := map[string]*indexEntry{}
	keys := map[string]map[string]bool{}
	for obj, refs := range objs {
		for secret, ks := range refs {
			e := merged[secret]
			if e == nil {
				e = &indexEntry{}
				merged[secret] = e
				keys[secret] = map[string]bool{}
			}
			e.UsedBy = append(e.UsedBy, obj)
			for k := range ks {
				keys[secret][k] = true
			}
		}
	}
	for secret, e := range merged {
		sort.Strings(e.UsedBy)
		for k := range keys[secret] {
			e.Keys = append(e.Keys, k)
		}
		sort.Strings(e.Keys)
	}
	body, err := yaml.Marshal(merged)
	if err != nil {
		return nil
	}
	return append([]byte("# Secrets referenced by objects in this namespace: names and keys only, never values.\n"), body...)
}

func equalRefs(a, b refSet) bool {
	if len(a) != len(b) {
		return false
	}
	for s, ka := range a {
		kb, ok := b[s]
		if !ok || len(ka) != len(kb) {
			return false
		}
		for k := range ka {
			if !kb[k] {
				return false
			}
		}
	}
	return true
}

func refObjID(kind, name string) string { return strings.ToLower(kind) + "/" + name }

// small helpers over decoded JSON

func slice(m map[string]any, fields ...string) []map[string]any {
	s, _, _ := unstructured.NestedSlice(m, fields...)
	out := make([]map[string]any, 0, len(s))
	for _, v := range s {
		if mm, ok := v.(map[string]any); ok {
			out = append(out, mm)
		}
	}
	return out
}

func nested(m map[string]any, fields ...string) (map[string]any, bool) {
	v, ok, _ := unstructured.NestedMap(m, fields...)
	return v, ok
}

func str(m map[string]any, field string) string {
	s, _ := m[field].(string)
	return s
}
