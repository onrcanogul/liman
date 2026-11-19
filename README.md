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
make dev-nginx          # local Nginx in Docker, see below
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

## Routing with Nginx

Without further configuration an app is only reachable on its loopback port.
To route the app's `domain` to it, tell liman where it may write Nginx
config. On a server with Nginx installed, create `~/.liman/config.yaml`:

```yaml
nginx:
  conf_dir: /etc/nginx/liman      # liman writes one <app>.conf here
  cert_dir: /etc/liman/certs      # optional: <domain>/fullchain.pem + privkey.pem
  acme_root: /var/lib/liman/acme  # optional: served for ACME HTTP-01 challenges
```

and include that directory once from the `http` block of your Nginx config:

```
include /etc/nginx/liman/*.conf;
```

liman never reads or edits any other Nginx file. A deploy then gains two
steps between the health check and the removal of the previous release:

```
==> [5/7] Write Nginx config for whoami.localhost
      ...
      upstream liman-whoami {
    -     server 127.0.0.1:58276;
    +     server 127.0.0.1:58352;
      }
      ...
==> [6/7] Reload Nginx
==> [7/7] Remove previous containers of whoami
```

The reload step runs `nginx -t` first, reloads, and then waits until Nginx
itself reports the new release (through an `X-Liman-Release` response header)
before the old container is removed. If anything fails the previous config is
restored and reloaded.

A domain is served over HTTPS when `cert_dir/<domain>/fullchain.pem` and
`privkey.pem` exist. liman does not obtain certificates yet.

Defaults assume Nginx runs on the host: `test_command: [nginx, -t]`,
`reload_command: [nginx, -s, reload]`, `upstream_host: 127.0.0.1`,
`probe_addr: 127.0.0.1:80`. All four can be overridden.

### Trying it locally

No Nginx needs to be installed; a script runs one in Docker and writes a
matching configuration under `.dev/` (git-ignored):

```
make build
make dev-nginx                      # prints an export LIMAN_HOME=... line
export LIMAN_HOME=$PWD/.dev
./bin/liman deploy examples/whoami.yaml
curl http://whoami.localhost:18080

make dev-cert DOMAIN=whoami.localhost   # self-signed certificate
./bin/liman deploy examples/whoami.yaml
curl -k https://whoami.localhost:18443

./bin/liman stop whoami
make dev-nginx-stop
```

liman only lists and touches containers it started itself (label
`liman.app`).

## Private images

liman keeps no registry credentials of its own. It uses the ones `docker
login` stored, the same way the docker CLI does: from
`~/.docker/config.json` (or `$DOCKER_CONFIG/config.json`), including
credential helpers such as the macOS keychain. Log in once:

```
echo "$GITHUB_TOKEN" | docker login ghcr.io -u <github-user> --password-stdin
```

For GHCR the token needs the `read:packages` scope. A deploy then shows which
account it uses, never the secret:

```
==> [1/5] Pull image ghcr.io/user/orders-api:1.4
    Authenticating to ghcr.io as user
```

If a pull is denied, the error says whether no credentials were found or the
stored ones were rejected.

## Layout

```
cmd/liman/           CLI entry point (wiring only)
internal/cli/        command dispatch
internal/app/        app definition: loading and validation
internal/deploy/     the steps of a deploy and how each is undone
internal/pipeline/   runs steps: dry run, confirmation, rollback
internal/history/    append-only record of executed steps
internal/config/     liman's own configuration (config.yaml)
internal/nginx/      renders, writes, tests and reloads proxy config
internal/textdiff/   line diff used to preview config changes
scripts/             local development helpers
internal/engine/     container engine interface and shared types
internal/docker/     Docker implementation of the engine interface
internal/buildinfo/  version metadata injected at build time
```
