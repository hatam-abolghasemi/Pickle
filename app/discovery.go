package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// apiDiscovery lists the served resource types, the way client-go's
// discovery client does for ServerPreferredResources, in the few requests
// the API server's aggregated discovery needs (one for the core group, one
// for every other group), with the legacy per-version requests as a
// fallback.
//
// client-go's discovery package isn't used: it imports the typed scheme of
// every built-in API, half of pickle's binary and a few MiB of heap
// registered at startup, none of which pickle uses.
type apiDiscovery struct {
	base   *url.URL
	client *http.Client
}

func newAPIDiscovery(rc *rest.Config) (*apiDiscovery, error) {
	base, _, err := rest.DefaultServerUrlFor(rc)
	if err != nil {
		return nil, err
	}
	hc, err := rest.HTTPClientFor(rc)
	if err != nil {
		return nil, err
	}
	return &apiDiscovery{base: base, client: hc}, nil
}

// errPartialDiscovery: some groups couldn't be listed (an aggregated API
// down, say); the lists returned hold every other group.
type errPartialDiscovery struct {
	Groups map[schema.GroupVersion]error
}

func (e *errPartialDiscovery) Error() string {
	var parts []string
	for gv, err := range e.Groups {
		parts = append(parts, fmt.Sprintf("%s: %v", gv, err))
	}
	sort.Strings(parts)
	return "unable to retrieve the complete list of server APIs: " + strings.Join(parts, ", ")
}

const (
	aggregatedAccept = "application/json;g=apidiscovery.k8s.io;v=v2;as=APIGroupDiscoveryList,application/json;q=0.9"
	discoveryTimeout = 30 * time.Second
)

// Aggregated discovery (apidiscovery.k8s.io/v2), as much of it as is read.
type groupDiscoveryList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Versions []struct {
			Version   string `json:"version"`
			Freshness string `json:"freshness"`
			Resources []struct {
				Resource     string `json:"resource"`
				ResponseKind *struct {
					Kind string `json:"kind"`
				} `json:"responseKind"`
				Scope string   `json:"scope"`
				Verbs []string `json:"verbs"`
			} `json:"resources"`
		} `json:"versions"`
	} `json:"items"`
}

// ServerPreferredResources returns, per group, every resource type at the
// group's preferred version, plus those served only at other versions, at
// the most preferred version serving them.
func (d *apiDiscovery) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	ctx, cancel := context.WithTimeout(context.Background(), discoveryTimeout)
	defer cancel()
	var out []*metav1.APIResourceList
	failed := map[schema.GroupVersion]error{}
	for _, root := range []string{"/api", "/apis"} {
		body, aggregated, err := d.get(ctx, root, aggregatedAccept)
		if err != nil {
			return nil, err
		}
		var lists []*metav1.APIResourceList
		if aggregated {
			lists, err = fromAggregated(body, failed)
		} else {
			lists, err = d.legacy(ctx, root, body, failed)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, lists...)
	}
	if len(failed) > 0 {
		return out, &errPartialDiscovery{Groups: failed}
	}
	return out, nil
}

func fromAggregated(body []byte, failed map[schema.GroupVersion]error) ([]*metav1.APIResourceList, error) {
	var l groupDiscoveryList
	if err := json.Unmarshal(body, &l); err != nil {
		return nil, fmt.Errorf("decoding aggregated discovery: %w", err)
	}
	var out []*metav1.APIResourceList
	for _, g := range l.Items {
		seen := map[string]bool{}
		for _, v := range g.Versions { // most preferred first
			gv := schema.GroupVersion{Group: g.Metadata.Name, Version: v.Version}
			if v.Freshness == "Stale" {
				failed[gv] = fmt.Errorf("stale aggregated discovery")
				continue
			}
			list := &metav1.APIResourceList{GroupVersion: gv.String()}
			for _, r := range v.Resources {
				if seen[r.Resource] {
					continue
				}
				seen[r.Resource] = true
				kind := ""
				if r.ResponseKind != nil {
					kind = r.ResponseKind.Kind
				}
				list.APIResources = append(list.APIResources, metav1.APIResource{
					Name: r.Resource, Kind: kind, Namespaced: r.Scope == "Namespaced", Verbs: r.Verbs,
				})
			}
			if len(list.APIResources) > 0 {
				out = append(out, list)
			}
		}
	}
	return out, nil
}

// legacy discovers one root (/api or /apis) a version at a time, for API
// servers without aggregated discovery.
func (d *apiDiscovery) legacy(ctx context.Context, root string, body []byte, failed map[schema.GroupVersion]error) ([]*metav1.APIResourceList, error) {
	type group struct {
		name     string
		versions []string // most preferred first
	}
	var groups []group
	if root == "/api" {
		var v metav1.APIVersions
		if err := json.Unmarshal(body, &v); err != nil {
			return nil, fmt.Errorf("decoding /api: %w", err)
		}
		groups = []group{{versions: v.Versions}}
	} else {
		var gl metav1.APIGroupList
		if err := json.Unmarshal(body, &gl); err != nil {
			return nil, fmt.Errorf("decoding /apis: %w", err)
		}
		for _, g := range gl.Groups {
			vs := []string{g.PreferredVersion.Version}
			for _, v := range g.Versions {
				if v.Version != g.PreferredVersion.Version {
					vs = append(vs, v.Version)
				}
			}
			groups = append(groups, group{name: g.Name, versions: vs})
		}
	}
	var out []*metav1.APIResourceList
	for _, g := range groups {
		seen := map[string]bool{}
		for _, v := range g.versions {
			gv := schema.GroupVersion{Group: g.name, Version: v}
			b, _, err := d.get(ctx, root+"/"+gv.String(), "application/json")
			if err != nil {
				failed[gv] = err
				continue
			}
			var rl metav1.APIResourceList
			if err := json.Unmarshal(b, &rl); err != nil {
				failed[gv] = err
				continue
			}
			list := &metav1.APIResourceList{GroupVersion: gv.String()}
			for _, r := range rl.APIResources {
				if seen[r.Name] || strings.Contains(r.Name, "/") {
					continue // subresources aren't types, as in client-go's preferred list
				}
				seen[r.Name] = true
				list.APIResources = append(list.APIResources, r)
			}
			if len(list.APIResources) > 0 {
				out = append(out, list)
			}
		}
	}
	return out, nil
}

// get fetches a discovery document; aggregated reports whether the server
// answered with aggregated discovery.
func (d *apiDiscovery) get(ctx context.Context, path, accept string) (body []byte, aggregated bool, err error) {
	u := *d.base
	u.Path = strings.TrimRight(u.Path, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Accept", accept)
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, false, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	_, params, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	aggregated = params["g"] == "apidiscovery.k8s.io" && params["as"] == "APIGroupDiscoveryList"
	return body, aggregated, nil
}
