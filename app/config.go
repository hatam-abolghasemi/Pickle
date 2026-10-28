package main

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type config struct {
	Cluster string
	Color   string
	RepoDir string

	GitURL            string
	GitBranch         string
	GitSSHUser        string
	GitSSHKeyFile     string
	GitKnownHostsFile string
	GitTokenFile      string
	GitUsername       string
	AuthorName        string
	AuthorEmail       string

	FingerprintKey []byte

	CommitDelay         time.Duration
	CommitMaxDelay      time.Duration
	RediscoveryInterval time.Duration
	PageSize            int64
	PagePause           time.Duration
	QPS                 float32
	Burst               int

	MetricsAddr string
	LogLevel    slog.Level

	Exclude           []excludeRule
	ExcludeNamespaces []string
	CompactEvery      int

	Deprecated []string
}

// excludeRule matches resource types by kind or plural name, optionally
// limited to one API group ("core" for the core group); name "*" matches the
// whole group.
type excludeRule struct {
	name  string
	group string
	raw   string
}

// parseExclude reads PICKLE_EXCLUDE entries:
//
//	IPAMBlock                          a kind (or plural resource) in any group
//	IPAMBlock.crd.projectcalico.org    a kind (or plural resource) in one group
//	*.crd.projectcalico.org            every type of one group
func parseExclude(v string) ([]excludeRule, error) {
	var out []excludeRule
	for _, f := range splitList(v) {
		r := excludeRule{raw: f}
		name, group, hasGroup := strings.Cut(f, ".")
		r.name = strings.ToLower(name)
		if hasGroup {
			r.group = strings.ToLower(group)
		}
		if r.name == "" || (r.name == "*" && !hasGroup) {
			return nil, fmt.Errorf("PICKLE_EXCLUDE: invalid entry %q (use Kind, Kind.group or *.group)", f)
		}
		out = append(out, r)
	}
	return out, nil
}

func (r excludeRule) matches(group, resource, kind string) bool {
	if r.group != "" && r.group != groupName(group) {
		return false
	}
	return r.name == "*" || r.name == strings.ToLower(kind) || r.name == resource
}

func (c *config) excluded(group, resource, kind string) bool {
	for _, r := range c.Exclude {
		if r.matches(group, resource, kind) {
			return true
		}
	}
	return false
}

func (c *config) namespaceExcluded(ns string) bool {
	for _, pat := range c.ExcludeNamespaces {
		if ok, _ := path.Match(pat, ns); ok {
			return true
		}
	}
	return false
}

func splitList(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' })
}

// clusterColors maps PICKLE_COLOR names to the marker put in front of every
// commit subject, so commits from different clusters stand apart at a glance.
var clusterColors = map[string]string{
	"red":    "🔴",
	"orange": "🟠",
	"yellow": "🟡",
	"green":  "🟢",
	"blue":   "🔵",
	"purple": "🟣",
	"brown":  "🟤",
	"black":  "⚫",
	"white":  "⚪",
}

func parseColor(v string) (string, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return "", nil
	}
	if m, ok := clusterColors[v]; ok {
		return m, nil
	}
	return "", fmt.Errorf("PICKLE_COLOR: unknown color %q (use red, orange, yellow, green, blue, purple, brown, black or white)", v)
}

var clusterNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func loadConfig() (*config, error) {
	c := &config{
		Cluster:           os.Getenv("PICKLE_CLUSTER"),
		RepoDir:           env("PICKLE_REPO_DIR", "/data/repo"),
		GitURL:            os.Getenv("PICKLE_GIT_URL"),
		GitBranch:         env("PICKLE_GIT_BRANCH", "main"),
		GitSSHUser:        env("PICKLE_GIT_SSH_USER", "git"),
		GitSSHKeyFile:     env("PICKLE_GIT_SSH_KEY_FILE", "/etc/pickle/git/ssh-privatekey"),
		GitKnownHostsFile: env("PICKLE_GIT_KNOWN_HOSTS_FILE", "/etc/pickle/git/known_hosts"),
		GitTokenFile:      env("PICKLE_GIT_TOKEN_FILE", "/etc/pickle/git/token"),
		GitUsername:       env("PICKLE_GIT_USERNAME", "pickle"),
		AuthorName:        env("PICKLE_GIT_AUTHOR_NAME", "pickle"),
		MetricsAddr:       env("PICKLE_METRICS_ADDR", ":9090"),
	}
	if !clusterNameRe.MatchString(c.Cluster) {
		return nil, fmt.Errorf("PICKLE_CLUSTER must be lowercase letters, digits and dashes (got %q)", c.Cluster)
	}
	if err := checkGitURL(c.GitURL); err != nil {
		return nil, err
	}
	c.AuthorEmail = env("PICKLE_GIT_AUTHOR_EMAIL", "pickle@"+c.Cluster+".local")

	var err error
	if c.Color, err = parseColor(os.Getenv("PICKLE_COLOR")); err != nil {
		return nil, err
	}
	delayDefault := 10 * time.Second
	if os.Getenv("PICKLE_COMMIT_DELAY") == "" && os.Getenv("PICKLE_COMMIT_INTERVAL") != "" {
		c.Deprecated = append(c.Deprecated, "PICKLE_COMMIT_INTERVAL is deprecated; it is used as PICKLE_COMMIT_DELAY")
		if delayDefault, err = durEnv("PICKLE_COMMIT_INTERVAL", delayDefault); err != nil {
			return nil, err
		}
	}
	if c.CommitDelay, err = durEnv("PICKLE_COMMIT_DELAY", delayDefault); err != nil {
		return nil, err
	}
	if c.CommitMaxDelay, err = durEnv("PICKLE_COMMIT_MAX_DELAY", time.Minute); err != nil {
		return nil, err
	}
	if c.CommitMaxDelay < c.CommitDelay {
		return nil, fmt.Errorf("PICKLE_COMMIT_MAX_DELAY (%s) must not be shorter than PICKLE_COMMIT_DELAY (%s)", c.CommitMaxDelay, c.CommitDelay)
	}
	if c.FingerprintKey, err = readFingerprintKey(env("PICKLE_SECRET_FINGERPRINT_KEY_FILE", "/etc/pickle/git/fingerprint-key")); err != nil {
		return nil, err
	}
	if c.RediscoveryInterval, err = optDurEnv("PICKLE_REDISCOVERY_INTERVAL"); err != nil {
		return nil, err
	}
	if c.Exclude, err = parseExclude(os.Getenv("PICKLE_EXCLUDE")); err != nil {
		return nil, err
	}
	c.ExcludeNamespaces = splitList(os.Getenv("PICKLE_EXCLUDE_NAMESPACES"))
	for _, pat := range c.ExcludeNamespaces {
		if _, err := path.Match(pat, ""); err != nil {
			return nil, fmt.Errorf("PICKLE_EXCLUDE_NAMESPACES: invalid pattern %q", pat)
		}
	}
	if c.CompactEvery, err = intEnv("PICKLE_GIT_COMPACT_EVERY", 256); err != nil {
		return nil, err
	}
	if err := c.LogLevel.UnmarshalText([]byte(env("PICKLE_LOG_LEVEL", "info"))); err != nil {
		return nil, fmt.Errorf("PICKLE_LOG_LEVEL: use debug, info, warn or error")
	}
	if c.PagePause, err = durEnv("PICKLE_PAGE_PAUSE", 200*time.Millisecond); err != nil {
		return nil, err
	}
	ps, err := intEnv("PICKLE_PAGE_SIZE", 50)
	if err != nil {
		return nil, err
	}
	c.PageSize = int64(ps)
	qps, err := intEnv("PICKLE_QPS", 5)
	if err != nil {
		return nil, err
	}
	c.QPS = float32(qps)
	if c.Burst, err = intEnv("PICKLE_BURST", 10); err != nil {
		return nil, err
	}
	return c, nil
}

// minFingerprintKey keeps the key out of reach of guessing: whoever has the
// key and the repo can test candidate values against a fingerprint.
const minFingerprintKey = 32

// readFingerprintKey reads the optional key for Secret value fingerprints.
// A missing file turns fingerprints off.
func readFingerprintKey(p string) ([]byte, error) {
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("secret fingerprint key: %w", err)
	}
	b = bytes.TrimSpace(b)
	if len(b) < minFingerprintKey {
		return nil, fmt.Errorf("secret fingerprint key %s must be at least %d characters (e.g. openssl rand -hex 32)", p, minFingerprintKey)
	}
	return b, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func durEnv(k string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s: invalid duration %q", k, v)
	}
	return d, nil
}

// optDurEnv reads an optional interval: empty, "0" or "off" disables it.
func optDurEnv(k string) (time.Duration, error) {
	switch v := os.Getenv(k); v {
	case "", "0", "off":
		return 0, nil
	default:
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Minute {
			return 0, fmt.Errorf("%s: invalid duration %q (use 0 or off to disable, otherwise at least 1m)", k, v)
		}
		return d, nil
	}
}

// checkGitURL rejects URLs that would leak credentials: secrets belong in the
// pickle-git Secret, never in the ConfigMap or in logged URLs.
func checkGitURL(u string) error {
	if u == "" {
		return fmt.Errorf("PICKLE_GIT_URL is required")
	}
	if strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "ssh://") {
		pu, err := url.Parse(u)
		if err != nil {
			return fmt.Errorf("PICKLE_GIT_URL: %w", err)
		}
		if _, hasPassword := pu.User.Password(); hasPassword {
			return fmt.Errorf("PICKLE_GIT_URL must not contain a password or token; put it in the pickle-git Secret")
		}
		if pu.Scheme != "ssh" && pu.User != nil {
			return fmt.Errorf("PICKLE_GIT_URL must not contain credentials; set PICKLE_GIT_USERNAME and the token in the pickle-git Secret")
		}
	}
	return nil
}

func intEnv(k string, def int) (int, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s: invalid positive integer %q", k, v)
	}
	return n, nil
}
