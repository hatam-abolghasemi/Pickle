package main

import (
	"path"
	"regexp"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

const (
	clusterScopedDir = "_cluster"
	kindsFile        = "_kinds.txt"
	secretsIndexFile = "_secrets-referenced.txt"
	unreadableFile   = "_unreadable.txt"
)

// kindRegistry decides each kind's file prefix. The first API group to claim a
// kind keeps the short prefix ("deployment-"); any later group with the same
// kind gets "kind.group-". Claims are persisted in <cluster>/_kinds.txt, so an
// existing file is never renamed when a colliding CRD shows up later.
type kindRegistry struct {
	claims map[string]string // lowercase kind -> owning group ("core" for "")
	dirty  bool
}

func newKindRegistry() *kindRegistry { return &kindRegistry{claims: map[string]string{}} }

func groupName(g string) string {
	if g == "" {
		return "core"
	}
	return g
}

// isBuiltinGroup is used only to let built-in kinds claim names before CRDs.
func isBuiltinGroup(g string) bool {
	return g == "" || !strings.Contains(g, ".") || strings.HasSuffix(g, ".k8s.io")
}

func (k *kindRegistry) token(kind, group string) string {
	kl, g := strings.ToLower(kind), groupName(group)
	owner, ok := k.claims[kl]
	if !ok {
		k.claims[kl] = g
		k.dirty = true
		return kl
	}
	if owner == g {
		return kl
	}
	return kl + "." + g
}

func (k *kindRegistry) load(data []byte) error {
	return yaml.Unmarshal(data, &k.claims)
}

func (k *kindRegistry) render() []byte {
	keys := make([]string, 0, len(k.claims))
	for kk := range k.claims {
		keys = append(keys, kk)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# kind -> API group owning the short file prefix. Other groups with the same kind use \"kind.group-name.yaml\".\n")
	for _, kk := range keys {
		b.WriteString(kk + ": " + k.claims[kk] + "\n")
	}
	return []byte(b.String())
}

var unsafeChars = regexp.MustCompile(`[^a-z0-9.-]`)

func fileName(token, name string) string {
	return token + "-" + unsafeChars.ReplaceAllString(strings.ToLower(name), "_") + ".yaml"
}

// objPath is the repo-relative path of an object's file.
func objPath(cluster string, namespaced bool, ns, token, name string) string {
	dir := clusterScopedDir
	if namespaced {
		dir = ns
	}
	return path.Join(cluster, dir, fileName(token, name))
}

// tokenOf finds which known token a file name was written with (longest match wins,
// since a group like cert-manager.io can itself contain dashes).
func tokenOf(base string, tokens map[string]string) string {
	best := ""
	for t := range tokens {
		if strings.HasPrefix(base, t+"-") && len(t) > len(best) {
			best = t
		}
	}
	return best
}
