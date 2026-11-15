package main

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	base := map[string]string{"PICKLE_CLUSTER": "stg", "PICKLE_GIT_URL": "git@example.com:a/b.git"}
	for k, v := range kv {
		base[k] = v
	}
	for k, v := range base {
		t.Setenv(k, v)
	}
}

func TestConfigDefaults(t *testing.T) {
	setEnv(t, nil)
	c, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.CommitDelay != 10*time.Second || c.CommitMaxDelay != time.Minute || c.RediscoveryInterval != 0 {
		t.Fatalf("defaults: delay=%s max=%s rediscovery=%s", c.CommitDelay, c.CommitMaxDelay, c.RediscoveryInterval)
	}
	if len(c.Deprecated) != 0 {
		t.Fatalf("unexpected deprecations: %v", c.Deprecated)
	}
}

func TestConfigDeprecatedCommitInterval(t *testing.T) {
	setEnv(t, map[string]string{"PICKLE_COMMIT_INTERVAL": "13s", "PICKLE_REDISCOVERY_INTERVAL": "10m"})
	c, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.CommitDelay != 13*time.Second || len(c.Deprecated) != 1 {
		t.Fatalf("PICKLE_COMMIT_INTERVAL must map to the delay with a warning: %s %v", c.CommitDelay, c.Deprecated)
	}
	if c.RediscoveryInterval != 10*time.Minute {
		t.Fatalf("an explicit rediscovery interval must still work: %s", c.RediscoveryInterval)
	}
}

func TestConfigRejects(t *testing.T) {
	for name, kv := range map[string]map[string]string{
		"max below delay":     {"PICKLE_COMMIT_DELAY": "30s", "PICKLE_COMMIT_MAX_DELAY": "10s"},
		"token in https url":  {"PICKLE_GIT_URL": "https://pickle:glpat-xyz@gitlab.example.com/a/b.git"},
		"user in https url":   {"PICKLE_GIT_URL": "https://pickle@gitlab.example.com/a/b.git"},
		"password in ssh url": {"PICKLE_GIT_URL": "ssh://git:pw@gitlab.example.com/a/b.git"},
		"tiny rediscovery":    {"PICKLE_REDISCOVERY_INTERVAL": "5s"},
		"unknown color":       {"PICKLE_COLOR": "#00ff00"},
	} {
		t.Run(name, func(t *testing.T) {
			setEnv(t, kv)
			if _, err := loadConfig(); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
	setEnv(t, map[string]string{"PICKLE_GIT_URL": "ssh://git@gitlab.example.com/a/b.git", "PICKLE_REDISCOVERY_INTERVAL": "off"})
	if _, err := loadConfig(); err != nil {
		t.Fatalf("ssh user without password is fine: %v", err)
	}
}

func TestGitAuthRefusesTokenOverHTTP(t *testing.T) {
	dir := t.TempDir()
	tok := dir + "/token"
	if err := writeFile(tok, "secret"); err != nil {
		t.Fatal(err)
	}
	_, err := gitAuth(&config{GitURL: "http://git.example.com/a.git", GitTokenFile: tok})
	if err == nil || !strings.Contains(err.Error(), "plain http") {
		t.Fatalf("expected refusal, got %v", err)
	}
	if _, err := gitAuth(&config{GitURL: "https://git.example.com/a.git", GitTokenFile: tok}); err != nil {
		t.Fatal(err)
	}
}

func TestExcludeRules(t *testing.T) {
	setEnv(t, map[string]string{
		"PICKLE_EXCLUDE":            "IPAMBlock, blockaffinities.crd.projectcalico.org,*.metrics.example.io\nsecrets.core",
		"PICKLE_EXCLUDE_NAMESPACES": "scratch, tmp-*",
	})
	c, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		group, resource, kind string
		want                  bool
	}{
		{"crd.projectcalico.org", "ipamblocks", "IPAMBlock", true},
		{"projectcalico.org", "ipamblocks", "IPAMBlock", true},
		{"crd.projectcalico.org", "blockaffinities", "BlockAffinity", true},
		{"other.io", "blockaffinities", "BlockAffinity", false},
		{"metrics.example.io", "anything", "Anything", true},
		{"", "secrets", "Secret", true},
		{"bitnami.com", "secrets", "Secret", false},
		{"", "configmaps", "ConfigMap", false},
	} {
		if got := c.excluded(tc.group, tc.resource, tc.kind); got != tc.want {
			t.Errorf("excluded(%s, %s, %s) = %v", tc.group, tc.resource, tc.kind, got)
		}
	}
	for ns, want := range map[string]bool{"scratch": true, "tmp-42": true, "payments": false, "": false} {
		if got := c.namespaceExcluded(ns); got != want {
			t.Errorf("namespaceExcluded(%q) = %v", ns, got)
		}
	}
	setEnv(t, map[string]string{"PICKLE_EXCLUDE": "*"})
	if _, err := loadConfig(); err == nil {
		t.Error("a bare * must be rejected")
	}
}

func TestConfigColor(t *testing.T) {
	setEnv(t, nil)
	c, err := loadConfig()
	if err != nil || c.Color != "" {
		t.Fatalf("no color by default: %q %v", c.Color, err)
	}
	setEnv(t, map[string]string{"PICKLE_COLOR": " Green "})
	if c, err = loadConfig(); err != nil || c.Color != "🟢" {
		t.Fatalf("green must map to its marker: %q %v", c.Color, err)
	}
}

func TestExcludeSecretTypes(t *testing.T) {
	setEnv(t, nil)
	c, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !c.ExcludeSecretTypes["helm.sh/release.v1"] || len(c.ExcludeSecretTypes) != 1 {
		t.Errorf("default: %v", c.ExcludeSecretTypes)
	}
	setEnv(t, map[string]string{"PICKLE_EXCLUDE_SECRET_TYPES": ""})
	if c, _ = loadConfig(); len(c.ExcludeSecretTypes) != 0 {
		t.Errorf("empty must exclude none: %v", c.ExcludeSecretTypes)
	}
	setEnv(t, map[string]string{"PICKLE_EXCLUDE_SECRET_TYPES": "helm.sh/release.v1, kubernetes.io/tls"})
	if c, _ = loadConfig(); !c.ExcludeSecretTypes["kubernetes.io/tls"] || !c.ExcludeSecretTypes["helm.sh/release.v1"] {
		t.Errorf("list: %v", c.ExcludeSecretTypes)
	}
}

func TestMirrorOwnedBy(t *testing.T) {
	own := func(apiVersion, kind string) []metav1.OwnerReference {
		return []metav1.OwnerReference{{APIVersion: apiVersion, Kind: kind, Name: "x"}}
	}
	sealed := own("bitnami.com/v1alpha1", "SealedSecret")
	other := own("monitoring.coreos.com/v1", "Prometheus")

	setEnv(t, nil)
	c, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !c.mirrorsOwned(nil) {
		t.Error("an object without owners is always mirrored")
	}
	if !c.mirrorsOwned(sealed) {
		t.Error("default: a SealedSecret's Secret must be mirrored")
	}
	if c.mirrorsOwned(other) {
		t.Error("default: other owned objects are skipped")
	}
	if !c.mirrorsOwned(append(other, sealed...)) {
		t.Error("one matching owner is enough")
	}
	if c.mirrorsOwned(own("other.io/v1", "SealedSecret")) {
		t.Error("the default is limited to bitnami.com")
	}

	setEnv(t, map[string]string{"PICKLE_MIRROR_OWNED_BY": ""})
	if c, _ = loadConfig(); c.mirrorsOwned(sealed) {
		t.Error("empty: every owned object is skipped")
	}

	setEnv(t, map[string]string{"PICKLE_MIRROR_OWNED_BY": "Prometheus, *.example.io, Deployment.apps, ConfigMap.core"})
	c, _ = loadConfig()
	for _, tc := range []struct {
		owners []metav1.OwnerReference
		want   bool
	}{
		{other, true},
		{own("v1", "Prometheus"), true},
		{own("example.io/v2", "Widget"), true},
		{own("sub.example.io/v1", "Widget"), false},
		{own("apps/v1", "Deployment"), true},
		{own("extensions/v1beta1", "Deployment"), false},
		{own("v1", "ConfigMap"), true},
		{sealed, false},
	} {
		if got := c.mirrorsOwned(tc.owners); got != tc.want {
			t.Errorf("mirrorsOwned(%s %s) = %v", tc.owners[0].APIVersion, tc.owners[0].Kind, got)
		}
	}

	setEnv(t, map[string]string{"PICKLE_MIRROR_OWNED_BY": "*"})
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "PICKLE_MIRROR_OWNED_BY") {
		t.Errorf("a bare * must be rejected by name: %v", err)
	}
}
