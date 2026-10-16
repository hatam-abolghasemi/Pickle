# Security

## Access

- **Read everything, write nothing.** The ClusterRole is `get`, `list`, `watch` on every resource, so new CRDs are covered automatically.
- **No silent gaps.** Types pickle can't read are listed in `<cluster>/_unreadable.txt`.
- **Hardened pod:** non-root, distroless, read-only filesystem, no capabilities, `RuntimeDefault` seccomp, single replica.

## Secrets

Values are replaced by `****` in memory, before rendering, so they never touch disk or Git. Masking fails closed: only `apiVersion`, `kind`, `metadata`, `type`, `immutable` and `data` are written, so a field pickle doesn't know about can never carry a value into Git:

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
- Enable the chart's NetworkPolicy (`networkPolicy.enabled`), which limits the pod to the API server, your Git host and DNS.
- Exclude what you don't need mirrored, e.g. `PICKLE_EXCLUDE: "secrets.core"`. RBAC can't subtract from a wildcard, so pickle can still read them, but it never lists or watches them.

## Also

- ConfigMaps are mirrored as-is, so a password stored in one ends up in Git.
- The repo maps your whole infrastructure. Keep it private.
- Prefer a deploy key scoped to one repo over a personal token.
- Credentials never go in the URL: pickle refuses a `PICKLE_GIT_URL` containing a username, password or token, and refuses to send a token over plain `http://`.

## Supply chain

- Release images are built with SBOM and provenance attestations.
- Every GitHub Action is pinned to a commit SHA; Dependabot keeps the pins current.
- CI runs `govulncheck` on every change.
