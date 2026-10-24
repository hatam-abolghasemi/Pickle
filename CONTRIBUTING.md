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

Tag `vX.Y.Z` on `main`. CI publishes the image to `ghcr.io/hatam-abolghasemi/pickle` and the chart to `oci://ghcr.io/hatam-abolghasemi/charts/pickle`.
