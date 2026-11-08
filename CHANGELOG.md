# Changelog

Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versioning: [SemVer](https://semver.org/).

## [Unreleased]

## [1.2.1] - 2026-11-08

### Added

- An opt-in ServiceMonitor in the Helm chart, for Prometheus Operator: `serviceMonitor.enabled: true`, with `serviceMonitor.interval` and `serviceMonitor.labels`.

### Fixed

- The Grafana dashboard failed to import in Grafana 13: its variables had no `current` value.

## [1.2.0] - 2026-11-08

### Changed

- `pickle decrypt` is one command for a whole jar, a cluster, a namespace or one Secret: `pickle decrypt stg/payments`. Next to every `secret-<name>.yaml` it writes `secret-<name>.base64.yaml` (values under `data`) and `secret-<name>.raw.yaml` (plain values under `stringData`), both ready for `kubectl apply`, or nothing at all if any value fails. It finds the key at `~/.config/pickle/pickle.key` (or `/pickle.key` in Docker) and keeps its files out of git through the clone's `.git/info/exclude`. `pickle decrypt -clean` removes them. This replaces printing to stdout and `-value`, and the OpenSSL shell function in the docs. See [docs/restore.md](docs/restore.md).

### Fixed

- An empty Secret value decrypted to `null` instead of `""`.

## [1.1.0] - 2026-11-07

### Added

- Encrypted Secret values, opt-in, so Secrets can be restored from the repo alone. Put your certificate in the `pickle-git` Secret as `encryption-cert` (Helm: `secrets.encryptionCert`) and each value is written as `enc:<fingerprint>:<blob>`: CMS AuthEnvelopedData with AES-256-GCM and RSA-OAEP. Pickle only has the certificate, so it can never decrypt. Decrypting gives exactly the original value or an error, never wrong bytes. Needs `fingerprint-key`. Diffs stay per key, and an unchanged Secret costs no more CPU or memory than before. Turning it on or changing the certificate rewrites every Secret file once. See [docs/security.md](docs/security.md#encrypted-values).
- `pickle decrypt -key pickle.key <file or dir>`: prints Secret files with their values decrypted, ready for `kubectl apply`. No OpenSSL, cluster or config needed; runs from the image. Prints nothing if any value fails. Plain OpenSSL 3 works too. See [docs/restore.md](docs/restore.md).
- A Grafana dashboard, [`dashboards/pickle.json`](dashboards/pickle.json).
- Metrics `pickle_build_info`, `pickle_start_time_seconds`, `pickle_secret_encryption_enabled`, `pickle_secret_values_encrypted_total` and `pickle_secret_encryption_failures_total`.

### Changed

- Files read like hand-written manifests: `apiVersion`, `kind`, `metadata`, then the body, `name` first in every list item, lists indented under their key, and no fields that only repeat the API server's defaults (`dnsPolicy: ClusterFirst`, probe thresholds, `protocol: TCP`...). A value that differs from the default always stays, so applying a file gives back the same object. Writing a file also costs about 25% less CPU and 40% less memory than before. After upgrading, every mirrored file is rewritten once, in one commit per cluster. See [docs/how-it-works.md](docs/how-it-works.md#normalization).
- The plain manifests in `k8s/` use the same layout.

## [1.0.6] - 2026-11-01

### Changed

Git work no longer grows with the repo or with the number of clusters sharing the branch. In a test repo of four clusters and 52,000 files (a 31 MiB pack), a sync cycle of a 3-file change went from 6.5 s, 350 MiB allocated and a 140-195 MiB heap peak to 60 ms, 2.3 MiB allocated and a 7 MiB peak; the local store stays at 5 MiB instead of growing 31 MiB per cycle.

- Fetches are incremental. Each fetch asked for the branch head at depth 1, and without thin packs, which go-git can't take, the server answered with the whole tree of the head every time: every cluster's directory, downloaded, parsed and indexed on each cycle. Each one also left a pack whose index stayed in memory until the next compaction, so the heap grew with every cycle, up to `GOMEMLIMIT`. Pickle now tells the server which commits it has and receives only what was committed since. If the branch's history was rewritten and the server shares no commit with the local clone, the head is downloaded afresh, never the rewritten history.
- Pushes send only the objects along the changed paths. go-git's push walked the entire tree of the remote head, and of every shallow commit since the last compaction, to work out what the server already had.
- The local store is sparse: it keeps the head commit, its root tree and this cluster's directory, nothing of other clusters' directories. The first clone streams the pack to a file and indexes it in a few MiB instead of through go-git's parser, which held up to 96 MiB of delta bases: the startup peak went from 144 MiB to 21 MiB of heap in the test repo. Only this cluster's files are written to the working tree.
- Compaction copies what pickle reads into a new local store instead of cloning the branch again: no network, and 14 MiB instead of 232 MiB at peak.
- CustomResourceDefinitions are listed 5 per page whatever `PICKLE_PAGE_SIZE` says: a CRD carries its whole OpenAPI schema, often hundreds of KiB, and a page is decoded whole into memory.
- Default resources are now requests 48Mi, limits 128Mi (from 64Mi and 512Mi).

## [1.0.5] - 2026-10-30

### Added

- Each cluster's directory is claimed in `<cluster>/_cluster-id.txt`, with the UID of the cluster's `kube-system` namespace. A pickle whose `PICKLE_CLUSTER` names a directory another cluster already claimed refuses to start, and one that sees another cluster claim its directory while running stops without pushing. Before, two clusters given the same name mixed their objects in one directory, and each restart deleted the other cluster's files as stale and re-added its own. On the first start the directory is claimed by whichever cluster pushes first.

### Changed

- The backfill lists each type once across all namespaces, instead of once per namespace: a few requests per type rather than one per type and namespace, most of them empty. A restart's backfill took 45 to 50 minutes on busy clusters, most of it spent on per-namespace lists, with every commit in that time labelled `backfill:`. A type whose list outlives etcd's compaction window is listed namespace by namespace instead.
- `pickle_backfill_namespaces_total` and `pickle_backfill_namespaces_done` are replaced by `pickle_backfill_types_total` and `pickle_backfill_types_done`. The `backfill progress` log line names a type instead of a namespace.

### Fixed

- `_secrets-referenced.txt` files are no longer committed half-written on every start. The index was rewritten from what the backfill had seen so far, so a namespace that changed before the backfill reached it got an index listing only that change, then the full one again minutes later. Index files are now written once the backfill is complete.

## [1.0.4] - 2026-10-30

### Added

- `PICKLE_METADATA_WATCH` (Helm: `config.metadataWatch`), default `core/pods,apps/replicasets,batch/jobs`: types watched and listed as metadata only. An object with an owner, nearly every Pod, ReplicaSet and Job, is skipped from its metadata alone; one without is read whole with a `GET` and mirrored as before. Empty: every type is read whole, as in 1.0.3.
- Profiling builds: built with `-tags pprof`, pickle serves `/debug/pprof/` on `DEBUG_PPROF_ADDR`. Release images don't have the tag.

### Changed

Less CPU and memory, with the same files and commits: in a lab cluster of 6,600 objects in 64 namespaces, pickle 1.0.3 and this version produced identical repos.

- Watch events cost half the CPU. Pod status changes, 70% of the events in the lab, no longer decode the whole Pod (see `PICKLE_METADATA_WATCH`); an object whose mirrored body didn't change (a status update, a heartbeat) is no longer rendered to YAML to find that out; the last manager is read from `managedFields` directly instead of converting every entry. 30 events per second took 1.55 CPU-seconds a minute with 1.0.3, 0.76 now.
- Half the binary. Types are discovered with pickle's own client (aggregated discovery: two requests) instead of client-go's, which brought the typed scheme of every built-in API: the image's binary is 14.7 MB instead of 30 MB, and the code mapped in memory about 8 MiB less.
- Page cache handed back. Every file pickle writes, the YAML and git's objects, was charged to its pod as page cache, and once read back by a commit or push counted in its working set, growing with the cluster. Their pages are now dropped about 40 seconds after a push, once written to disk, and right after the repo is read at startup. In the lab the pod's working set a minute after the initial sync was 49.6 MiB instead of 77.4 MiB.
- Pushes send objects without searching for deltas (`pack.window = 0`): the search loaded every pushed object and the trees of every directory a commit touched into memory. Peak memory during the initial sync went from 35 MiB to 24 MiB. Packs are slightly larger; the git server repacks them.
- The git object cache is capped at 2 MiB instead of 8 MiB. Idle heap went from 9.4 MiB to 5.9 MiB.

## [1.0.3] - 2026-10-28

### Added

- Secret value fingerprints: with a `fingerprint-key` in the `pickle-git` Secret (Helm: `secrets.fingerprintKey`), each value is written as `****:<fingerprint>`, a keyed HMAC-SHA256, so a changed value shows up as a diff line under its own key without revealing it. Secrets are then only rewritten when a value, key or label changes, not on a header-only change. Enabling it, or changing the key, rewrites every Secret file once.

### Fixed

- No more header-only commits. An object that changed and changed back before the next commit, such as an Argo CD Application with a sync or refresh request that the controller clears seconds later, was committed with only `last-changed` moved. Each commit now holds net changes against the commit before it, and such files are put back to their committed bytes. Commit messages also report `added` / `modified` against that commit.
- The `argocd.argoproj.io/refresh` annotation and the `operation` field of Argo CD Applications are no longer mirrored. Both are one-shot requests the controller clears, and re-applying them on restore would trigger a refresh or a sync.

## [1.0.2] - 2026-10-25

### Added

- `PICKLE_COLOR` (Helm: `color`): a colored marker in front of every commit subject, e.g. `🟢 [stg] 3 added`, so commits from different clusters are easy to tell apart.
- `pickle_push_conflicts_total`: pushes that found another cluster had pushed to the branch first.

### Changed

- The release workflow only starts on pushes that touch `CHANGELOG.md`, and checks for an untagged version before running CI and building anything. Changes under `[Unreleased]` are never released on their own.

### Fixed

- Clusters sharing a branch no longer fail with `object not found` or `incorrect old value provided`. When another cluster pushes first, this cluster's directory is put on top of the new head and pushed again within seconds, instead of waiting for the next retry with backoff.
- Commits are built from only the changed paths instead of through the git index, so their cost no longer grows with the size of the repo: 900 changes in a 40,000-file repo take well under a second instead of minutes. Watches and backfill are no longer held up while a large commit is being built.
- Objects whose controller writes status through the main resource, such as Argo CD Applications, are no longer rewritten on every reconcile. A file is only rewritten when its YAML body changes; `last-manager` and `last-changed` then show that change. Secrets still follow the header, since their values are masked.

## [1.0.1] - 2026-10-16

### Added

- `PICKLE_EXCLUDE` (by kind, kind and group, or whole group) and `PICKLE_EXCLUDE_NAMESPACES` (with patterns). Files mirrored before an exclusion are removed on the next start.
- `PICKLE_COMMIT_DELAY`, `PICKLE_COMMIT_MAX_DELAY`, `PICKLE_GIT_COMPACT_EVERY` and `PICKLE_LOG_LEVEL`.
- Go runtime memory metrics: `pickle_go_heap_live_bytes`, `pickle_go_memory_mapped_bytes`, `pickle_go_memory_released_bytes`, `pickle_go_goroutines`.
- Optional NetworkPolicy in the Helm chart; `extraEnv` value.
- `govulncheck` in CI; SBOM and provenance on release images.

### Changed

- Commits are event-driven: nothing runs while the cluster is quiet, and bursts of changes become one commit.
- New types are picked up from CustomResourceDefinition and APIService events instead of a 10-minute timer. `PICKLE_REDISCOVERY_INTERVAL` is now off by default.
- Watches reconnect every 30–60 minutes instead of every 5–10.
- The local clone is shallow and compacts itself, so history is never downloaded and the `emptyDir` no longer grows over time.
- Freed memory is returned to the OS after backfill, relists and pushes; the git object cache is capped at 8 MiB; `GOMEMLIMIT` follows the memory limit.
- Go 1.27, client-go 0.37.1, go-git 5.19.3, golang.org/x/crypto 0.56.0, golang.org/x/net 0.60.0, distroless Debian 13 base image.
- GitHub Actions pinned to commit SHAs, with least-privilege permissions per job.

### Deprecated

- `PICKLE_COMMIT_INTERVAL`: still honored as `PICKLE_COMMIT_DELAY`, with a warning.

### Security

- Secret masking fails closed: unknown Secret fields are dropped instead of written.
- Git URLs with embedded credentials are rejected, and tokens are never sent over plain `http://`.
- A non-empty `PICKLE_REPO_DIR` that isn't a pickle clone is refused instead of deleted.
- The metrics server only answers GET, with read, write and idle timeouts.

## [1.0.0] - 2026-10-14

### Added

- Live mirroring of every Kubernetes object to Git as normalized, re-appliable YAML.
- Rate-limited, paginated backfill of existing objects.
- `last-manager`, `last-changed` and `argocd-app` header comments.
- Deletions mirrored, including those made while pickle was down.
- Read-only RBAC on all resources, with `_unreadable.txt` listing anything unreadable.
- Secrets mirrored with every value as `****`, plus a per-namespace `_secrets-referenced.txt`.
- Stable file names across kind collisions, recorded in `_kinds.txt`.
- Several clusters per branch.
- Prometheus metrics and a health endpoint.
- Plain manifests, a Helm chart, and docs.

[Unreleased]: https://github.com/hatam-abolghasemi/pickle/compare/v1.2.1...HEAD
[1.2.1]: https://github.com/hatam-abolghasemi/pickle/compare/v1.2.0...v1.2.1
[1.2.0]: https://github.com/hatam-abolghasemi/pickle/compare/v1.1.0...v1.2.0
[1.1.0]: https://github.com/hatam-abolghasemi/pickle/compare/v1.0.6...v1.1.0
[1.0.6]: https://github.com/hatam-abolghasemi/pickle/compare/v1.0.5...v1.0.6
[1.0.5]: https://github.com/hatam-abolghasemi/pickle/compare/v1.0.4...v1.0.5
[1.0.4]: https://github.com/hatam-abolghasemi/pickle/compare/v1.0.3...v1.0.4
[1.0.3]: https://github.com/hatam-abolghasemi/pickle/compare/v1.0.2...v1.0.3
[1.0.2]: https://github.com/hatam-abolghasemi/pickle/compare/v1.0.1...v1.0.2
[1.0.1]: https://github.com/hatam-abolghasemi/pickle/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/hatam-abolghasemi/pickle/releases/tag/v1.0.0
