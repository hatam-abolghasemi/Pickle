# Changelog

Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versioning: [SemVer](https://semver.org/).

## [Unreleased]

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

[Unreleased]: https://github.com/hatam-abolghasemi/pickle/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/hatam-abolghasemi/pickle/releases/tag/v1.0.0
