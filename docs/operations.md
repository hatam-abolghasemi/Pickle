# Operations

## Metrics

Prometheus format on `:9090/metrics`, exposed by the `pickle` Service.

| Metric | |
|---|---|
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
| `pickle_go_heap_live_bytes` | Heap held by live objects after the last GC |
| `pickle_go_memory_mapped_bytes` / `_released_bytes` | Memory mapped by the Go runtime / already returned to the OS |
| `pickle_go_goroutines` | About one per watched type |

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
```

A quiet cluster has an old last-push time; that's normal. `PickleNotPushing` fires only on actual failures.

## Logs

```bash
sudo kubectl -n pickle logs -f deploy/pickle
```

| Line | Meaning |
|---|---|
| `watching types=N unreadable=M` | Startup |
| `backfill progress resource=... done=... total=...` | One per type |
| `backfill complete objects=N staleRemoved=M` | Initial sync done |
| `pushed changes=N` | A commit went out |
| `commit/push failed; will retry` | Changes are kept and retried |
| `new readable type resource=...` | A new type was picked up |
| `excluded by PICKLE_EXCLUDE resource=...` | A type matched an exclusion |
| `local git store compacted` | The local store was replaced by a sparse copy of itself (no network) |
| `the remote branch shares no commit with the local clone` | The branch's history was rewritten; the head is downloaded afresh |

## Troubleshooting

**`_unreadable.txt` appeared.** Each line names a type and why. `forbidden`: a policy or ClusterRole blocks it. `discovery failed`: an aggregated API (e.g. metrics-server) is down. Once fixed, the type is picked up as soon as its APIService reports a change (or at the next `PICKLE_REDISCOVERY_INTERVAL`, if set) and the file disappears. For an authorization change that no object reflects, restart pickle or set an interval.

**`mirrors another cluster (kube-system UID ...)`.** Pickle exits because `<cluster>/` in the repo belongs to a different cluster: two installs share one `PICKLE_CLUSTER`. Give each cluster its own name. If the directory really is this cluster's (the cluster was rebuilt, so `kube-system` has a new UID), delete `<cluster>/_cluster-id.txt` from the repo and restart pickle; it claims the directory again on its next start.

**`known_hosts (required ...)`.** The `pickle-git` Secret lacks `known_hosts`; recreate it with `ssh-keyscan` output.

**Slow backfill.** By design; live changes are captured from the start anyway. To speed it up, raise `PICKLE_QPS` and `PICKLE_PAGE_SIZE` or lower `PICKLE_PAGE_PAUSE`.

**An object is missing.** It probably has `ownerReferences`, its type or namespace is [skipped](how-it-works.md#skipped), or it matches `PICKLE_EXCLUDE`.

**A type is just noise in Git.** Exclude it, e.g. `PICKLE_EXCLUDE: "IPAMBlock"`. Its files go away on the next start.

**`PICKLE_REPO_DIR ... is not empty and is not a pickle clone`.** The directory holds something pickle didn't create. Point it at an empty directory; pickle won't delete it for you.

## Resources

Requests 20m CPU / 48Mi, limits 500m / 128Mi.

Pickle keeps two 32-byte hashes per object, not the objects, and of the Git repo only the head's root tree and its own cluster's directory. The rest of its memory is the Go runtime and client-go (a floor of roughly 15 MiB), a git object cache capped at 2 MiB, and short bursts during backfill, relists and pushes. After each burst the freed memory is returned to the OS. `GOMEMLIMIT` is set from the memory limit, so the GC works harder before the container would be OOM-killed.

None of the Git work grows with the repo or with the number of clusters sharing the branch, except the first download at startup, which is streamed through a file and indexed in a few MiB. In a test repo of four clusters and 52,000 files (a 31 MiB pack), the startup clone peaked at 21 MiB of heap and each sync cycle at 7 MiB. Big list pages are the other burst: CustomResourceDefinitions, often hundreds of KiB each, are listed 5 per page whatever `PICKLE_PAGE_SIZE` says.

The container's working set also counts the kernel's page cache of the files pickle writes into the `emptyDir`: the mirrored YAML and git's objects. Pickle hands those pages back about 40 seconds after a push (once the kernel has written them to disk), and after reading the repo at startup, so they don't pile up with the size of the cluster. Compare `container_memory_rss` with `pickle_go_heap_live_bytes` to see what pickle itself holds.
