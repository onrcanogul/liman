# liman

A small PaaS for a single server, written in Go. It automates the deploy steps
usually done by hand over SSH: pull an image, start a container with limits,
health check it, write the Nginx config, reload.

This is a learning project. See [ROADMAP.md](ROADMAP.md) for the plan and
current status.

## Requirements

- Go 1.27+
- Docker
- [golangci-lint](https://golangci-lint.run/) v2 for `make lint`

## Development

```
make build   # build bin/liman with version info
make test    # run tests
make lint    # run linters
make check   # lint + test
make test-integration   # tests that need a running Docker daemon
```

```
$ ./bin/liman version
liman 3f2a1c9 (commit 3f2a1c9, built 2026-11-19T17:00:00Z)
```

Every deploy is a list of named steps. A dry run shows them, together with
the command you would have typed by hand, without touching anything:

```
$ ./bin/liman deploy --dry-run examples/whoami.yaml
Dry run for whoami: nothing will be changed.

==> [1/5] Pull image traefik/whoami:v1.10
    $ docker pull traefik/whoami:v1.10
==> [2/5] Allocate a free host port
==> [3/5] Start container whoami-20261119-181805
    $ docker run -d --name whoami-20261119-181805 --label liman.app=whoami -p 127.0.0.1:<free port>:80 --memory 64m --cpus 0.5 -e WHOAMI_NAME=*** traefik/whoami:v1.10
==> [4/5] Health check
    $ curl --fail http://127.0.0.1:<free port>/health
==> [5/5] Remove previous containers of whoami
    $ docker rm -f $(docker ps -aq --filter label=liman.app=whoami)   # except whoami-20261119-181805
```

`liman plan <file>` is a shorthand for the same thing. See
[examples/app.yaml](examples/app.yaml) for the app definition format.

A real deploy runs the steps against the local Docker daemon. The new release
starts next to the previous one and the previous one is only removed once the
new one is healthy. If a step fails, or you press Ctrl+C, the completed steps
are undone in reverse order and the previous release keeps running:

```
$ ./bin/liman deploy broken.yaml
==> [1/5] Pull image ...
==> [2/5] Allocate a free host port
    127.0.0.1:57840
==> [3/5] Start container web-20261119-181848
    c0fcb3eb3a90
==> [4/5] Health check
    GET http://127.0.0.1:57840/nope
    container output:
        ...
<== Undo: Start container web-20261119-181848
liman: Health check: not healthy after 30s: unexpected status 404 Not Found
```

Other commands:

```
liman deploy --step <file>   # ask before every step; "n" undoes what was done
liman ps                     # containers managed by liman
liman logs -f <app>          # follow the logs of the newest release
liman stop <app>             # stop and remove all containers of an app
liman history [-n N] [app]   # every executed step, with result and duration
```

History is kept in `~/.liman/history.jsonl` (override the directory with
`LIMAN_HOME`). Environment variable values are never printed.

liman only lists and touches containers it started itself (label
`liman.app`). Private registries are not supported yet.

## Layout

```
cmd/liman/           CLI entry point (wiring only)
internal/cli/        command dispatch
internal/app/        app definition: loading and validation
internal/deploy/     the steps of a deploy and how each is undone
internal/pipeline/   runs steps: dry run, confirmation, rollback
internal/history/    append-only record of executed steps
internal/engine/     container engine interface and shared types
internal/docker/     Docker implementation of the engine interface
internal/buildinfo/  version metadata injected at build time
```
