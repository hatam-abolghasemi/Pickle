# Configuration

Settings are environment variables from one ConfigMap: [`k8s/configmap.yaml`](../k8s/configmap.yaml), or rendered by Helm from [`values.yaml`](../helm/pickle/values.yaml). With plain manifests, restart after changing it:

```bash
sudo kubectl -n pickle rollout restart deploy/pickle
```

| Variable | Default | |
|---|---|---|
| `PICKLE_CLUSTER` | required | Root directory in the repo (`a-z 0-9 -`) |
| `PICKLE_GIT_URL` | required | `git@host:path`, `ssh://...` or `https://...` |
| `PICKLE_GIT_BRANCH` | `main` | Created on first push |
| `PICKLE_COMMIT_INTERVAL` | `13s` | |
| `PICKLE_PAGE_SIZE` | `50` | Objects per LIST page |
| `PICKLE_PAGE_PAUSE` | `200ms` | Pause between pages |
| `PICKLE_QPS` / `PICKLE_BURST` | `5` / `10` | API rate limit |
| `PICKLE_REDISCOVERY_INTERVAL` | `10m` | How often new types are picked up |
| `PICKLE_METRICS_ADDR` | `:9090` | `/metrics` and `/healthz` |
| `PICKLE_GIT_SSH_USER` | `git` | |
| `PICKLE_GIT_USERNAME` | `pickle` | HTTPS username |
| `PICKLE_GIT_AUTHOR_NAME` | `pickle` | |
| `PICKLE_GIT_AUTHOR_EMAIL` | `pickle@<cluster>.local` | |
| `PICKLE_REPO_DIR` | `/data/repo` | Local clone |

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
