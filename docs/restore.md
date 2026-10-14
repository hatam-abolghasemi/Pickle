# Restore

Pickle restores configuration, not data: volumes, databases and Secret values come from your other backups. Point `kubectl` at the **new** cluster, then run these from the cloned repo (cluster `stg` here).

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

The `secret-*.yaml` files keep each Secret's type, labels and keys, with every value as `****`. Fill in real values from your vault before applying them.

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
