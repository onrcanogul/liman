# liman

A small PaaS for a single server, written in Go. It automates the deploy steps
usually done by hand over SSH: pull an image, start a container with limits,
health check it, point Nginx at it, remove the old one.

This is a learning project. See [ROADMAP.md](ROADMAP.md) for the plan and
current status.

## How it fits together

```
your machine                         the server
liman (CLI)  ──HTTP + token──>  limand (daemon)  ──>  Docker, Nginx
```

- `limand` runs on the server and does the work.
- `liman` is a client: it sends commands to the daemon and prints the progress
  the daemon streams back.

For now both run on one machine; installing the daemon on a server is the next
stage.

## Requirements

- Go 1.27+
- Docker
- [golangci-lint](https://golangci-lint.run/) v2 for `make lint`

## Quick start (everything on one machine)

```
make build                                  # builds bin/liman and bin/limand
export LIMAN_HOME=$PWD/.dev                 # where both keep their files

./bin/limand token create dev --write-client $LIMAN_HOME/client.yaml
./bin/limand                                # serves on 127.0.0.1:7070; keep it running
```

In a second terminal (with the same `LIMAN_HOME`):

```
./bin/liman status
./bin/liman deploy examples/whoami.yaml
./bin/liman ps
./bin/liman stop whoami
```

## Deploying

An app is described by a file, see [examples/app.yaml](examples/app.yaml).
Every deploy is a list of named steps. A dry run shows them, together with the
command you would have typed by hand, without touching anything:

```
$ liman deploy --dry-run examples/whoami.yaml
Dry run for whoami: nothing will be changed.

==> [1/5] Pull image traefik/whoami:v1.10
    $ docker pull traefik/whoami:v1.10
==> [2/5] Allocate a free host port
==> [3/5] Start container whoami-20261119-181805-123
    $ docker run -d --name whoami-20261119-181805-123 --label liman.app=whoami -p 127.0.0.1:<free port>:80 --memory 64m --cpus 0.5 -e WHOAMI_NAME=*** traefik/whoami:v1.10
==> [4/5] Health check
    $ curl --fail http://127.0.0.1:<free port>/health
==> [5/5] Remove previous containers of whoami
    $ docker rm -f $(docker ps -aq --filter label=liman.app=whoami)   # except whoami-20261119-181805-123
```

`liman plan <file>` is a shorthand for the same thing.

A real deploy runs the steps on the server. The new release starts next to the
previous one, which is only removed once the new one is healthy. If a step
fails, or you press Ctrl+C, the daemon undoes the completed steps in reverse
order and the previous release keeps running:

```
$ liman deploy broken.yaml
==> [1/5] Pull image ...
==> [2/5] Allocate a free host port
    127.0.0.1:57840
==> [3/5] Start container web-20261119-181848-507
    c0fcb3eb3a90
==> [4/5] Health check
    GET http://127.0.0.1:57840/nope
    container output:
        ...
<== Undo: Start container web-20261119-181848-507
liman: Health check: not healthy after 30s: unexpected status 404 Not Found
```

Other commands:

```
liman deploy --step <file>   # ask before every step; "n" undoes what was done
liman ps                     # containers managed by liman
liman logs -f <app>          # follow the logs of the newest release
liman stop <app>             # stop and remove all containers of an app
liman history [-n N] [app]   # every executed step, with result and duration
liman status                 # is the daemon reachable, is the token accepted
```

Only one deploy or stop per app runs at a time; a second one is refused.
Environment variable values are never printed. liman only lists and touches
containers it started itself (label `liman.app`).

## The daemon

`limand` keeps its files in `$LIMAN_HOME` (default `~/.liman`):

| File | Content |
|---|---|
| `config.yaml` | Optional settings, see below |
| `liman.db` | SQLite database: step history, releases, token hashes |

```yaml
# config.yaml
daemon:
  listen: 127.0.0.1:7070   # the default
```

The API is plain HTTP and controls Docker, so `listen` must never be a public
address. On a server it will be the address of a private (WireGuard)
interface.

On SIGTERM or Ctrl+C the daemon stops accepting requests and waits for running
ones, so a deploy in progress finishes or rolls back instead of being cut off.

### Tokens

Every request except `GET /v1/health` needs a token.

```
limand token create <name>                         # prints the token once
limand token create <name> --write-client <file>   # writes a client.yaml instead
limand token list
limand token revoke <name>
```

Only a hash of each token is stored; a lost token cannot be recovered, only
replaced.

### The client side

`liman` reads the daemon's address and the token from
`$LIMAN_HOME/client.yaml`:

```yaml
url: http://127.0.0.1:7070
token: liman_...
```

The environment variables `LIMAN_URL` and `LIMAN_TOKEN` take precedence.

### API

| Request | Purpose |
|---|---|
| `GET /v1/health` | Liveness, no token needed |
| `POST /v1/deploys[?dry_run=true][&step=true]` | Body: app.yaml. Streams progress as JSON lines; the last line is the result |
| `POST /v1/confirmations/{id}` | Answers a question asked by a step-by-step deploy |
| `GET /v1/apps` | Managed containers |
| `GET /v1/apps/{name}/logs[?follow=true]` | Logs as plain text |
| `DELETE /v1/apps/{name}` | Stop and remove an app |
| `GET /v1/history[?app=][&limit=]` | Executed steps |

## Private images

The daemon keeps no registry credentials of its own. It uses the ones `docker
login` stored for the user it runs as, the same way the docker CLI does: from
`~/.docker/config.json` (or `$DOCKER_CONFIG/config.json`), including
credential helpers such as the macOS keychain. Log in once on the machine the
daemon runs on:

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

## Routing with Nginx

Without further configuration an app is only reachable on its loopback port.
To route the app's `domain` to it, tell the daemon where it may write Nginx
config. On a server with Nginx installed, add to `config.yaml`:

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
matching `config.yaml` under `.dev/` (git-ignored). Run it before starting the
daemon:

```
make dev-nginx
export LIMAN_HOME=$PWD/.dev
./bin/limand token create dev --write-client $LIMAN_HOME/client.yaml
./bin/limand
```

and in a second terminal:

```
export LIMAN_HOME=$PWD/.dev
./bin/liman deploy examples/whoami.yaml
curl http://whoami.localhost:18080

make dev-cert DOMAIN=whoami.localhost   # self-signed certificate
./bin/liman deploy examples/whoami.yaml
curl -k https://whoami.localhost:18443

./bin/liman stop whoami
make dev-nginx-stop
```

## Development

```
make build              # bin/liman and bin/limand, with version info
make test               # unit tests
make lint               # linters
make check              # lint + test
make test-integration   # tests that need a running Docker daemon
```

## Layout

```
cmd/liman/           CLI entry point (wiring only)
cmd/limand/          daemon entry point (wiring only)
internal/cli/        the liman command: arguments, output, exit codes
internal/daemon/     the limand command: serving and token management
internal/api/        wire types and the HTTP client (what the CLI links)
internal/server/     HTTP handlers, streaming, auth, per-app locking
internal/store/      SQLite: history, releases, tokens
internal/app/        app definition: loading and validation
internal/deploy/     the steps of a deploy and how each is undone
internal/pipeline/   runs steps: dry run, confirmation, rollback
internal/engine/     container engine interface and shared types
internal/docker/     Docker implementation of the engine interface
internal/nginx/      renders, writes, tests and reloads proxy config
internal/config/     config.yaml and client.yaml
internal/textdiff/   line diff used to preview config changes
internal/buildinfo/  version metadata injected at build time
scripts/             local development helpers
```
