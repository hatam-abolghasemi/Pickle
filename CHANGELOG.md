# Changelog

Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versioning: [SemVer](https://semver.org/).

## [Unreleased]

## [1.0.2] - 2026-10-24

### Added

- `PICKLE_COLOR` (Helm: `color`): a colored marker in front of every commit subject, e.g. `🟢 [stg] 3 added`, so commits from different clusters are easy to tell apart.

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

[Unreleased]: https://github.com/hatam-abolghasemi/pickle/compare/v1.0.2...HEAD
[1.0.2]: https://github.com/hatam-abolghasemi/pickle/compare/v1.0.1...v1.0.2
[1.0.1]: https://github.com/hatam-abolghasemi/pickle/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/hatam-abolghasemi/pickle/releases/tag/v1.0.0
