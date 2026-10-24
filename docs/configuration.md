# Configuration

Settings are environment variables from one ConfigMap: [`k8s/configmap.yaml`](../k8s/configmap.yaml), or rendered by Helm from [`values.yaml`](../helm/pickle/values.yaml). With plain manifests, restart after changing it:

```bash
sudo kubectl -n pickle rollout restart deploy/pickle
```

| Variable | Default | |
|---|---|---|
| `PICKLE_CLUSTER` | required | Root directory in the repo (`a-z 0-9 -`) |
| `PICKLE_GIT_URL` | required | `git@host:path`, `ssh://...` or `https://...`, never with credentials in it |
| `PICKLE_GIT_BRANCH` | `main` | Created on first push |
| `PICKLE_COMMIT_DELAY` | `10s` | Commit once changes have been quiet this long |
| `PICKLE_COMMIT_MAX_DELAY` | `1m` | Commit at the latest this long after the first pending change |
| `PICKLE_COLOR` | empty | Marker before every commit subject: `red`, `orange`, `yellow`, `green`, `blue`, `purple`, `brown`, `black` or `white` (🔴🟠🟡🟢🔵🟣🟤⚫⚪) |
| `PICKLE_EXCLUDE` | empty | Types never mirrored; see [Excluding](#excluding) |
| `PICKLE_EXCLUDE_NAMESPACES` | empty | Namespaces never mirrored; shell patterns like `tmp-*` |
| `PICKLE_REDISCOVERY_INTERVAL` | `off` | Optional timer for picking up new types; at least `1m` |
| `PICKLE_PAGE_SIZE` | `50` | Objects per LIST page |
| `PICKLE_PAGE_PAUSE` | `200ms` | Pause between pages |
| `PICKLE_QPS` / `PICKLE_BURST` | `5` / `10` | API rate limit |
| `PICKLE_GIT_COMPACT_EVERY` | `256` | Commits between local clone compactions |
| `PICKLE_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `PICKLE_METRICS_ADDR` | `:9090` | `/metrics` and `/healthz` |
| `PICKLE_GIT_SSH_USER` | `git` | |
| `PICKLE_GIT_USERNAME` | `pickle` | HTTPS username |
| `PICKLE_GIT_AUTHOR_NAME` | `pickle` | |
| `PICKLE_GIT_AUTHOR_EMAIL` | `pickle@<cluster>.local` | |
| `PICKLE_REPO_DIR` | `/data/repo` | Local clone. Must be empty or a pickle clone; anything else is refused, never deleted |
| `GOMEMLIMIT` | the memory limit | Set from `limits.memory` by the manifests, so the GC works harder before the container is OOM-killed |

`PICKLE_COMMIT_INTERVAL` from 1.0.0 still works as `PICKLE_COMMIT_DELAY`, with a warning in the log.

In Helm, these map to `cluster`, `color`, `config.*` and `git.*` in [`values.yaml`](../helm/pickle/values.yaml); anything else can go in `extraEnv`.

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

Excluded types are never watched or listed, so they cost nothing. Excluded namespaces are skipped in the backfill, and their events are dropped, including the Namespace object itself. Files mirrored before an exclusion are removed on the next start, in the `initial sync complete` commit.

These come on top of the built-in exclusions in [how it works](how-it-works.md#skipped).

## Git credentials

Credentials never go in the ConfigMap. They're files from the `pickle-git` Secret, mounted at `/etc/pickle/git/`:

| Key | Used for |
|---|---|
| `ssh-privatekey` + `known_hosts` | SSH URLs. Host keys are always verified. |
| `token` | HTTPS URLs. Without it, pickle connects anonymously. |

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
