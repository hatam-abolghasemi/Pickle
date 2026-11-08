# Operations

## Metrics

Prometheus format on `:9090/metrics`, exposed by the `pickle` Service. The pod carries `prometheus.io/scrape` annotations, and on clusters with Prometheus Operator, which ignores them, the chart also creates a ServiceMonitor. If your Prometheus only picks up ServiceMonitors with a certain label, set it under `serviceMonitor.labels`, such as `release: prometheus`. If it scrapes pods by their annotations too, turn the ServiceMonitor off, or pickle is scraped twice:

```yaml
serviceMonitor:
  enabled: false
```

| Metric | |
|---|---|
| `pickle_build_info` | `1`, with the `version` and `goversion` labels |
| `pickle_start_time_seconds` | When pickle started |
| `pickle_backfill_types_total` / `_done` | Backfill progress |
| `pickle_backfill_complete` | `1` after the initial sync |
| `pickle_objects_tracked` | Objects in the repo |
| `pickle_objects_written_total` / `_deleted_total` | Files written / removed |
| `pickle_commits_total` | Commits pushed |
| `pickle_push_failures_total` | Failed push cycles (retried) |
| `pickle_push_conflicts_total` | Pushes where another cluster had pushed to the branch first (handled by pushing again on top) |
| `pickle_last_push_timestamp_seconds` | Last successful push |
| `pickle_watch_reconnects_total` | Normal watch reconnects (every 30–60 min per type) |
| `pickle_resyncs_total` | Relists after an expired watch |
| `pickle_resources_watched` | Types watched |
| `pickle_resources_forbidden` | Types listed in `_unreadable.txt` |
| `pickle_secret_encryption_enabled` | `1` with [encrypted Secret values](security.md#encrypted-values) on |
| `pickle_secret_values_encrypted_total` | Secret values encrypted (only new or changed ones) |
| `pickle_secret_encryption_failures_total` | Secret values that couldn't be encrypted and were written masked |
| `pickle_go_heap_live_bytes` | Heap held by live objects after the last GC |
| `pickle_go_memory_mapped_bytes` / `_released_bytes` | Memory mapped by the Go runtime / already returned to the OS |
| `pickle_go_goroutines` | About one per watched type |

### Dashboard

[`dashboards/pickle.json`](../dashboards/pickle.json) is a Grafana dashboard (Grafana 13, v2 dashboard schema) with tabs for the overview, Git, the Kubernetes API, Secrets and pickle's own runtime. Import it under **Dashboards → New → Import**, then pick your Prometheus or Mimir data source and the cluster. The memory and CPU panels also need cAdvisor metrics, joined to pickle on the `namespace` and `pod` labels your scrape config adds.

### Alerts

```yaml
- alert: PickleNotPushing
  expr: increase(pickle_push_failures_total[30m]) > 0 and time() - pickle_last_push_timestamp_seconds > 1800
  annotations:
    summary: "pickle on {{ $labels.cluster }} has failed to push for 30 minutes"

- alert: PickleMissingTypes
  expr: pickle_resources_forbidden > 0
  for: 1h
  annotations:
    summary: "pickle on {{ $labels.cluster }} cannot read some types, see _unreadable.txt"

- alert: PickleResyncStorm
  expr: increase(pickle_resyncs_total[1h]) > 20
  annotations:
    summary: "pickle on {{ $labels.cluster }} keeps losing its watches"

- alert: PickleSecretsNotEncrypted
  expr: increase(pickle_secret_encryption_failures_total[1h]) > 0
  annotations:
    summary: "pickle on {{ $labels.cluster }} wrote Secret values masked instead of encrypted, see the logs"
```

A quiet cluster has an old last-push time; that's normal. `PickleNotPushing` fires only on actual failures.

## Logs

```bash
sudo kubectl -n pickle logs -f deploy/pickle
```

| Line | Meaning |
|---|---|
| `pickle starting version=... secretEncryption=true` | Startup; encrypted Secret values are on |
| `watching types=N unreadable=M` | Startup |
| `backfill progress resource=... done=... total=...` | One per type |
| `backfill complete objects=N staleRemoved=M` | Initial sync done |
| `pushed changes=N` | A commit went out |
| `commit/push failed; will retry` | Changes are kept and retried |
| `new readable type resource=...` | A new type was picked up |
| `excluded by PICKLE_EXCLUDE resource=...` | A type matched an exclusion |
| `local git store compacted` | The local store was replaced by a sparse copy of itself (no network) |
| `the remote branch shares no commit with the local clone` | The branch's history was rewritten; the head is downloaded afresh |
| `secret value not encrypted, written masked` | A Secret value wasn't valid base64 or couldn't be encrypted; it's `****:<fingerprint>` in Git |

## Troubleshooting

**`_unreadable.txt` appeared.** Each line names a type and why. `forbidden`: a policy or ClusterRole blocks it. `discovery failed`: an aggregated API (e.g. metrics-server) is down. Once fixed, the type is picked up as soon as its APIService reports a change (or at the next `PICKLE_REDISCOVERY_INTERVAL`, if set) and the file disappears. For an authorization change that no object reflects, restart pickle or set an interval.

**`mirrors another cluster (kube-system UID ...)`.** Pickle exits because `<cluster>/` in the repo belongs to a different cluster: two installs share one `PICKLE_CLUSTER`. Give each cluster its own name. If the directory really is this cluster's (the cluster was rebuilt, so `kube-system` has a new UID), delete `<cluster>/_cluster-id.txt` from the repo and restart pickle; it claims the directory again on its next start.

**`known_hosts (required ...)`.** The `pickle-git` Secret lacks `known_hosts`; recreate it with `ssh-keyscan` output.

**Slow backfill.** By design; live changes are captured from the start anyway. To speed it up, raise `PICKLE_QPS` and `PICKLE_PAGE_SIZE` or lower `PICKLE_PAGE_PAUSE`.

**An object is missing.** It probably has `ownerReferences`, its type or namespace is [skipped](how-it-works.md#skipped), or it matches `PICKLE_EXCLUDE`.

**A type is just noise in Git.** Exclude it, e.g. `PICKLE_EXCLUDE: "IPAMBlock"`. Its files go away on the next start.

**`pickle decrypt` fails.** It writes nothing when any value fails, so nothing half-decrypted gets applied. `wrong key`: the value was encrypted to another certificate; use the key that goes with it, such as the old key for commits from before a new certificate. `damaged or altered`: the blob isn't what pickle wrote; take the file from another commit. `no private key found`: put it at `~/.config/pickle/pickle.key` or pass `-key`. `password-protected`: write an unprotected copy of the key first (`openssl pkey -in pickle.key -out plain.key`). `skipped ... masked` is only a note: those Secrets were written before encryption was on, so fill them in from your vault. `not git-ignored`: you ran it outside a git clone, or Docker didn't see the clone's `.git` because you didn't run it from the clone's root.

**`encryption-cert ...: needs fingerprint-key`.** Encrypted Secret values need a fingerprint key too: add `fingerprint-key` to the `pickle-git` Secret (see [security](security.md#encrypted-values)). Other `encryption-cert` errors mean the file isn't a PEM certificate with an RSA key of at least 2048 bits.

**`PICKLE_REPO_DIR ... is not empty and is not a pickle clone`.** The directory holds something pickle didn't create. Point it at an empty directory; pickle won't delete it for you.

## Resources

Requests 20m CPU / 48Mi, limits 500m / 128Mi.

Pickle keeps two 32-byte hashes per object, not the objects, and of the Git repo only the head's root tree and its own cluster's directory. The rest of its memory is the Go runtime and client-go (a floor of roughly 15 MiB), a git object cache capped at 2 MiB, and short bursts during backfill, relists and pushes. After each burst the freed memory is returned to the OS. `GOMEMLIMIT` is set from the memory limit, so the GC works harder before the container would be OOM-killed.

None of the Git work grows with the repo or with the number of clusters sharing the branch, except the first download at startup, which is streamed through a file and indexed in a few MiB. In a test repo of four clusters and 52,000 files (a 31 MiB pack), the startup clone peaked at 21 MiB of heap and each sync cycle at 7 MiB. Big list pages are the other burst: CustomResourceDefinitions, often hundreds of KiB each, are listed 5 per page whatever `PICKLE_PAGE_SIZE` says.

The container's working set also counts the kernel's page cache of the files pickle writes into the `emptyDir`: the mirrored YAML and git's objects. Pickle hands those pages back about 40 seconds after a push (once the kernel has written them to disk), and after reading the repo at startup, so they don't pile up with the size of the cluster. Compare `container_memory_rss` with `pickle_go_heap_live_bytes` to see what pickle itself holds.

[Encrypted Secret values](security.md#encrypted-values) cost nothing while values don't change: an unchanged Secret is recognized from its fingerprints, without reading its file or encrypting. A new or changed value costs about 0.4 ms of CPU with a 4096-bit key. Turning encryption on, or changing the certificate, encrypts every value once: a cluster with 20,000 Secret values spends about 8 CPU-seconds on it, spread over the backfill.
