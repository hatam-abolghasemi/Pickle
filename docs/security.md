# Security

## Access

- **Read everything, write nothing.** The ClusterRole is `get`, `list`, `watch` on every resource, so new CRDs are covered automatically.
- **No silent gaps.** Types pickle can't read are listed in `<cluster>/_unreadable.txt`.
- **Hardened pod:** non-root, distroless, read-only filesystem, no capabilities, `RuntimeDefault` seccomp, single replica.

## Secrets

Values are replaced by `****` in memory, before rendering, so they never touch disk or Git:

```yaml
# last-manager: kubectl-edit (Update)
# last-changed: 2026-10-14T17:52:03Z
apiVersion: v1
kind: Secret
metadata:
  name: payments-db
  namespace: payments
type: Opaque
data:
  DB_PASSWORD: '****'
  DB_USER: '****'
```

A changed value shows up as a new header; added, removed or renamed keys show up as diff lines. ServiceAccount token Secrets are skipped.

Each namespace also gets `_secrets-referenced.txt`, built from what workloads declare (`secretKeyRef`, `envFrom`, volumes, `imagePullSecrets`, Ingress `tls`, ServiceAccount secrets):

```yaml
payments-db:
  keys:
  - DB_PASSWORD
  - DB_USER
  used-by:
  - cronjob/payments-reconcile
  - deployment/payments-api
```

## The trade-off

Pickle's ServiceAccount can read Secret values from the API, even though pickle never writes them anywhere. Anyone able to run code in the pod could use that. To narrow it:

- Allow only cluster admins to `exec` into or modify the `pickle` namespace.
- Add a network policy limiting the pod to the API server and your Git host.

## Also

- ConfigMaps are mirrored as-is, so a password stored in one ends up in Git.
- The repo maps your whole infrastructure. Keep it private.
- Prefer a deploy key scoped to one repo over a personal token.
