# Configuration

Settings are environment variables from one ConfigMap: [`k8s/configmap.yaml`](../k8s/configmap.yaml), or rendered by Helm from [`values.yaml`](../helm/pickle/values.yaml). With plain manifests, restart after changing it:

```bash
sudo kubectl -n pickle rollout restart deploy/pickle
```

| Variable | Default | |
|---|---|---|
| `PICKLE_CLUSTER` | required | Root directory in the repo (`a-z 0-9 -`), one per cluster: a directory another cluster has claimed is refused |
| `PICKLE_GIT_URL` | required | `git@host:path`, `ssh://...` or `https://...`, never with credentials in it |
| `PICKLE_GIT_BRANCH` | `main` | Created on first push |
| `PICKLE_COMMIT_DELAY` | `10s` | Commit once changes have been quiet this long |
| `PICKLE_COMMIT_MAX_DELAY` | `1m` | Commit at the latest this long after the first pending change |
| `PICKLE_COLOR` | empty | Marker before every commit subject: `red`, `orange`, `yellow`, `green`, `blue`, `purple`, `brown`, `black` or `white` (🔴🟠🟡🟢🔵🟣🟤⚫⚪) |
| `PICKLE_EXCLUDE` | empty | Types never mirrored; see [Excluding](#excluding) |
| `PICKLE_EXCLUDE_NAMESPACES` | empty | Namespaces never mirrored; shell patterns like `tmp-*` |
| `PICKLE_METADATA_WATCH` | `core/pods,apps/replicasets,batch/jobs` | Types watched as metadata only: owned objects are skipped without reading their spec and status, objects without an owner are read whole. For types whose objects nearly always have an owner and change all the time. Empty: every type is read whole |
| `PICKLE_EXCLUDE_SECRET_TYPES` | `helm.sh/release.v1` | Secret types never mirrored, e.g. `kubernetes.io/tls`; see [Excluding](#excluding). Empty: every type is mirrored |
| `PICKLE_MIRROR_OWNED_BY` | `SealedSecret.bitnami.com` | Owner kinds whose objects are mirrored anyway; see [Owned objects](#owned-objects). Empty: every owned object is skipped |
| `PICKLE_REDISCOVERY_INTERVAL` | `off` | Optional timer for picking up new types; at least `1m` |
| `PICKLE_PAGE_SIZE` | `50` | Objects per LIST page |
| `PICKLE_PAGE_PAUSE` | `200ms` | Pause between pages |
| `PICKLE_QPS` / `PICKLE_BURST` | `5` / `10` | API rate limit |
| `PICKLE_GIT_COMPACT_EVERY` | `256` | Commits between local store compactions (local only, no network) |
| `PICKLE_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `PICKLE_METRICS_ADDR` | `:9090` | `/metrics` and `/healthz` |
| `PICKLE_GIT_SSH_USER` | `git` | |
| `PICKLE_GIT_USERNAME` | `pickle` | HTTPS username |
| `PICKLE_GIT_AUTHOR_NAME` | `pickle` | |
| `PICKLE_GIT_AUTHOR_EMAIL` | `pickle@<cluster>.local` | |
| `PICKLE_SECRET_FINGERPRINT_KEY_FILE` | `/etc/pickle/git/fingerprint-key` | Key for Secret value fingerprints; a missing file turns them off |
| `PICKLE_SECRET_ENCRYPTION_CERT_FILE` | `/etc/pickle/git/encryption-cert` | Certificate to encrypt Secret values to; a missing file turns encryption off |
| `PICKLE_REPO_DIR` | `/data/repo` | Local clone. Must be empty or a pickle clone; anything else is refused, never deleted |
| `GOGC` | `50` | Go's GC target; at 50 the heap grows to 1.5x the live set after a burst, not 2x |
| `GOMEMLIMIT` | 90% of the container's memory limit | Read from pickle's own cgroup at startup; the rest is headroom. Set it only to override |
| `PICKLE_PAGE_CACHE_DROP_DELAY` | `40s` | Used only when the kernel's writeback window can't be read from `/proc/sys/vm`; see [Memory](operations.md#resources) |

`PICKLE_COMMIT_INTERVAL` from 1.0.0 still works as `PICKLE_COMMIT_DELAY`, with a warning in the log.

In Helm, these map to `cluster`, `color`, `config.*`, `git.*` and `secrets.*` in [`values.yaml`](../helm/pickle/values.yaml); anything else can go in `extraEnv`.

## Excluding

`PICKLE_EXCLUDE` takes a comma-separated list. Each entry is one of:

| Entry | Excludes |
|---|---|
| `IPAMBlock` | That kind (or plural resource name) in every API group |
| `IPAMBlock.crd.projectcalico.org` | That kind (or plural) in one group; use `core` for the core group, e.g. `secrets.core` |
| `*.crd.projectcalico.org` | Every type of one group |

```yaml
PICKLE_EXCLUDE: "IPAMBlock,BlockAffinity"
PICKLE_EXCLUDE_NAMESPACES: "scratch,tmp-*"
```

With Helm:

```yaml
config:
  exclude:
    - IPAMBlock
  excludeNamespaces:
    - tmp-*
```

Excluded types are never watched or listed, so they cost nothing. Objects in excluded namespaces are skipped in the backfill, and their events are dropped, including the Namespace object itself. Files mirrored before an exclusion are removed on the next start, in the `initial sync complete` commit.

`PICKLE_EXCLUDE_SECRET_TYPES` drops Secrets by their `type`, a comma-separated list of exact values. The default, `helm.sh/release.v1`, drops Helm's release records: each is a whole release, chart and values gzipped, rewritten on every upgrade, while the objects it installed are mirrored on their own. Set it to empty to mirror them anyway. Service account tokens are always skipped. Their files go away on the next start, like any exclusion.

These come on top of the built-in exclusions in [how it works](how-it-works.md#skipped).

## Owned objects

Objects with `ownerReferences` are skipped: their parent is mirrored and recreates them. `PICKLE_MIRROR_OWNED_BY` makes an exception for objects owned by the kinds it lists, written `Kind` (any group), `Kind.group` or `*.group`, matched against the owner's `apiVersion` and `kind`. One matching owner is enough.

The default, `SealedSecret.bitnami.com`, is for [Sealed Secrets](https://github.com/bitnami-labs/sealed-secrets). A SealedSecret only opens with the controller's private key, so a repo holding only SealedSecrets can't be restored without that key. With the default, each Secret the controller made is mirrored next to its SealedSecret, masked or [encrypted](security.md#encrypted-values) like any Secret, and restores with pickle's key alone. See [restore](restore.md#sealed-secrets) for which one to apply.

```yaml
PICKLE_MIRROR_OWNED_BY: "SealedSecret.bitnami.com,Prometheus.monitoring.coreos.com"
```

With Helm:

```yaml
config:
  mirrorOwnedBy:
    - SealedSecret.bitnami.com
```

Mirrored files never carry `ownerReferences`. Set it to empty to skip every owned object, as in 1.2.1 and earlier.

## Git credentials

Credentials never go in the ConfigMap. They're files from the `pickle-git` Secret, mounted at `/etc/pickle/git/`:

| Key | Used for |
|---|---|
| `ssh-privatekey` + `known_hosts` | SSH URLs. Host keys are always verified. |
| `token` | HTTPS URLs. Without it, pickle connects anonymously. |
| `fingerprint-key` | Optional. Secret values are written as `****:<fingerprint>` so a changed value shows up under its key; see [security](security.md#secrets). At least 32 characters. |
| `encryption-cert` | Optional. Your certificate (RSA, 2048 bits or more); Secret values are written encrypted to it, see [security](security.md#encrypted-values). Needs `fingerprint-key`. |

SSH setup is in the [README](../README.md#quick-start). For HTTPS:

```bash
sudo kubectl -n pickle create secret generic pickle-git --from-literal=token=YOUR_TOKEN
```

With Helm, set `git.token`, or `git.existingSecret` to use a Secret you manage.

## Running locally

With `KUBECONFIG` set, pickle uses it instead of the in-cluster ServiceAccount:

```bash
git init --bare /tmp/cluster-state.git
```

```bash
KUBECONFIG=~/.kube/config PICKLE_CLUSTER=stg PICKLE_GIT_URL=/tmp/cluster-state.git PICKLE_REPO_DIR=/tmp/pickle-repo go -C app run .
```
