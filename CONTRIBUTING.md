# Contributing

Thanks for your interest in improving this project.

## Development

```bash
make test     # go test ./...
make vet      # go vet ./...
make build    # local compile
```

The reconcile engine (`internal/sync`) is unit-tested with no AWS or Google dependency —
add a test there for any behavior change to matching, ordering, or the delete/prune gates.

Before opening a PR:

- `gofmt` your code (CI fails on unformatted files).
- `go vet`, `go test -race`, and the linters in [`ci.yml`](.github/workflows/ci.yml) pass.
- Keep changes to the deployment contract (template parameters / env vars) backward
  compatible, or call them out — they are versioned (see [RELEASING.md](RELEASING.md)).

## Pull requests

- Keep PRs focused and describe the behavior change and why.
- Update [README.md](README.md) / [IMPLEMENTATION.md](IMPLEMENTATION.md) when behavior or
  configuration changes, and add a [CHANGELOG.md](CHANGELOG.md) entry under `Unreleased`.

## Reporting security issues

See [SECURITY.md](SECURITY.md) — do not file security reports as public issues.
