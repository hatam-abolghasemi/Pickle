package main

import "testing"

func TestKindRegistryCollision(t *testing.T) {
	r := newKindRegistry()
	if got := r.token("Certificate", "cert-manager.io"); got != "certificate" {
		t.Fatalf("first claim: %q", got)
	}
	if got := r.token("Certificate", "acme.example.com"); got != "certificate.acme.example.com" {
		t.Fatalf("collision: %q", got)
	}

	// persisted claims keep names stable even if discovery order changes later
	r2 := newKindRegistry()
	if err := r2.load(r.render()); err != nil {
		t.Fatal(err)
	}
	if got := r2.token("Certificate", "acme.example.com"); got != "certificate.acme.example.com" {
		t.Fatalf("after reload: %q", got)
	}
	if got := r2.token("Certificate", "cert-manager.io"); got != "certificate" {
		t.Fatalf("after reload owner: %q", got)
	}
}

func TestPaths(t *testing.T) {
	if p := objPath("stg", true, "payments", "deployment", "Payments_API"); p != "stg/payments/deployment-payments_api.yaml" {
		t.Fatal(p)
	}
	if p := objPath("stg", false, "", "clusterrole", "system:aggregate-to-admin"); p != "stg/_cluster/clusterrole-system_aggregate-to-admin.yaml" {
		t.Fatal(p)
	}
}

func TestTokenOf(t *testing.T) {
	tokens := map[string]string{"certificate": "x", "certificate.cert-manager.io": "y", "role": "z"}
	if got := tokenOf("certificate.cert-manager.io-web.yaml", tokens); got != "certificate.cert-manager.io" {
		t.Fatal(got)
	}
	if got := tokenOf("certificate-web.yaml", tokens); got != "certificate" {
		t.Fatal(got)
	}
	if got := tokenOf("unknown-x.yaml", tokens); got != "" {
		t.Fatal(got)
	}
}
