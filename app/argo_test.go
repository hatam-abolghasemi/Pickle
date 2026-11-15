package main

import (
	"os"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	argoApps = &resource{gvr: schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}, kind: "Application", namespaced: true, token: "application"}
	argoDeps = &resource{gvr: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, kind: "Deployment", namespaced: true, token: "deployment"}
)

// argoApp is an Application named payments in namespace ns whose last sync
// is op (a YAML flow mapping), or none when op is "".
func argoApp(t *testing.T, ns, op string) string {
	t.Helper()
	s := `{apiVersion: argoproj.io/v1alpha1, kind: Application, metadata: {name: payments, namespace: ` + ns + `}, spec: {project: default}`
	if op != "" {
		s += `, status: {operationState: ` + op + `}`
	}
	return s + `}`
}

const (
	manualSync = `{phase: Succeeded, startedAt: "2026-11-15T09:00:00Z", finishedAt: "2026-11-15T09:00:20Z",
	  operation: {initiatedBy: {username: hatam}, sync: {revision: main}},
	  syncResult: {revision: 3f2a9c1e5b7d}}`
	autoSync = `{phase: Running, startedAt: "2026-11-15T10:00:00Z",
	  operation: {initiatedBy: {automated: true}, sync: {revision: 9b8c7d6e5f4a}}}`
)

// argoDeployment is a Deployment of Application app, last changed by
// manager at ts.
func argoDeployment(manager, ts, app string) string {
	return `{apiVersion: apps/v1, kind: Deployment, metadata: {name: api, namespace: payments,
	  annotations: {argocd.argoproj.io/tracking-id: "` + app + `:apps/Deployment:payments/api"},
	  managedFields: [{manager: ` + manager + `, operation: Apply, time: "` + ts + `"}]},
	  spec: {replicas: 2}}`
}

func TestArgoSyncInHeader(t *testing.T) {
	for _, c := range []struct {
		name, app, dep string
		want           []string // header lines; nil: no argocd-initiated-by line
	}{
		{"manual sync", argoApp(t, "argocd", manualSync), argoDeployment("argocd-controller", "2026-11-15T09:00:10Z", "payments"),
			[]string{"# argocd-app: payments\n# argocd-initiated-by: hatam\napiVersion:"}},
		{"auto sync still running", argoApp(t, "argocd", autoSync), argoDeployment("argocd-application-controller", "2026-11-15T10:00:03Z", "payments"),
			[]string{"# argocd-initiated-by: automated\n"}},
		{"clock skew within slack", argoApp(t, "argocd", manualSync), argoDeployment("argocd-controller", "2026-11-15T08:59:57Z", "payments"),
			[]string{"# argocd-initiated-by: hatam\n"}},
		{"Application outside the Argo CD namespace", argoApp(t, "team-a", manualSync), argoDeployment("argocd-controller", "2026-11-15T09:00:10Z", "team-a_payments"),
			[]string{"# argocd-app: team-a_payments\n# argocd-initiated-by: hatam\n"}},
		{"synced through tiphys", argoApp(t, "argocd", `{phase: Succeeded, startedAt: "2026-11-15T09:00:00Z", finishedAt: "2026-11-15T09:00:20Z",
		  operation: {initiatedBy: {username: tiphys}, info: [{name: Triggered by, value: "sara (tiphys)"}], sync: {revision: main}}}`),
			argoDeployment("argocd-controller", "2026-11-15T09:00:10Z", "payments"),
			[]string{"# argocd-app: payments\n# argocd-initiated-by: sara (tiphys)\n"}},
		{"changed by kubectl, not Argo CD", argoApp(t, "argocd", manualSync), argoDeployment("kubectl-edit", "2026-11-15T09:00:10Z", "payments"), nil},
		{"changed before the sync", argoApp(t, "argocd", manualSync), argoDeployment("argocd-controller", "2026-11-14T12:00:00Z", "payments"), nil},
		{"changed after the sync finished", argoApp(t, "argocd", manualSync), argoDeployment("argocd-controller", "2026-11-15T09:05:00Z", "payments"), nil},
		{"Application never synced", argoApp(t, "argocd", ""), argoDeployment("argocd-controller", "2026-11-15T09:00:10Z", "payments"), nil},
		{"another Application", argoApp(t, "argocd", manualSync), argoDeployment("argocd-controller", "2026-11-15T09:00:10Z", "orders"), nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, err := newEngine(&config{Cluster: "stg", RepoDir: t.TempDir()}, nil, testDisco, nil, newMetrics("stg"))
			if err != nil {
				t.Fatal(err)
			}
			e.handle(argoApps, obj(t, c.app), false, true)
			p := e.handle(argoDeps, obj(t, c.dep), false, true)
			b, err := os.ReadFile(e.abs(p))
			if err != nil {
				t.Fatal(err)
			}
			got := string(b)
			if c.want == nil {
				if strings.Contains(got, "argocd-initiated-by") {
					t.Errorf("no sync lines expected:\n%s", got)
				}
				return
			}
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("want %q in:\n%s", w, got)
				}
			}
		})
	}
}

// TestArgoSyncForgotten: a deleted Application, or one whose operation
// state is gone, no longer attributes anything.
func TestArgoSyncForgotten(t *testing.T) {
	a := newArgoSyncs()
	h := func() header {
		h := header{manager: "argocd-controller", changed: "2026-11-15T09:00:10Z", argoApp: "payments"}
		a.attribute(&h)
		return h
	}
	a.note(obj(t, argoApp(t, "argocd", manualSync)), false)
	if h().argoInitiatedBy != "hatam" {
		t.Fatal("sync not recorded")
	}
	// A metadata-only event carries no status: the sync stays known.
	a.note(obj(t, `{apiVersion: argoproj.io/v1alpha1, kind: Application, metadata: {name: payments, namespace: argocd}}`), false)
	if h().argoInitiatedBy != "hatam" {
		t.Error("a metadata-only event must not forget the sync")
	}
	a.note(obj(t, `{apiVersion: argoproj.io/v1alpha1, kind: Application, metadata: {name: payments, namespace: argocd}, status: {sync: {status: Synced}}}`), false)
	if h().argoInitiatedBy != "" {
		t.Error("an Application without operationState must forget the sync")
	}
	a.note(obj(t, argoApp(t, "argocd", manualSync)), false)
	a.note(obj(t, argoApp(t, "argocd", manualSync)), true)
	if h().argoInitiatedBy != "" {
		t.Error("a deleted Application must forget the sync")
	}
}

// TestArgoSyncAmbiguousName: two Applications named payments in different
// namespaces, and a bare "payments" tracking id can't say which one.
func TestArgoSyncAmbiguousName(t *testing.T) {
	a := newArgoSyncs()
	a.note(obj(t, argoApp(t, "argocd", manualSync)), false)
	a.note(obj(t, argoApp(t, "team-a", autoSync)), false)
	bare := header{manager: "argocd-controller", changed: "2026-11-15T09:00:10Z", argoApp: "payments"}
	if a.attribute(&bare); bare.argoInitiatedBy != "" {
		t.Errorf("ambiguous name attributed to %q", bare.argoInitiatedBy)
	}
	full := header{manager: "argocd-controller", changed: "2026-11-15T10:00:10Z", argoApp: "team-a_payments"}
	if a.attribute(&full); full.argoInitiatedBy != "automated" {
		t.Errorf("namespaced id: got %q", full.argoInitiatedBy)
	}
	a.note(obj(t, argoApp(t, "team-a", autoSync)), true)
	if a.attribute(&bare); bare.argoInitiatedBy != "hatam" {
		t.Errorf("after the other one is gone: got %q", bare.argoInitiatedBy)
	}
}

// TestReadArgoSyncTiphys: tiphys syncs through Argo CD's API as its own
// account and names its user in the operation's info.
func TestReadArgoSyncTiphys(t *testing.T) {
	for _, c := range []struct{ op, want string }{
		{`{startedAt: "2026-11-15T09:00:00Z", operation: {initiatedBy: {username: tiphys}, info: [{name: Triggered by, value: "sara (tiphys)"}]}}`, "sara (tiphys)"},
		{`{startedAt: "2026-11-15T09:00:00Z", operation: {initiatedBy: {username: tiphys}, info: [{name: other, value: x}, {name: " triggered BY ", value: " sara\n(tiphys) "}]}}`, "sara (tiphys)"},
		// A rollback through tiphys carries no info: the Argo CD account.
		{`{startedAt: "2026-11-15T09:00:00Z", operation: {initiatedBy: {username: tiphys}}}`, "tiphys"},
		{`{startedAt: "2026-11-15T09:00:00Z", operation: {initiatedBy: {username: tiphys}, info: [{name: Triggered by, value: ""}]}}`, "tiphys"},
		{`{startedAt: "2026-11-15T09:00:00Z", operation: {initiatedBy: {automated: true}, info: [{name: Triggered by, value: sara}]}}`, "sara"},
		// Malformed or missing parts are skipped, never fatal.
		{`{startedAt: "2026-11-15T09:00:00Z", operation: {initiatedBy: {username: tiphys}, info: "Triggered by sara"}}`, "tiphys"},
		{`{startedAt: "2026-11-15T09:00:00Z", operation: {initiatedBy: {username: tiphys}, info: [x, 3, null, {name: 5, value: [a]}]}}`, "tiphys"},
		{`{startedAt: "2026-11-15T09:00:00Z", operation: [1, 2]}`, ""},
		{`{startedAt: "2026-11-15T09:00:00Z", operation: {initiatedBy: "sara"}}`, ""},
		{`{startedAt: "2026-11-15T09:00:00Z"}`, ""},
	} {
		s, ok := readArgoSync(obj(t, argoApp(t, "argocd", c.op)).Object)
		if !ok || s.initiatedBy != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.op, s.initiatedBy, ok, c.want)
		}
	}
	for _, bad := range []string{`"x"`, `[1]`, `{startedAt: 5}`, `{startedAt: yesterday}`} {
		if _, ok := readArgoSync(obj(t, argoApp(t, "argocd", bad)).Object); ok {
			t.Errorf("%s: an operationState without a startedAt time can't place a change", bad)
		}
	}
	if _, ok := readArgoSync(obj(t, `{apiVersion: argoproj.io/v1alpha1, kind: Application, metadata: {name: a}, status: [1]}`).Object); ok {
		t.Error("a status that isn't a map must be skipped")
	}
}

// TestArgoSyncWithoutData: an object pointing at an Application pickle has
// never seen, or a header with nothing to go on, is left alone.
func TestArgoSyncWithoutData(t *testing.T) {
	a := newArgoSyncs()
	for _, h := range []header{
		{manager: "argocd-controller", changed: "2026-11-15T09:00:10Z", argoApp: "payments"},
		{manager: "argocd-controller", changed: "", argoApp: "payments"},
		{manager: "argocd-controller", changed: "not a time", argoApp: "payments"},
		{manager: "", changed: "", argoApp: ""},
	} {
		want := h
		if a.attribute(&h); h != want {
			t.Errorf("got %+v", h)
		}
	}
	// Applications without a namespace or name, as from a broken watch.
	a.note(obj(t, `{apiVersion: argoproj.io/v1alpha1, kind: Application}`), false)
	a.note(obj(t, `{apiVersion: argoproj.io/v1alpha1, kind: Application}`), true)
	a.note(obj(t, `{apiVersion: argoproj.io/v1alpha1, kind: Application, metadata: {name: never-seen, namespace: argocd}}`), true)
}

func TestReadArgoSync(t *testing.T) {
	s, ok := readArgoSync(obj(t, argoApp(t, "argocd", `{startedAt: "2026-11-15T09:00:00Z",
	  operation: {initiatedBy: {username: admin}, sync: {revision: main}}}`)).Object)
	if !ok || s.initiatedBy != "admin" || !s.finished.IsZero() {
		t.Errorf("got %+v, %v", s, ok)
	}
	if _, ok := readArgoSync(obj(t, argoApp(t, "argocd", `{phase: Running}`)).Object); ok {
		t.Error("an operationState without startedAt can't place a change in time")
	}
}

// TestArgoSyncLeftOutWhereHeaderCounts: Secrets masked without a fingerprint
// key rewrite on header-only changes, so they get no sync lines; otherwise
// every sync would rewrite them.
func TestArgoSyncLeftOutWhereHeaderCounts(t *testing.T) {
	e, err := newEngine(&config{Cluster: "stg", RepoDir: t.TempDir()}, nil, testDisco, nil, newMetrics("stg"))
	if err != nil {
		t.Fatal(err)
	}
	secrets := &resource{gvr: schema.GroupVersionResource{Version: "v1", Resource: "secrets"}, kind: "Secret", namespaced: true, token: "secret"}
	e.handle(argoApps, obj(t, argoApp(t, "argocd", manualSync)), false, true)
	p := e.handle(secrets, obj(t, `{apiVersion: v1, kind: Secret, metadata: {name: db, namespace: payments,
	  annotations: {argocd.argoproj.io/tracking-id: "payments:/Secret:payments/db"},
	  managedFields: [{manager: argocd-controller, operation: Apply, time: "2026-11-15T09:00:10Z"}]},
	  data: {A: YQ==}}`), false, true)
	if b, _ := os.ReadFile(e.abs(p)); !strings.Contains(string(b), "# argocd-app: payments") || strings.Contains(string(b), "argocd-initiated-by") {
		t.Errorf("got:\n%s", b)
	}
}
