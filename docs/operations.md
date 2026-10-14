# Operations

## Metrics

Prometheus format on `:9090/metrics`, exposed by the `pickle` Service.

| Metric | |
|---|---|
| `pickle_backfill_namespaces_total` / `_done` | Backfill progress |
| `pickle_backfill_complete` | `1` after the initial sync |
| `pickle_objects_tracked` | Objects in the repo |
| `pickle_objects_written_total` / `_deleted_total` | Files written / removed |
| `pickle_commits_total` | Commits pushed |
| `pickle_push_failures_total` | Failed push cycles (retried) |
| `pickle_last_push_timestamp_seconds` | Last successful push |
| `pickle_watch_reconnects_total` | Normal watch reconnects |
| `pickle_resyncs_total` | Relists after an expired watch |
| `pickle_resources_watched` | Types watched |
| `pickle_resources_forbidden` | Types listed in `_unreadable.txt` |

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
| `backfill progress namespace=... done=... total=...` | One per namespace |
| `backfill complete objects=N staleRemoved=M` | Initial sync done |
| `pushed changes=N` | A commit went out |
| `commit/push failed; will retry` | Changes are kept and retried |
| `new readable type resource=...` | A new type was picked up |

## Troubleshooting

**`_unreadable.txt` appeared.** Each line names a type and why. `forbidden`: a policy or ClusterRole blocks it. `discovery failed`: an aggregated API (e.g. metrics-server) is down. Once fixed, the type is picked up within `PICKLE_REDISCOVERY_INTERVAL` and the file disappears.

**`known_hosts (required ...)`.** The `pickle-git` Secret lacks `known_hosts`; recreate it with `ssh-keyscan` output.

**Slow backfill.** By design; live changes are captured from the start anyway. To speed it up, raise `PICKLE_QPS` and `PICKLE_PAGE_SIZE` or lower `PICKLE_PAGE_PAUSE`.

**An object is missing.** It probably has `ownerReferences`, or its type is [skipped](how-it-works.md#skipped).

## Resources

Requests 20m CPU / 64Mi, limits 500m / 512Mi. Pickle keeps a 32-byte hash per object, not the objects; most memory is the Git index.
