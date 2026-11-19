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

```
$ ./bin/liman plan examples/app.yaml
Plan for orders-api (examples/app.yaml):
  1. Pull image ghcr.io/user/orders-api:1.4
  2. Allocate a free host port
  3. Start container orders-api on port 3000 (memory 512m, cpus 0.5, 2 env vars)
  4. Health check: GET /health on port 3000
  5. Write Nginx config for api.example.com
  6. Reload Nginx
```

See [examples/app.yaml](examples/app.yaml) for the app definition format.

## Layout

```
cmd/liman/           CLI entry point (wiring only)
internal/cli/        command dispatch
internal/app/        app definition: loading and validation
internal/plan/       turns an app definition into deploy steps
internal/buildinfo/  version metadata injected at build time
```
