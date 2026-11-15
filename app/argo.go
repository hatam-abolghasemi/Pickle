package main

import (
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// argoSync is the last sync operation of an Argo CD Application, as its
// status.operationState records it: who started it, and when.
type argoSync struct {
	initiatedBy string // see readArgoSync
	started     time.Time
	finished    time.Time // zero while the sync runs
}

// argoSyncSlack covers the clock difference between the API server, which
// stamps managedFields, and the Argo CD controller, which stamps startedAt
// and finishedAt.
const argoSyncSlack = 5 * time.Second

// argoSyncs holds the last sync of every Application pickle has seen, by
// <namespace>_<name>. Argo CD's tracking ids use that form for Applications
// outside its own namespace and a bare <name> for those inside it.
type argoSyncs struct {
	mu sync.RWMutex
	m  map[string]argoSync
	// namespaces of the Applications behind each bare <name>: with more
	// than one, a bare tracking id can't tell which Application it means.
	namespaces map[string]map[string]bool
}

func newArgoSyncs() *argoSyncs {
	return &argoSyncs{m: map[string]argoSync{}, namespaces: map[string]map[string]bool{}}
}

func isArgoApplication(r *resource) bool {
	return r.gvr.Group == "argoproj.io" && r.gvr.Resource == "applications"
}

// note records the sync operation of Application u. An Application seen
// through a metadata-only watch has no status and changes nothing.
func (a *argoSyncs) note(u *unstructured.Unstructured, deleted bool) {
	name, ns := u.GetName(), u.GetNamespace()
	key := ns + "_" + name
	s, ok := readArgoSync(u.Object)
	_, hasStatus := u.Object["status"]
	a.mu.Lock()
	defer a.mu.Unlock()
	if deleted {
		delete(a.m, key)
		delete(a.namespaces[name], ns)
		if len(a.namespaces[name]) == 0 {
			delete(a.namespaces, name)
		}
		return
	}
	if a.namespaces[name] == nil {
		a.namespaces[name] = map[string]bool{}
	}
	a.namespaces[name][ns] = true
	switch {
	case ok:
		a.m[key] = s
	case hasStatus:
		delete(a.m, key)
	}
}

// lookup finds the sync of the Application a tracking id names.
func (a *argoSyncs) lookup(app string) (argoSync, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !strings.Contains(app, "_") {
		if len(a.namespaces[app]) != 1 {
			return argoSync{}, false
		}
		for ns := range a.namespaces[app] {
			app = ns + "_" + app
		}
	}
	s, ok := a.m[app]
	return s, ok
}

// attribute adds who started the sync to h, when the
// object's last change was Argo CD's, made during the Application's last
// sync. Anything else leaves h as it is: a missing line is better than a
// wrong name.
func (a *argoSyncs) attribute(h *header) {
	if h.argoApp == "" || !isArgoManager(h.manager) {
		return
	}
	changed, err := time.Parse(time.RFC3339, h.changed)
	if err != nil {
		return
	}
	s, ok := a.lookup(h.argoApp)
	if !ok || changed.Before(s.started.Add(-argoSyncSlack)) {
		return
	}
	if !s.finished.IsZero() && changed.After(s.finished.Add(argoSyncSlack)) {
		return
	}
	h.argoInitiatedBy = s.initiatedBy
}

func isArgoManager(m string) bool {
	return m == "argocd-controller" || m == "argocd-application-controller"
}

// triggeredByInfo is the operation info a portal that syncs through Argo CD's
// API with its own account, like tiphys, names its user in: tiphys sends
// {name: "Triggered by", value: "<user> (tiphys)"}.
const triggeredByInfo = "triggered by"

// readArgoSync reads status.operationState of an Application. Who started
// the sync is, in order: the "Triggered by" info, the Argo CD username, or
// "automated" for auto-sync and self-heal. Anything missing or of the wrong
// type is skipped, never an error.
func readArgoSync(app map[string]any) (argoSync, bool) {
	status, _ := app["status"].(map[string]any)
	st, _ := status["operationState"].(map[string]any)
	if st == nil {
		return argoSync{}, false
	}
	var s argoSync
	var err error
	if s.started, err = time.Parse(time.RFC3339, str(st, "startedAt")); err != nil {
		return argoSync{}, false
	}
	if f := str(st, "finishedAt"); f != "" {
		s.finished, _ = time.Parse(time.RFC3339, f)
	}

	op, _ := st["operation"].(map[string]any)
	by, _ := op["initiatedBy"].(map[string]any)
	if s.initiatedBy = triggeredBy(op); s.initiatedBy == "" {
		s.initiatedBy = strings.TrimSpace(str(by, "username"))
	}
	if s.initiatedBy == "" {
		if auto, _ := by["automated"].(bool); auto {
			s.initiatedBy = "automated"
		}
	}
	return s, true
}

// triggeredBy is the value of the operation's "Triggered by" info, on one
// line, or "".
func triggeredBy(op map[string]any) string {
	infos, _ := op["info"].([]any)
	for _, i := range infos {
		info, _ := i.(map[string]any)
		if strings.EqualFold(strings.TrimSpace(str(info, "name")), triggeredByInfo) {
			if v := strings.Join(strings.Fields(str(info, "value")), " "); v != "" {
				return v
			}
		}
	}
	return ""
}
