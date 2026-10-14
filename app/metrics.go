package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// metrics is a tiny Prometheus text exporter; no client library needed.
type metrics struct {
	cluster string

	backfillNSTotal  atomic.Int64
	backfillNSDone   atomic.Int64
	backfillComplete atomic.Int64

	objectsTracked atomic.Int64
	written        atomic.Int64
	deleted        atomic.Int64

	commits      atomic.Int64
	pushFailures atomic.Int64
	lastPush     atomic.Int64

	watchReconnects atomic.Int64
	resyncs         atomic.Int64

	resourcesWatched   atomic.Int64
	resourcesForbidden atomic.Int64
}

func newMetrics(cluster string) *metrics { return &metrics{cluster: cluster} }

func (m *metrics) write(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	g := func(name, help, typ string, v int64) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s{cluster=%q} %d\n", name, help, name, typ, name, m.cluster, v)
	}
	g("pickle_backfill_namespaces_total", "Namespaces to backfill.", "gauge", m.backfillNSTotal.Load())
	g("pickle_backfill_namespaces_done", "Namespaces backfilled so far.", "gauge", m.backfillNSDone.Load())
	g("pickle_backfill_complete", "1 once the initial backfill has finished.", "gauge", m.backfillComplete.Load())
	g("pickle_objects_tracked", "Objects currently mirrored in the repo.", "gauge", m.objectsTracked.Load())
	g("pickle_objects_written_total", "Object files written (created or changed).", "counter", m.written.Load())
	g("pickle_objects_deleted_total", "Object files removed.", "counter", m.deleted.Load())
	g("pickle_commits_total", "Commits pushed.", "counter", m.commits.Load())
	g("pickle_push_failures_total", "Failed fetch/commit/push cycles.", "counter", m.pushFailures.Load())
	g("pickle_last_push_timestamp_seconds", "Unix time of the last successful push.", "gauge", m.lastPush.Load())
	g("pickle_watch_reconnects_total", "Watch streams reopened (normal, every 5-10 minutes per type).", "counter", m.watchReconnects.Load())
	g("pickle_resyncs_total", "Full relists of a type after its watch expired (410 Gone).", "counter", m.resyncs.Load())
	g("pickle_resources_watched", "Resource types being watched.", "gauge", m.resourcesWatched.Load())
	g("pickle_resources_forbidden", "Discovered resource types pickle cannot read (listed in _unreadable.txt).", "gauge", m.resourcesForbidden.Load())
}

func serveMetrics(addr string, m *metrics) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) { m.write(w) })
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("metrics server", "err", err)
	}
}
