# Security

## Access

- **Read everything, write nothing.** The ClusterRole is `get`, `list`, `watch` on every resource, so new CRDs are covered automatically.
- **No silent gaps.** Types pickle can't read are listed in `<cluster>/_unreadable.txt`.
- **Hardened pod:** non-root, distroless, read-only filesystem, no capabilities, `RuntimeDefault` seccomp, single replica.

## Secrets

Values are masked in memory, before rendering, so they never touch disk or Git. Masking fails closed: only `apiVersion`, `kind`, `metadata`, `type`, `immutable` and `data` are written, so a field pickle doesn't know about can never carry a value into Git.

With a fingerprint key (`fingerprint-key` in the `pickle-git` Secret), each value becomes `****:` plus the first 12 hex digits of its HMAC-SHA256 under that key:

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
  DB_PASSWORD: '****:3f9a1c7e5b2d'
  DB_USER: '****:a07c41d9e2f8'
```

A changed value shows up as a diff line under its own key, so you see exactly which key changed, and a Secret whose values didn't change is never rewritten. The fingerprint reveals nothing about a value without the key. With the key, someone holding the repo could test guesses against it, which is why it lives in the cluster next to the Git credentials, never in the repo, and must be at least 32 characters. The same value always gets the same fingerprint, so a value reused across Secrets is visible as such. Keep the key stable: a new key rewrites every Secret file once.

```bash
sudo kubectl -n pickle patch secret pickle-git --type merge -p "{\"stringData\":{\"fingerprint-key\":\"$(openssl rand -hex 32)\"}}"
sudo kubectl -n pickle rollout restart deploy/pickle
```

Without a key, every value is `****`, a changed value only shows up as a new header, and added, removed or renamed keys show up as diff lines. ServiceAccount token Secrets are skipped.

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
