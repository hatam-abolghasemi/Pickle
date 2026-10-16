package main

import (
	"strings"
	"testing"
	"time"
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
