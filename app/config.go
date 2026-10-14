package main

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"time"
)

type config struct {
	Cluster string
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

	CommitInterval      time.Duration
	RediscoveryInterval time.Duration
	PageSize            int64
	PagePause           time.Duration
	QPS                 float32
	Burst               int

	MetricsAddr string
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
	if c.GitURL == "" {
		return nil, fmt.Errorf("PICKLE_GIT_URL is required")
	}
	c.AuthorEmail = env("PICKLE_GIT_AUTHOR_EMAIL", "pickle@"+c.Cluster+".local")

	var err error
	if c.CommitInterval, err = durEnv("PICKLE_COMMIT_INTERVAL", 13*time.Second); err != nil {
		return nil, err
	}
	if c.RediscoveryInterval, err = durEnv("PICKLE_REDISCOVERY_INTERVAL", 10*time.Minute); err != nil {
		return nil, err
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
