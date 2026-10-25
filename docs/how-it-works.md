# How it works

## Startup

1. **Discover** every resource type that supports `list` and `watch`, minus noise, subresources and `PICKLE_EXCLUDE`.
2. **Probe** each type with a 1-item `LIST`. Failures are skipped and listed in `<cluster>/_unreadable.txt` with the reason.
3. **Watch** each readable type cluster-wide, from now on.
4. **Backfill** existing objects: cluster-scoped first, then each namespace alphabetically (minus `PICKLE_EXCLUDE_NAMESPACES`), `PICKLE_PAGE_SIZE` per page with `PICKLE_PAGE_PAUSE` between pages, at most `PICKLE_QPS` requests per second.
5. **Clean up** files of objects deleted while pickle was down (only for fully listed types, so a permission hiccup never wipes files), and files of anything now excluded.
6. **Push** an `initial sync complete` commit.

Watches start before the backfill because a long backfill outlives etcd's compaction window (~5 minutes): a watch started afterwards would fail with `410 Gone`. Objects a watch already wrote are skipped by the backfill, since a backfill page is always older.

Afterwards:

- Watches reconnect every 30–60 minutes from the last seen resourceVersion. A reconnect is one request, not a relist.
- An expired watch (`410 Gone`) triggers a full relist of that type.
- Any change to a CustomResourceDefinition or APIService triggers a rediscovery 5 seconds later, so new CRDs are picked up within seconds and nothing polls for them. Set `PICKLE_REDISCOVERY_INTERVAL` to also rediscover on a timer.

Nothing runs on a timer while the cluster is quiet: no commit ticks and no discovery polling.

## Normalization

Kept: `apiVersion`, `kind`, `name`, `namespace`, `labels`, `annotations`, and the body.

| Removed | Why |
|---|---|
| `status` | runtime state |
| `uid`, `resourceVersion`, `generation`, `creationTimestamp`, `selfLink` | server-assigned |
| `managedFields` | used for the header, then dropped |
| `finalizers`, `ownerReferences` | would block deletes or point at missing UIDs |
| `last-applied-configuration` annotation | duplicate of the object |
| `deployment.kubernetes.io/revision`, `pv.kubernetes.io/*`, `volume.kubernetes.io/selected-node` | controller bookkeeping |
| Service `clusterIP(s)`, except `None` | allocated per cluster |
| PVC `volumeName`, PV `claimRef.uid` / `resourceVersion` | bound per cluster |

Output is deterministic. Unchanged bytes mean no write and no commit, so status updates are free.

## Header

From `managedFields`, ignoring the `status` subresource:

```yaml
# last-manager: kubectl-edit (Update)
# last-changed: 2026-10-14T17:52:03Z
# argocd-app: payments-api
```

- `last-manager`: the tool that last changed the object (`argocd-controller`, `helm`, `kubectl-edit`, an operator...).
- `last-changed`: when that happened on the cluster.
- `argocd-app`: from Argo's tracking annotation or instance label.

The header is only refreshed when the rest of the file changes. Some controllers, such as the Argo CD application controller, write status through the object itself, which moves its `managedFields` time on every reconcile even though nothing pickle writes has changed. Secrets are the exception: their values are masked, so a header change is the only sign a value changed, and it is always written.

The user and source IP aren't stored on objects; only API server audit logs have them.

## Skipped

- **Derived objects:** anything with `ownerReferences` (ReplicaSets, Pods, Jobs of CronJobs, Applications from ApplicationSets). Their parent is mirrored.
- **Runtime churn:** events, leases, endpoints, endpointslices, controllerrevisions, volumeattachments, csinodes, csistoragecapacities, certificatesigningrequests, ipaddresses, resourceslices, componentstatuses, and the `metrics.k8s.io` group.
- **ServiceAccount token Secrets**, which the cluster regenerates.
- **Your exclusions:** `PICKLE_EXCLUDE` and `PICKLE_EXCLUDE_NAMESPACES`, see [configuration](configuration.md#excluding).

## Layout

```text
<cluster>/
  _kinds.txt
  _unreadable.txt
  _cluster/<kind>-<name>.yaml
  <namespace>/<kind>-<name>.yaml
  <namespace>/_secrets-referenced.txt
```

- Lowercase; characters outside `a-z 0-9 . -` become `_` (`system:view` → `clusterrole-system_view.yaml`).
- `_cluster` can't collide with a namespace, since namespaces can't contain `_`.
- Helper files end in `.txt`, so `kubectl apply -R` ignores them.

When two API groups define the same kind, the first keeps the short prefix (`certificate-web.yaml`) and later ones add their group (`certificate.acme.example.com-web.yaml`). Built-in groups claim first. Claims live in `_kinds.txt`, so files are never renamed.

## Committing

A change wakes the committer, which waits until changes have been quiet for `PICKLE_COMMIT_DELAY`, but never longer than `PICKLE_COMMIT_MAX_DELAY` after the first one. A Helm upgrade touching 30 objects becomes one commit. Then: fetch the remote head and build a commit on top of it that rewrites only the trees along pickle's changed paths, so a commit costs the same in a small repo as in a huge one. If another cluster pushed in between, pickle takes the new remote head, puts its own `<cluster>/` directory on top and pushes again after a short random pause; since each cluster only ever writes its own directory, this never needs a merge and never touches another cluster's files. Other failures (Git host unreachable, auth) are retried with backoff, up to 5 minutes. On `SIGTERM`, pickle pushes once more before exiting.

## Local clone

The clone is shallow: pickle fetches only the branch tip, because all it needs is something to build the next commit on. History is never downloaded, kept on disk or held in memory, however long the shared branch gets.

Each commit still leaves a few objects behind. Every `PICKLE_GIT_COMPACT_EVERY` commits, pickle builds a fresh shallow store next to the old one, swaps it in and deletes the old one. If anything fails, the old store is kept. Without this, the store would slowly fill the `emptyDir` until the pod is evicted.

The first clone writes a marker inside `.git` and removes it once the checkout is complete. An interrupted clone is redone on the next start. A `PICKLE_REPO_DIR` that is non-empty and not a pickle clone is refused, never deleted.
