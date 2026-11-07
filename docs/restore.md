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

`enc:<fingerprint>:<blob>` ([encrypted values](security.md#encrypted-values)): decrypt them with your private key, one of two ways. Both give exactly the original values or fail: if any value can't be decrypted, nothing is printed and nothing gets applied.

**With `pickle decrypt`**, on any OS, without OpenSSL. With Docker, from the cloned repo:

```bash
docker run --rm -i --user "$(id -u)" -v "$HOME/.config/pickle/pickle.key:/pickle.key:ro" -v "$PWD:/repo:ro" ghcr.io/hatam-abolghasemi/pickle:latest decrypt -key /pickle.key /repo/stg/payments/secret-payments-db.yaml
```

That prints the Secret with real values. Point it at a directory to get every `secret-*.yaml` under it, then apply them:

```bash
docker run --rm -i --user "$(id -u)" -v "$HOME/.config/pickle/pickle.key:/pickle.key:ro" -v "$PWD:/repo:ro" ghcr.io/hatam-abolghasemi/pickle:latest decrypt -key /pickle.key /repo/stg | sudo kubectl apply -f -
```

Without Docker, build it from pickle's source (Go 1.26+) and run it the same way:

```bash
git clone https://github.com/hatam-abolghasemi/pickle /tmp/pickle-src && go -C /tmp/pickle-src/app build -o /tmp/pickle .
```

```bash
/tmp/pickle decrypt -key ~/.config/pickle/pickle.key stg | sudo kubectl apply -f -
```

One value on its own prints its plain bytes:

```bash
/tmp/pickle decrypt -key ~/.config/pickle/pickle.key -value 'enc:3f9a1c7e5b2d:MIIC...'
```

**With OpenSSL 3** (Linux; needs bash, GNU base64 and `openssl` 3.0 or newer). Define this once in your shell:

```bash
pickle_decrypt() { local out="" line key v; while IFS= read -r line || [[ -n $line ]]; do if [[ $line =~ ^(\ +[^:\ ]+):\ [\'\"]?enc:[0-9a-f]+:([A-Za-z0-9+/=]+)[\'\"]?$ ]]; then key=${BASH_REMATCH[1]}; v=$(set -o pipefail; printf %s "${BASH_REMATCH[2]}" | base64 -d | openssl cms -decrypt -inform DER -inkey ~/.config/pickle/pickle.key | base64 -w0) || { echo "pickle_decrypt: $1: ${key//[[:space:]]/}: cannot decrypt, nothing printed" >&2; return 1; }; out+="$key: $v"$'\n'; else out+="$line"$'\n'; fi; done < "$1"; printf %s "$out"; }
```

Look at one Secret decrypted:

```bash
pickle_decrypt stg/payments/secret-payments-db.yaml
```

Apply every Secret of the cluster:

```bash
for f in stg/*/secret-*.yaml; do pickle_decrypt "$f" | sudo kubectl apply -f -; done
```

One value on its own:

```bash
echo '<blob>' | base64 -d | openssl cms -decrypt -inform DER -inkey ~/.config/pickle/pickle.key
```

`wrong key`, or OpenSSL's `Error decrypting CMS structure` (it may print junk after it), means the value was encrypted to another certificate: use the key that goes with it. `damaged or altered` means the blob isn't the one pickle wrote; take it from another commit.

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
