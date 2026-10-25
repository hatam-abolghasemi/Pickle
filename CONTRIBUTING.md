# Contributing

Thanks for helping. Open an issue before large changes, so we can agree on the approach first.

## Develop

```bash
go -C app test -race ./...
```

```bash
helm lint helm/pickle -f helm/values-sample.yaml
```

## Pull requests

- One topic per PR, with tests for behavior changes.
- Run `gofmt`; CI checks it.
- Add a line to `CHANGELOG.md` under `[Unreleased]`.
- Use [Conventional Commits](https://www.conventionalcommits.org/) for commit messages (`feat:`, `fix:`, `docs:`...).

## Releases

`CHANGELOG.md` is the only place a version is written. Changes collect under `## [Unreleased]` across as many pushes as you like; nothing is released until you cut a version. To release:

1. Rename `## [Unreleased]` to `## [X.Y.Z] - YYYY-MM-DD` and add a new empty `## [Unreleased]` above it.
2. Update the compare links at the bottom of the changelog.
3. Merge to `main`.

The release workflow only starts on pushes to `main` that touch `CHANGELOG.md`. It takes the newest `## [X.Y.Z] - YYYY-MM-DD` heading; if `vX.Y.Z` is already tagged it stops there. Otherwise it runs CI and, if that passes, publishes:

- the image `ghcr.io/hatam-abolghasemi/pickle` as `X.Y.Z`, `X.Y` and `latest`
- the chart `oci://ghcr.io/hatam-abolghasemi/charts/pickle` with version and appVersion `X.Y.Z`
- the GitHub release `vX.Y.Z`, with that changelog section as its notes

Pushes without a new version heading release nothing. Don't put versions in `Chart.yaml`, manifests or docs; they are placeholders filled at release time.
