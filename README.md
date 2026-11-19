# liman

A small PaaS for a single server, written in Go. It automates the deploy steps
usually done by hand over SSH: pull an image, start a container with limits,
health check it, write the Nginx config, reload.

This is a learning project. See [ROADMAP.md](ROADMAP.md) for the plan and
current status.

## Requirements

- Go 1.27+
- Docker (from stage 2)
- [golangci-lint](https://golangci-lint.run/) v2 for `make lint`

## Development

```
make build   # build bin/liman with version info
make test    # run tests
make lint    # run linters
make check   # lint + test
```

```
$ ./bin/liman version
liman 3f2a1c9 (commit 3f2a1c9, built 2026-11-19T17:00:00Z)
```

## Layout

```
cmd/liman/           CLI entry point (wiring only)
internal/cli/        command dispatch
internal/buildinfo/  version metadata injected at build time
```
