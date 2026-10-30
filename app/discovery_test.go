package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
)

func flatLists(t *testing.T, d *apiDiscovery) ([]string, error) {
	t.Helper()
	lists, err := d.ServerPreferredResources()
	var out []string
	for _, l := range lists {
		for _, r := range l.APIResources {
			out = append(out, fmt.Sprintf("%s %s %s %v", l.GroupVersion, r.Name, r.Kind, r.Namespaced))
		}
	}
	sort.Strings(out)
	return out, err
}

func TestDiscoveryLegacy(t *testing.T) {
	docs := map[string]string{
		"/api":            `{"versions":["v1"]}`,
		"/apis":           `{"groups":[{"name":"apps","versions":[{"groupVersion":"apps/v1","version":"v1"}],"preferredVersion":{"groupVersion":"apps/v1","version":"v1"}},{"name":"ex.com","versions":[{"groupVersion":"ex.com/v2","version":"v2"},{"groupVersion":"ex.com/v1","version":"v1"}],"preferredVersion":{"groupVersion":"ex.com/v2","version":"v2"}},{"name":"down.com","versions":[{"groupVersion":"down.com/v1","version":"v1"}],"preferredVersion":{"groupVersion":"down.com/v1","version":"v1"}}]}`,
		"/api/v1":         `{"groupVersion":"v1","resources":[{"name":"pods","kind":"Pod","namespaced":true,"verbs":["list","watch"]},{"name":"pods/log","kind":"Pod","namespaced":true,"verbs":["get"]},{"name":"nodes","kind":"Node","verbs":["list","watch"]}]}`,
		"/apis/apps/v1":   `{"groupVersion":"apps/v1","resources":[{"name":"deployments","kind":"Deployment","namespaced":true,"verbs":["list","watch"]}]}`,
		"/apis/ex.com/v2": `{"groupVersion":"ex.com/v2","resources":[{"name":"widgets","kind":"Widget","namespaced":true,"verbs":["list"]}]}`,
		"/apis/ex.com/v1": `{"groupVersion":"ex.com/v1","resources":[{"name":"widgets","kind":"Widget","namespaced":true,"verbs":["list"]},{"name":"gadgets","kind":"Gadget","verbs":["list"]}]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := docs[r.URL.Path]
		if !ok {
			http.Error(w, "no", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(b))
	}))
	defer srv.Close()
	d, err := newAPIDiscovery(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	got, err := flatLists(t, d)
	var pe *errPartialDiscovery
	if !errors.As(err, &pe) || len(pe.Groups) != 1 {
		t.Fatalf("want one failed group (down.com), got %v", err)
	}
	want := []string{"apps/v1 deployments Deployment true", "ex.com/v1 gadgets Gadget false", "ex.com/v2 widgets Widget true", "v1 nodes Node false", "v1 pods Pod true"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestDiscoveryAggregated(t *testing.T) {
	core := `{"kind":"APIGroupDiscoveryList","items":[{"metadata":{"name":""},"versions":[{"version":"v1","resources":[{"resource":"pods","responseKind":{"group":"","version":"v1","kind":"Pod"},"scope":"Namespaced","verbs":["list","watch"],"subresources":[{"subresource":"log"}]}]}]}]}`
	groups := `{"kind":"APIGroupDiscoveryList","items":[
	  {"metadata":{"name":"ex.com"},"versions":[
	    {"version":"v2","resources":[{"resource":"widgets","responseKind":{"kind":"Widget"},"scope":"Namespaced","verbs":["list"]}]},
	    {"version":"v1","resources":[{"resource":"widgets","responseKind":{"kind":"Widget"},"scope":"Namespaced","verbs":["list"]},{"resource":"gadgets","responseKind":{"kind":"Gadget"},"scope":"Cluster","verbs":["list"]}]}]},
	  {"metadata":{"name":"metrics.k8s.io"},"versions":[{"version":"v1beta1","freshness":"Stale"}]}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept"), "as=APIGroupDiscoveryList") {
			http.Error(w, "want aggregated", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json;g=apidiscovery.k8s.io;v=v2;as=APIGroupDiscoveryList")
		switch r.URL.Path {
		case "/api":
			w.Write([]byte(core))
		case "/apis":
			w.Write([]byte(groups))
		default:
			http.Error(w, "one request per root", http.StatusTeapot)
		}
	}))
	defer srv.Close()
	d, _ := newAPIDiscovery(&rest.Config{Host: srv.URL})
	got, err := flatLists(t, d)
	var pe *errPartialDiscovery
	if !errors.As(err, &pe) || len(pe.Groups) != 1 {
		t.Fatalf("want the stale group reported, got %v", err)
	}
	want := []string{"ex.com/v1 gadgets Gadget false", "ex.com/v2 widgets Widget true", "v1 pods Pod true"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
