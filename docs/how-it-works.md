# How it works

## Startup

1. **Discover** every resource type that supports `list` and `watch`, minus noise and subresources.
2. **Probe** each type with a 1-item `LIST`. Failures are skipped and listed in `<cluster>/_unreadable.txt` with the reason.
3. **Watch** each readable type cluster-wide, from now on.
4. **Backfill** existing objects: cluster-scoped first, then each namespace alphabetically, `PICKLE_PAGE_SIZE` per page with `PICKLE_PAGE_PAUSE` between pages, at most `PICKLE_QPS` requests per second.
5. **Clean up** files of objects deleted while pickle was down. This applies only to fully listed types, so a permission hiccup never wipes files.
6. **Push** an `initial sync complete` commit.

Watches start before the backfill because a long backfill outlives etcd's compaction window (~5 minutes): a watch started afterwards would fail with `410 Gone`. Objects a watch already wrote are skipped by the backfill, since a backfill page is always older.

Afterwards:

- Watches reconnect every 5–10 minutes from the last seen resourceVersion.
- An expired watch (`410 Gone`) triggers a full relist of that type.
- Discovery reruns every `PICKLE_REDISCOVERY_INTERVAL`, picking up new CRDs without a restart.

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

The user and source IP aren't stored on objects; only API server audit logs have them.

## Skipped

- **Derived objects:** anything with `ownerReferences` (ReplicaSets, Pods, Jobs of CronJobs, Applications from ApplicationSets). Their parent is mirrored.
- **Runtime churn:** events, leases, endpoints, endpointslices, controllerrevisions, volumeattachments, csinodes, csistoragecapacities, certificatesigningrequests, ipaddresses, resourceslices, componentstatuses, and the `metrics.k8s.io` group.
- **ServiceAccount token Secrets**, which the cluster regenerates.

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

Every `PICKLE_COMMIT_INTERVAL`, if anything changed: fetch, point the index at the remote head, stage only pickle's changed paths, commit, push. A failed push is retried on the next tick on top of the new head, so clusters sharing a branch never conflict. On `SIGTERM`, pickle pushes once more before exiting.
