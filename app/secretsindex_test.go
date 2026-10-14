package main

import (
	"strings"
	"testing"
)

func TestSecretRefsAndIndex(t *testing.T) {
	d := obj(t, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: api, namespace: p}
spec:
  template:
    spec:
      imagePullSecrets: [{name: regcred}]
      containers:
      - name: c
        env:
        - name: DB_PASSWORD
          valueFrom: {secretKeyRef: {name: db, key: PASSWORD}}
        envFrom: [{secretRef: {name: app-env}}]
      volumes:
      - name: tls
        secret: {secretName: tls, items: [{key: tls.crt, path: c}]}
`)
	refs := secretRefs("apps", "Deployment", d.Object)
	for _, s := range []string{"db", "app-env", "regcred", "tls"} {
		if _, ok := refs[s]; !ok {
			t.Errorf("missing ref %s", s)
		}
	}
	if !refs["db"]["PASSWORD"] || !refs["tls"]["tls.crt"] {
		t.Errorf("missing keys: %v", refs)
	}
	if secretRefs("example.com", "Deployment", d.Object) != nil {
		t.Error("CRD named Deployment must not be treated as a workload")
	}

	x := newSecretIndex()
	if !x.set("p", "deployment/api", refs) {
		t.Fatal("expected change")
	}
	if x.set("p", "deployment/api", refs) {
		t.Fatal("same refs should not be a change")
	}
	x.set("p", "cronjob/job", refSet{"db": {"USER": true}})
	out := string(x.render("p"))
	for _, want := range []string{"db:", "- PASSWORD", "- USER", "- cronjob/job", "- deployment/api", "regcred:"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	x.set("p", "deployment/api", nil)
	x.set("p", "cronjob/job", nil)
	if x.render("p") != nil {
		t.Error("empty namespace should render nil")
	}
}
