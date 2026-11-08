# Security

## Access

- **Read everything, write nothing.** The ClusterRole is `get`, `list`, `watch` on every resource, so new CRDs are covered automatically.
- **No silent gaps.** Types pickle can't read are listed in `<cluster>/_unreadable.txt`.
- **Hardened pod:** non-root, distroless, read-only filesystem, no capabilities, `RuntimeDefault` seccomp, single replica.

## Secrets

Values are masked in memory, before rendering, so they never touch disk or Git, unless you turn on [encrypted values](#encrypted-values). Masking fails closed: only `apiVersion`, `kind`, `metadata`, `type`, `immutable` and `data` are written, so a field pickle doesn't know about can never carry a value into Git.

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
```

```bash
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

### Encrypted values

Optional. For incident day: Secret values you can decrypt from the repo alone, on any machine, even when the cluster and pickle are gone. It's a shortcut for recovery, not a secret store; Vault, Sealed Secrets or whatever you use stays the source of truth.

Each value is written as `enc:<fingerprint>:<blob>`:

```yaml
data:
  DB_PASSWORD: enc:3f9a1c7e5b2d:MIIC1QYJKoZIhvcNAQkQARegggLEMIICwAIBADGCAl0w...
  DB_USER: enc:a07c41d9e2f8:MIICtQYJKoZIhvcNAQkQARegggKkMIICoAIBADGCAl0w...
```

The blob is a standard CMS AuthEnvelopedData: AES-256-GCM encrypts the value, RSA-OAEP (SHA-256) encrypts that AES key to your certificate. GCM authenticates the value, so decrypting gives exactly the original bytes or fails with an error; a damaged or altered blob never decrypts to something wrong. Pickle gets only your certificate: it can encrypt, never decrypt. Diffs stay per key: an unchanged value keeps its line.

`pickle decrypt` opens them, for a whole cluster, a namespace or one Secret: next to each Secret it writes a base64 and a plain-text copy, both ready for `kubectl apply`, or nothing at all if any value fails. Plain OpenSSL 3 opens single values too. See [restore](restore.md).

**1. Make sure pickle has a fingerprint key.** Encryption needs one; pickle refuses to start without it. This prints nothing if it's missing:

```bash
sudo kubectl -n pickle get secret pickle-git -o jsonpath='{.data.fingerprint-key}'
```

If it's missing, add it as [above](#secrets).

**2. Create your key pair**, once, on your machine (OpenSSL 3). The certificate never expires, and pickle ignores its dates anyway.

```bash
mkdir -p ~/.config/pickle && chmod 700 ~/.config/pickle && cd "$(mktemp -d)"
```

```bash
if [ -e ~/.config/pickle/pickle.key ]; then echo "pickle.key exists: keeping it"; else openssl genrsa -out ~/.config/pickle/pickle.key 4096 && chmod 600 ~/.config/pickle/pickle.key; fi
```

```bash
mkdir ca && touch ca/index.txt && echo 01 > ca/serial && printf '[ca]\ndefault_ca=d\n[d]\ndatabase=ca/index.txt\nnew_certs_dir=ca\nserial=ca/serial\ndefault_md=sha256\npolicy=p\n[p]\ncommonName=supplied\n' > ca.cnf
```

```bash
openssl req -new -key ~/.config/pickle/pickle.key -subj "/CN=pickle" -out pickle.csr
```

```bash
openssl ca -batch -config ca.cnf -selfsign -keyfile ~/.config/pickle/pickle.key -in pickle.csr -enddate 99991231235959Z -notext -out ~/.config/pickle/pickle.crt
```

Check that the key decrypts what the certificate encrypts; this prints `ok`:

```bash
echo -n ok | openssl cms -encrypt -aes-256-gcm -binary -outform DER -recip ~/.config/pickle/pickle.crt | openssl cms -decrypt -inform DER -inkey ~/.config/pickle/pickle.key
```

`pickle.key` is the only thing that decrypts your Secrets. It never goes to the cluster or the repo. Keep a backup somewhere safe (a password manager, a vault); without it the values can't be decrypted, by anyone. Losing it only loses this shortcut.

**3. Give pickle the certificate** and restart it:

```bash
sudo kubectl -n pickle patch secret pickle-git -p "{\"data\":{\"encryption-cert\":\"$(base64 -w0 ~/.config/pickle/pickle.crt)\"}}"
```

```bash
sudo kubectl -n pickle rollout restart deploy/pickle
```

With Helm, add `--set-file secrets.encryptionCert=$HOME/.config/pickle/pickle.crt` to your `helm upgrade` instead, or put `encryption-cert` in your `git.existingSecret`.

**4. Check it's on.** The startup line says `secretEncryption=true`:

```bash
sudo kubectl -n pickle logs deploy/pickle | grep "pickle starting"
```

Every Secret file is rewritten once with `enc:` values on the next push.

**5. Test a restore now**, not on incident day. Pull the repo and decrypt one namespace with your key, as in [restore](restore.md). If its `.raw.yaml` files show your real values, you're set.

**Turning it off:** remove `encryption-cert`, then restart. New values go back to `****:<fingerprint>`.

```bash
sudo kubectl -n pickle patch secret pickle-git --type json -p '[{"op":"remove","path":"/data/encryption-cert"}]'
```

```bash
sudo kubectl -n pickle rollout restart deploy/pickle
```

**A new certificate** (steps 2 and 3 again) re-encrypts every value to it.

Good to know:

- A blob opens only with the key whose certificate it was encrypted to. After a new certificate, keep the old key as long as you may need the old commits; `pickle decrypt` says `wrong key` when you use another one.
- Git keeps every blob ever pushed. Whoever has the key and the repo can decrypt all of them; a new certificate or turning encryption off only changes new commits.
- There is one certificate, so one key. Share `pickle.key` only with whoever should be able to restore Secrets.
- A value that can't be encrypted is written as `****:<fingerprint>`, never in plain, and counted in `pickle_secret_encryption_failures_total`.

## Keep these files safe

| File | What it is | Where to keep it | If you lose it |
|---|---|---|---|
| `pickle.key` | Your private key: the only thing that decrypts [encrypted Secret values](#encrypted-values) | `~/.config/pickle/`, plus a backup in a password manager or vault. Never in the cluster or a repo. | Nobody can decrypt the values in Git anymore. Make a new key pair; values are encrypted again from then on. |
| `pickle.crt` | Your certificate, given to pickle to encrypt with | `~/.config/pickle/`. It isn't secret. | Run step 2 of [encrypted values](#encrypted-values) again: it keeps your `pickle.key` and makes a new certificate for it. |
| `fingerprint_key` (the `fingerprint-key` value) | Keys the Secret fingerprints | With the chart managing the Secret: next to your deploy key, outside any repo. With `git.existingSecret`: in the cluster's `pickle-git` Secret, and in your [backup](#back-up-and-restore). | A new one rewrites every Secret file once. Nothing else breaks. |
| `pickle_deploy_key` | Pickle's SSH key, with write access to the jar repo | Outside any repo; after installing, only the `pickle-git` Secret needs it. | Make a new one and replace it in your Git host and the `pickle-git` Secret. |
| `known_hosts` | Your Git host's SSH host keys | Anywhere. It isn't secret. | Run `ssh-keyscan` again. |
| Your values file | Pickle's settings for one cluster | Commit it wherever you keep deployment config. It holds no secrets. | Write it again from [`values-sample.yaml`](../helm/values-sample.yaml). |

The `*.base64.yaml` and `*.raw.yaml` files `pickle decrypt` writes hold plain Secret values. They are for the moment you need them: delete them with `pickle decrypt -clean` when you're done.

## Back up and restore

One zip holds everything to restore pickle: your key pair, the deploy key, `known_hosts` and the cluster's `pickle-git` Secret, which includes its fingerprint key. Anyone who can open it can decrypt every Secret in your jar repo, so keep it somewhere private, such as a password manager, a vault or a private cloud folder.

**Back up.** Collect the files in RAM (`/dev/shm`), where they never touch disk. Adjust the deploy key and `known_hosts` paths to where you keep them:

```bash
B=/dev/shm/pickle-backup && rm -rf $B && mkdir -m 700 $B && cp ~/.config/pickle/pickle.key ~/.config/pickle/pickle.crt ./pickle_deploy_key ./known_hosts $B/
```

Add the cluster's Secret and its fingerprint key:

```bash
sudo kubectl -n pickle get secret pickle-git -o yaml > $B/pickle-git.yaml && sudo kubectl -n pickle get secret pickle-git -o jsonpath='{.data.fingerprint-key}' | base64 -d > $B/fingerprint_key
```

Zip it, then remove the copy in RAM:

```bash
cd /dev/shm && zip -r ~/Documents/pickle-backup-$(date +%F).zip pickle-backup && cd - > /dev/null && rm -rf /dev/shm/pickle-backup
```

Check it, then move `~/Documents/pickle-backup-<date>.zip` to its safe place:

```bash
unzip -l ~/Documents/pickle-backup-$(date +%F).zip
```

**Restore.** Unzip it in RAM:

```bash
unzip -q ~/Downloads/pickle-backup-*.zip -d /dev/shm && cd /dev/shm/pickle-backup && ls
```

On a new laptop, put the key pair back, so `pickle decrypt` works again:

```bash
mkdir -p ~/.config/pickle && chmod 700 ~/.config/pickle && cp pickle.key pickle.crt ~/.config/pickle/ && chmod 600 ~/.config/pickle/pickle.key
```

On a new or rebuilt cluster, recreate the `pickle-git` Secret from the files. `pickle-git.yaml` is there to read; it carries the old cluster's IDs, so don't apply it as it is:

```bash
sudo kubectl create namespace pickle
```

```bash
sudo kubectl -n pickle create secret generic pickle-git --from-file=ssh-privatekey=pickle_deploy_key --from-file=known_hosts=known_hosts --from-file=fingerprint-key=fingerprint_key --from-file=encryption-cert=pickle.crt
```

Then install pickle with `git.existingSecret: pickle-git` in your values file, as in the [README](../README.md#upgrade). To bring the cluster's Secrets back from the jar repo, see [restore](restore.md).

Remove the unzipped copy when you're done:

```bash
rm -rf /dev/shm/pickle-backup
```

## The trade-off

Pickle's ServiceAccount can read Secret values from the API, even though pickle never writes them anywhere unencrypted. Anyone able to run code in the pod could use that. To narrow it:

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
