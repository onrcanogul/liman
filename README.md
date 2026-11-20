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

They can run on one machine (see the quick start) or on two: the daemon on a
server, the CLI on your own machine (see Installing on a server).

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
==> [5/5] Retire previous containers of whoami
    $ docker stop <current container of whoami>   # kept for rollback; older ones are removed
```

`liman plan <file>` is a shorthand for the same thing.

A real deploy runs the steps on the server. The new release starts next to the
previous one, which is only retired once the new one is healthy and receiving
the traffic. If a step fails, or you press Ctrl+C, the daemon undoes the
completed steps in reverse order and the previous release keeps running:

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
liman rollback <app>         # put the previous release back into service
liman ps                     # containers managed by liman
liman logs -f <app>          # follow the logs of the newest release
liman stop <app>             # stop and remove all containers of an app; its secrets are kept
liman history [-n N] [app]   # every executed step, with result and duration
liman status                 # is the daemon reachable, is the token accepted
```

## Zero-downtime deploys and rollback

With Nginx configured, a deploy never leaves a moment in which nothing
answers:

1. the new release starts next to the current one and must pass its health
   check;
2. Nginx is switched to it, and liman waits until Nginx reports the new
   release;
3. the previous release keeps running for a drain period (5 seconds by
   default), so requests it had already accepted can finish;
4. the previous release is stopped, not deleted.

```yaml
# config.yaml
deploy:
  drain: 5s   # the default; 0s disables the wait
```

Because the previous container is kept, going back needs no app definition
and no image pull:

```
$ liman rollback whoami
==> [1/6] Start previous container whoami-20261119-221645-519
==> [2/6] Health check
    GET http://127.0.0.1:62586/health
==> [3/6] Write Nginx config for whoami.localhost
==> [4/6] Reload Nginx
==> [5/6] Drain connections to the previous release
    waiting 5s
==> [6/6] Remove container whoami-20261119-221650-883

whoami is back on release 20261119-221645-519, running on the server at 127.0.0.1:62586
```

`liman rollback` accepts `--dry-run` and `--step` like a deploy, and a
rollback that fails leaves the current release in service. `liman ps` shows
which container is `current` and which is the `previous` one being kept.

Limits: exactly one previous release is kept, so only one step back is
possible, and the release rolled back from is removed. Database migrations
are not liman's business: a rollback runs the old code against whatever
schema the new code left behind.

## Health monitoring

The daemon checks every app at a fixed interval and restarts the ones that
have stopped working, without being asked:

- a container that is not running is restarted on the next check;
- a running container whose health check fails three times in a row is
  restarted too. This catches apps that are up but no longer answering,
  which Docker on its own does not notice.

An app that keeps failing is restarted with growing pauses in between (the
interval, then twice that, and so on up to five minutes) instead of in a
tight loop. After five healthy checks in a row its record is cleared, and a
new deploy always starts with a clean one. The monitor never touches an app
that is being deployed or stopped.

`liman ps` shows the verdict, and `liman history` records every restart:

```
$ liman ps
APP  CONTAINER                IMAGE              STATE    HEALTH                   ADDRESS          STATUS
web  web-20261119-220029-592  nginx:1.27-alpine  running  restarting (2 restarts)  127.0.0.1:64150  Up 2 seconds

web: health check failed: unexpected status 403 Forbidden; restarted, next check in 40s
```

```yaml
# config.yaml
monitor:
  interval: 10s          # the default
  failure_threshold: 3   # the default
  disabled: false
```

Nothing is restarted while the daemon itself is not running, so the daemon
should be kept alive by a service manager such as systemd.

## More on deploys

Only one deploy or stop per app runs at a time; a second one is refused.
Environment variable values are never printed. liman only lists and touches
containers it started itself (label `liman.app`).

## Secrets

Passwords and API keys do not belong in `app.yaml`, which is committed to a
repository. The definition only names them:

```yaml
secrets:
  - DATABASE_URL
  - API_KEY
```

The values are given to the daemon once, and it adds them to the container's
environment under the same names on every deploy:

```sh
liman secret set orders-api DATABASE_URL       # prompts; the input is not shown
cat key.txt | liman secret set orders-api API_KEY
liman secret list orders-api                   # names and dates, never values
liman secret rm orders-api API_KEY
```

The value is never a command line argument (that would put it in the shell
history and the process list). A deploy that refers to a secret that is not
set is refused before anything is pulled or started. A changed secret takes
effect on the next deploy.

Values are encrypted with AES-256-GCM before they are stored. Each one is
bound to its app and name, so a ciphertext copied to another row does not
decrypt. The key is `master.key` in `$LIMAN_HOME`, created on the first start
(or set `secrets.key_file` in `config.yaml`). **Back it up, and not next to
the database**: without it the secrets cannot be recovered, and the daemon
refuses to start rather than create a new key when secrets exist but the key
is gone. It also refuses a key file that other users can read.

What this protects is the database and its backups. It does not hide a secret
from someone who can run `docker inspect` on the server, or from the app
itself: the value is in the container's environment in plain text. There is
no API to read a secret back.

## The panel

While it runs, the daemon serves a web panel at `/ui/` on its listen address (`http://127.0.0.1:7070/ui/` by default):

- **Overview**: how many apps are fine, which ones need attention, a health
  timeline per app, deploys per day and the latest activity.
- **Apps**: every app with its status, release, address and restarts; per
  app the health history, the response time of its health check, its
  deploys, the names of its secrets, its activity and its logs.
- **Activity**, **Secrets** (names only) and **Daemon** (version, which
  parts are switched on).
- **Backups**: the backups on the server, a button to make one now and a
  check for each (is it intact, does the server's master key open it).

Sign in with an API token (`limand token create panel`). The token is kept
in that browser tab only and is forgotten when the tab closes.

Besides looking, the panel can do these things, each on the page it belongs
to:

| Action | Where | Before it happens |
|---|---|---|
| Roll back | App page | A dialog shows the release in service and the one that replaces it; progress is shown step by step |
| Stop and remove | App page | The app's name has to be typed |
| Add or replace a secret | App page, Secrets | The value is typed into a hidden field, sent once and never shown again |
| Remove a secret | App page, Secrets | A dialog says what stops working |
| Back up now, check a backup | Backups | Nothing to confirm: neither can lose data |

Deploying a new version and restoring a backup stay with the command line:
the first needs an `app.yaml`, the second a stopped daemon.

The files of the panel are compiled into `limand`; there is nothing to
install next to it. They are served without a token, because they contain no
data: the page asks the API for everything, like any other client. To turn
the panel off:

```yaml
# config.yaml
panel:
  disabled: true
```

The health timeline and the response times come from the health monitor,
which records every check it makes and keeps seven days of them. With the
monitor disabled those two charts are empty.

## The daemon

`limand` keeps its files in `$LIMAN_HOME` (default `~/.liman`):

| File | Content |
|---|---|
| `config.yaml` | Optional settings, see below |
| `liman.db` | SQLite database: step and restart history, releases, token hashes, encrypted secrets, seven days of health checks |
| `master.key` | The key the secrets are encrypted with; created on first start |

```yaml
# config.yaml
daemon:
  listen: 127.0.0.1:7070   # the default
```

The API is plain HTTP and controls Docker. The daemon therefore refuses to
start with a `listen` address that is not loopback or a private network
(`0.0.0.0`, an empty host and public addresses are rejected), and the CLI
refuses to send its token over `http://` to a public address.

On SIGTERM or Ctrl+C the daemon stops accepting requests and waits for running
ones, so a deploy in progress finishes or rolls back instead of being cut off.

### Tokens

Every request except `GET /v1/health` needs a token.

```
limand token create <name>                         # prints the token once
limand token create <name> --write-client <file>   # writes a client.yaml instead
limand token create <name> --expires 90d           # stops working after 90 days (or 12h)
limand token list                                  # with scope and expiry
limand token revoke <name>
```

Only a hash of each token is stored; a lost token cannot be recovered, only
replaced.

A token without `--expires` works until it is revoked; tokens created
before expiry existed are of that kind. An expired token is refused with
`401 token expired` (every other refusal says only `invalid token`), its
use is logged as `expired token used` with the token's name, and it stays
in `token list` until it is revoked. There is no renewing: create a new
token and revoke the old one, so that a value that may have leaked is
replaced as well. `GET /v1/info` reports `token_expires_at`, and the panel
warns from 14 days before the end.

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
| `POST /v1/apps/{name}/rollback[?dry_run=true][&step=true]` | Put the previous release back into service; streams like a deploy |
| `DELETE /v1/apps/{name}` | Stop and remove an app |
| `GET /v1/history[?app=][&limit=]` | Executed steps |
| `GET /v1/apps/{name}/secrets` | Names of an app's secrets, never values |
| `PUT /v1/apps/{name}/secrets/{key}` | Body: the value. Stores it encrypted |
| `DELETE /v1/apps/{name}/secrets/{key}` | Removes a secret |
| `GET /v1/info` | Version, start time and which parts of the daemon are switched on |
| `GET /v1/releases[?app=][&limit=]` | Deploys and how they ended, newest first |
| `GET /v1/apps/{name}/health[?range=6h\|24h\|7d]` | Health checks of an app, summarised in 48 buckets |
| `GET /v1/backups` | Backups on the server, newest first |
| `POST /v1/backups` | Make a backup now and check it |
| `POST /v1/backups/{name}/verify` | Check a backup: intact, and can its secrets be decrypted |
| `GET /ui/` | The panel's files; no token needed, they contain no data |

## Security

Anyone with a valid token can run arbitrary containers on the server, which
is equivalent to root. The API is built around keeping everyone else out:

- **Network.** Loopback or private network only, enforced on both sides (see
  above). Encryption is delegated to that network (WireGuard).
- **Authentication.** Bearer tokens of 256 random bits; only their SHA-256 is
  stored. A token is either `admin` or, created with `--read-only`, `read`:
  such a token can look at everything and is refused every request that is
  not a `GET`. Give the panel a read-only token wherever it only needs to
  show things. Every request is logged with the name of the token that made
  it (never the token), so `journalctl -u limand` answers "who did that".
  A successful read that the same token repeats within ten minutes is not
  logged again, so that the panel's refreshing does not fill the log;
  changes and failures are always logged.
  Ten failed attempts from one address (one /64 for IPv6) within a minute
  lock that address out (429) until the minute is over. A request with a
  valid token is never locked out: failures by someone else at the same
  address cannot keep the operator away.
- **Browsers.** Requests that a browser marks as coming from another site
  (`Sec-Fetch-Site`) are refused, as are requests carrying an `Origin` header, so a web
  page open on the same machine cannot talk to a daemon on 127.0.0.1. The
  one exception is the daemon's own panel: a request whose `Origin` is the
  daemon's own address is accepted, provided that address is an IP or
  `localhost` (a host name could be a hostile site pointed at 127.0.0.1).
  The token is not a cookie, so a foreign page has nothing the browser
  would attach for it; there is no session to ride on. A token typed into
  the panel can do everything that token can do, so with an admin token the
  panel is as sensitive as a terminal with the CLI configured. It signs
  itself out after 30 minutes without a click or a key press, and its
  content security policy also demands Trusted Types, which makes the
  browser itself refuse to turn a string into markup.
- **The panel.** Served with a content security policy that allows scripts,
  styles and connections from the daemon itself and nothing else, and
  forbids framing. The page builds everything from text nodes, so a
  container name or an error message containing markup is shown, not run.
  Its token lives in the tab's session storage; a script that ran in the
  page could read it, which is what the policy is there to prevent.
- **Slow and large requests.** Headers must arrive within 10 seconds and a
  body within 15; headers are limited to 16 KiB and bodies to 1 MiB; at most
  64 requests are handled at once; idle connections are closed.
- **Input.** Every field of an app definition and every app name in a URL is
  checked against an allowlist of characters and a maximum length. Values are
  never concatenated into SQL, shell commands or file paths.
- **Values read back.** What liman wrote onto a container as a label
  (release, domain, health check path) is validated again when it is read:
  anyone with Docker access can start a container with any labels, and an
  image can declare labels of its own.
- **Terminal output.** The CLI removes control characters from everything it
  prints, so container logs and error messages cannot repaint the terminal.
- **Files.** `liman.db`, `master.key` and `client.yaml` are readable by their
  owner only. Tokens, secrets and environment variable values are never
  written to logs.
- **Secrets.** Encrypted at rest and write-only over the API (see Secrets).

Known limits: the `liman` user's sudo rule makes root parse Nginx config that user
writes, no TLS of its own, only two scopes (an `admin` token can do
everything, for every app), tokens expire only when created with
`--expires`, and the failed-attempt limit is per address and kept in
memory.

## Installing on a server

What the server needs: Ubuntu or Debian with systemd, Docker, Nginx, and a
private network between it and your machine (WireGuard below). The installer
adds no packages.

**1. Build for Linux** (on your machine; no cross-compiler is needed):

```
make release        # dist/limand-linux-amd64, dist/limand-linux-arm64, and the CLI
```

**2. A private network.** The API is unencrypted and the daemon refuses to
listen on a public address, so the two machines need a private one. With
WireGuard, on the server (`/etc/wireguard/wg0.conf`):

```ini
[Interface]
Address = 10.8.0.1/24
ListenPort = 51820
PrivateKey = <server private key>

[Peer]
PublicKey = <your machine's public key>
AllowedIPs = 10.8.0.2/32
```

and on your machine the mirror image, with `Address = 10.8.0.2/24`, the
server's public key, `Endpoint = <server's public IP>:51820` and
`AllowedIPs = 10.8.0.1/32`. Keys come from `wg genkey | tee private.key | wg
pubkey`. Open UDP port 51820 in the server's firewall, then
`systemctl enable --now wg-quick@wg0`.

**3. Install** (copy `deploy/` and the binary to the server first):

```
sudo ./install.sh --binary ./limand-linux-amd64 --listen 10.8.0.1:7070
```

The script creates a system user `liman` (member of the `docker` group),
installs the binary and the systemd unit, writes
`/var/lib/liman/config.yaml`, creates `/etc/nginx/liman.d` with one include
line in `/etc/nginx/conf.d/liman.conf`, and allows the `liman` user exactly
two commands through sudo: `nginx -t` and `systemctl reload nginx`. Existing
Nginx configuration is not touched. Running it again upgrades the binary and
leaves everything else as it is.

**4. A token, and the client:**

```
sudo -u liman env LIMAN_HOME=/var/lib/liman limand token create laptop
```

```yaml
# ~/.liman/client.yaml on your machine
url: http://10.8.0.1:7070
token: liman_...
```

The panel is then at `http://10.8.0.1:7070/ui/`.

**Operating it:**

```
systemctl status limand          # running? last log lines
journalctl -u limand -f          # follow the log
systemctl restart limand         # waits for running deploys, up to two minutes
```

The unit restarts the daemon if it crashes and starts it at boot, after
Docker and WireGuard. It is sandboxed: `/usr` and `/etc` are read-only for
it (except `/etc/nginx/liman.d`), `/home` and device files are hidden, it
has a `/tmp` of its own and may not create namespaces or touch kernel
settings (`systemd-analyze security limand`). The backup unit is locked down
much further: no capabilities, no network, a read-only file system except
its two directories. Membership of the `docker` group is still equivalent to root; the
dedicated user limits accidents, not a compromised daemon.

The installer also schedules a daily backup; see Backups.

For private images, log in as the daemon's user:
`sudo -u liman -H docker login ghcr.io`.

## Backups

Everything the daemon knows that cannot be recreated is in `liman.db`:
tokens, encrypted secrets, the history. Containers, images and the Nginx
config are not backed up; deploying again rebuilds them.

```
limand backup create /var/backups/liman --keep 14
limand backup verify /var/backups/liman/liman-20261120-031500.db
limand backup restore /var/backups/liman/liman-20261120-031500.db
```

- **create** writes `liman-<date>-<time>.db` into the directory and can run
  while the daemon is working: the copy is made by SQLite itself and is the
  state at one moment. (Copying `liman.db` with `cp` while the daemon runs
  can produce a corrupt file.) The new file is then opened and checked, and
  only after that are older backups removed.
- **verify** checks a backup's signature, checks it for damage and tries
  every secret in it against the master key. A backup is only known to be
  good once it has been read.
- **restore** needs the daemon stopped. It refuses a backup whose signature
  is wrong, and one without a checkable signature unless `--allow-unsigned`
  is given. It checks the backup first, keeps
  the database it replaces as `liman.db.replaced-<date>-<time>` (the
  newest three of those are kept, older ones removed), and lists
  the tokens that are valid afterwards: one that was revoked after the
  backup was made is valid again. (Expiry is not turned back: a token
  that has expired since stays expired.)

The panel's Backups page does `create` and `verify` from the browser. It
uses the directory and count from `config.yaml`:

```yaml
# config.yaml
backup:
  dir: /var/backups/liman   # default: backups/ in $LIMAN_HOME
  keep: 14                  # the default; 0 keeps them all
```

Restoring is deliberately not offered there: it replaces the database the
daemon has open, so the daemon must be stopped.

**Backups are signed.** Next to every `liman-….db` is a `liman-….db.sig`:
an HMAC of the file, keyed from the master key. The tokens in a backup
become valid tokens when it is restored, and a token row is only a hash that
anyone can compute for a token of their own; the signature is what shows
that a backup is still the file the daemon wrote. Keep the two files
together when copying backups elsewhere.

**The master key is not in the backup**, on purpose: a backup that carries
its own key protects nothing. Copy `master.key` once, by hand, to a place
that is not where the backups go. Without it the secrets in every backup are
lost; `verify` and `restore` say so when the key does not match.

On a server installed with `install.sh` a systemd timer takes a backup every
day into `/var/backups/liman` and keeps the newest 14
(`systemctl list-timers limand-backup`, `journalctl -u limand-backup`). Those
files are on the server's own disk: copy them elsewhere with whatever you
already use (rsync, restic), or they are lost together with the server.

### Practising on a throwaway server

`make lab-server` boots an Ubuntu 24.04 container that runs systemd, Docker
and Nginx like a small server would, copies the installer into it and prints
what to type. `make lab-server-stop` removes it.

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

liman never reads or edits any other Nginx file. A deploy then gains three
steps between the health check and the retiring of the previous release:

```
==> [5/8] Write Nginx config for whoami.localhost
      ...
      upstream liman-whoami {
    -     server 127.0.0.1:58276;
    +     server 127.0.0.1:58352;
      }
      ...
==> [6/8] Reload Nginx
==> [7/8] Drain connections to the previous release
==> [8/8] Retire previous containers of whoami
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
make load-test          # throughput, latency and leak checks (about a minute)
```

### Load tests

`make load-test` starts a complete daemon inside the test process (real HTTP
server and SQLite database, Docker replaced by an in-memory engine) and
measures it: the read endpoints at rising concurrency, concurrent deploys,
many clients fighting over one app, and a mixed run that checks afterwards
that no goroutines or memory were left behind.

To aim the read scenarios at a running daemon with real Docker behind it:

```
LIMAN_LOAD_URL=http://127.0.0.1:7070 LIMAN_LOAD_TOKEN=liman_... \
  go test -count=1 -tags load -run TestLoadReads -v ./internal/server
```

The numbers depend on the machine; compare runs on one machine, not against
anything absolute. On the development laptop the daemon answers about
100,000 health checks, 10,000 history queries and 1,000 container listings
per second (the last one limited by Docker itself), and runs about 2,500
deploys per second against the in-memory engine. Beyond 64 concurrent
requests it sheds load with 503.

## Layout

```
cmd/liman/           CLI entry point (wiring only)
cmd/limand/          daemon entry point (wiring only)
internal/cli/        the liman command: arguments, output, exit codes
internal/daemon/     the limand command: serving and token management
internal/api/        wire types and the HTTP client (what the CLI links)
internal/server/     HTTP handlers, streaming, auth, per-app locking
internal/panel/      the web panel: static files embedded into limand
internal/backup/     creating, listing and checking backups (command and API)
internal/store/      SQLite: history, releases, tokens, sealed secrets, health samples
internal/secrets/    encryption of secrets at rest and the master key file
internal/app/        app definition: loading and validation
internal/deploy/     the steps of a deploy and how each is undone
internal/pipeline/   runs steps: dry run, confirmation, rollback
internal/engine/     container engine interface and shared types
internal/docker/     Docker implementation of the engine interface
internal/nginx/      renders, writes, tests and reloads proxy config
internal/config/     config.yaml and client.yaml
internal/monitor/    health monitor: periodic checks, restarts, backoff
internal/health/     the HTTP health probe shared by deploys and the monitor
internal/applock/    per-app lock shared by the API and the monitor
internal/textdiff/   line diff used to preview config changes
internal/loadtest/   small load generator used by the load tests
internal/buildinfo/  version metadata injected at build time
deploy/              systemd units (daemon, daily backup) and the server install script
scripts/             local development helpers
```
