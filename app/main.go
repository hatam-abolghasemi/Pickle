// pickle watches every readable Kubernetes object and keeps its applicable,
// normalized YAML in a Git repository: reverse GitOps for configuration.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg, err := loadConfig()
	if err != nil {
		fatal("config", err)
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})))

	rc, err := restConfig()
	if err != nil {
		fatal("kubernetes config", err)
	}
	rc.QPS = cfg.QPS
	rc.Burst = cfg.Burst
	rc.UserAgent = "pickle"
	for _, d := range cfg.Deprecated {
		slog.Warn(d)
	}

	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		fatal("dynamic client", err)
	}
	disco, err := discovery.NewDiscoveryClientForConfig(rc)
	if err != nil {
		fatal("discovery client", err)
	}

	m := newMetrics(cfg.Cluster)
	go serveMetrics(cfg.MetricsAddr, m)

	store, err := openGitStore(cfg)
	if err != nil {
		fatal("git", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	e, err := newEngine(cfg, dyn, disco, store, m)
	if err != nil {
		fatal("engine", err)
	}
	slog.Info("pickle starting", "cluster", cfg.Cluster, "branch", cfg.GitBranch, "secretFingerprints", len(cfg.FingerprintKey) > 0)
	if err := e.run(ctx); err != nil {
		fatal("run", err)
	}
	slog.Info("pickle stopped")
}

func restConfig() (*rest.Config, error) {
	if kc := os.Getenv("KUBECONFIG"); kc != "" {
		return clientcmd.BuildConfigFromFlags("", kc)
	}
	return rest.InClusterConfig()
}

func fatal(what string, err error) {
	slog.Error(what, "err", err)
	os.Exit(1)
}
