# Comparison

| Tool | Saves | When | Where | Readable | Volume data |
|---|---|---|---|---|---|
| **pickle** | Objects, re-appliable | Every change | Git | ✅ | ❌ |
| [Velero](https://velero.io) | Objects + volumes | Scheduled | Object storage | ❌ | ✅ |
| [Kasten K10](https://www.veeam.com/kasten) | Apps + volumes | Scheduled | Object storage | ❌ | ✅ |
| [GitOps Reverser](https://github.com/ConfigButler/gitops-reverser) | Objects | Every change | Git | ✅ | ❌ |
| [Katafygio](https://github.com/bpineau/katafygio) | Objects | Every change + resync | Git | ✅ | ❌ |
| [kube-backup](https://github.com/pieterlange/kube-backup) | Selected types | CronJob | Git | ✅ | ❌ |
| [kube-dump](https://github.com/woozymasta/kube-dump) | Objects, PVC archives | On demand | Local, Git, S3 | ✅ | ✅ |
| [kubewatch](https://github.com/robusta-dev/kubewatch) | Nothing (notifies) | Every change | Slack, webhooks | — | ❌ |

**Velero** is the standard Kubernetes backup tool: scheduled snapshots of objects and volume data in object storage, with one-command restores. Compared with pickle, it keeps point-in-time archives instead of per-change history, needs write access to restore, stores Secret values, and can't show who hand-edited what. Use Velero for data and disaster recovery, pickle for configuration history. **Kasten K10** is Veeam's commercial counterpart, and the same comparison applies.

**GitOps Reverser** is the closest relative: an operator that writes YAML to Git on every change. It's configured through CRDs and encrypts Secrets with SOPS, so values can be decrypted. Pickle has no CRDs and never stores values at all; it adds a rate-limited backfill, `last-manager` headers, a Secrets checklist and branch sharing.

**Katafygio** has the same core idea, with releases dating from around 2018. **kube-backup** is a CronJob script, unmaintained for years. **kube-dump** is an on-demand exporter, good for one-off migrations. **kubewatch** notifies about changes but keeps no copy.

**Argo CD** shows drift only for objects it manages. Pickle covers everything, and the diff between your GitOps repo and pickle's repo lists your undocumented changes.

**Audit logs** are the only source of the user and source IP behind a change. They're an event stream, not restorable state, so they complement pickle.
