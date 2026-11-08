# Restore

Pickle restores configuration, not data: volumes and databases come from your other backups, and so do Secret values unless [encrypted values](security.md#encrypted-values) are on. Point `kubectl` at the **new** cluster, then run these from the cloned repo (cluster `stg` here).

**1. CRDs**, since custom resources need their definitions first:

```bash
for f in stg/_cluster/customresourcedefinition-*.yaml; do sudo kubectl apply --server-side -f "$f"; done
```

```bash
sudo kubectl wait --for condition=established --timeout=120s crd --all
```

**2. Namespaces:**

```bash
for f in stg/_cluster/namespace-*.yaml; do sudo kubectl apply -f "$f"; done
```

**3. Secrets.** Each namespace's `_secrets-referenced.txt` lists the Secrets and keys its workloads need:

```bash
cat stg/*/_secrets-referenced.txt
```

The `secret-*.yaml` files keep each Secret's type, labels and keys. What to do next depends on their values.

`****` or `****:<fingerprint>`: fill in real values from your vault before applying them.

`enc:<fingerprint>:<blob>` ([encrypted values](security.md#encrypted-values)): decrypt them with `pickle decrypt` and your private key. Make a shortcut once; it runs pickle's image, so you need only Docker and the key:

```bash
echo 'alias pickle-decrypt='"'"'docker run --rm --user "$(id -u):$(id -g)" -v "$HOME/.config/pickle/pickle.key:/pickle.key:ro" -v "$PWD:/repo" -w /repo ghcr.io/hatam-abolghasemi/pickle:latest decrypt'"'"'' >> ~/.bashrc && source ~/.bashrc
```

Then, from the root of your jar clone, decrypt a whole cluster, one namespace or one Secret:

```bash
pickle-decrypt stg
```

```bash
pickle-decrypt stg/payments
```

```bash
pickle-decrypt stg/payments/secret-payments-db.yaml
```

Next to every `secret-<name>.yaml` it writes two files, and both apply with `kubectl`:

- `secret-<name>.base64.yaml`: the Secret with its values under `data`, in base64, as the API keeps them.
- `secret-<name>.raw.yaml`: the same Secret with plain, readable values under `stringData`. A value that isn't text, like a keystore, stays under `data` in base64.

If any value fails to decrypt, nothing is written. The files are kept out of git through your clone's `.git/info/exclude`, which is local and never pushed. Secrets written before encryption was on (`****`) are skipped with a note.

Apply one Secret, or every Secret of the cluster:

```bash
sudo kubectl apply -f stg/payments/secret-payments-db.base64.yaml
```

```bash
find stg -name 'secret-*.base64.yaml' -exec sudo kubectl apply -f {} \;
```

Remove the decrypted files when you're done:

```bash
pickle-decrypt -clean stg
```

`wrong key` means the values were encrypted to another certificate: use the key that goes with it, such as the old key for commits from before a new certificate. `damaged or altered` means a blob isn't what pickle wrote; take the file from another commit.

Without Docker, plain OpenSSL 3 opens one value:

```bash
echo '<blob>' | base64 -d | openssl cms -decrypt -inform DER -inkey ~/.config/pickle/pickle.key
```

**4. Everything else**, dry run first:

```bash
sudo kubectl apply -R -f stg/ --dry-run=server
```

```bash
sudo kubectl apply -R -f stg/
```

Helper `.txt` files are ignored. Unfilled `secret-*.yaml` files fail with a base64 error, which is expected; everything else still applies.

## Notes

- Some objects only fit the old cluster: `node-*.yaml`, PersistentVolumes bound to old disks, `apiservice-*.yaml` for built-in groups, and webhook configs with an injected `caBundle`. Skim `stg/_cluster/` first.
- If Argo CD manages most workloads, restore Argo and let it sync, then use pickle's files for whatever Argo didn't know about.
- To list hand-applied changes:

  ```bash
  grep -rl 'last-manager: kubectl-' stg/
  ```
