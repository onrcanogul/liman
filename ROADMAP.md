# liman roadmap

A small PaaS for a single server, written in Go. The goal is to automate the
deploy steps that are done by hand on the server today, and to learn Go and
infrastructure along the way.

## Product

Three parts:

- `limand`: the daemon that runs on the server (where the real work happens)
- `liman`: the CLI that sends commands to the daemon
- A small read-only status panel (last stage)

Example usage:

```
liman deploy orders-api --image ghcr.io/user/orders-api:1.4 --domain api.example.com
liman logs orders-api
liman rollback orders-api
```

## How the work is done

This is a learning project. Every stage follows the same four steps:

1. **Concept:** learn the topic from scratch (what it is, why it exists, what
   happens without it).
2. **By hand:** do the job the tool will automate manually in a terminal first.
3. **Code:** write the same job in Go.
4. **Document:** record what was learned and done in personal notes.

## Decisions made up front

- **No builder.** No source code lives on the server; images are built in CI and
  pushed to GHCR. The tool only pulls ready-made images.
- **Nginx as the proxy.** Config is rendered from a template and written to the
  tool's own directory. Existing hand-written Nginx configs are never touched.
- **Development happens on a Mac, against local Docker.** The server is not
  touched during stages 0-3.
- **Every step must be observable.** This is not logging added afterwards; it is
  how the tool works:
  - Every deploy is made of explicit, named steps (pull image, allocate port,
    start container, health check, write Nginx config, reload).
  - `--dry-run`: shows the plan without doing anything; under each step it shows
    the command you would have typed by hand and the Nginx config diff.
  - `--step`: pauses before every step and waits for confirmation.
  - Every executed step is recorded with a timestamp (`liman history`).
- **The daemon listens only on the WireGuard interface.** The management API is
  never exposed to the internet.

## Stages

Durations cover coding only; the concept and by-hand steps add roughly half as
much again to each stage.

| # | Concept | By hand | What gets written | Working at the end | Go topic | Time |
|---|---|---|---|---|---|---|
| 0 | What a PaaS is, what similar tools do | Go setup, empty module | Setup, repo, project skeleton, lint | `liman version` | Toolchain, packages, modules | 2-3 h |
| 1 | Declarative definitions: "what" versus "how" | Designing an `app.yaml` | App definition file (`app.yaml`: name, image, domain, port, env, limits), parsing and validation | `liman plan app.yaml` lists the steps | Structs, error handling, writing tests | 4-6 h |
| 2 | Images, layers, registries; namespaces and cgroups; port mapping, limits | `docker pull/run/inspect/stats/logs` | Pulling images, starting containers with limits and stopping them via the Docker SDK | `liman deploy`, `liman ps`, `liman logs` locally | Interfaces, `context`, reading streams | 6-8 h |
| 3 | Step-by-step workflows, undo, idempotency | Breaking a deploy halfway by hand and recovering | Step pipeline, `--dry-run`, `--step`, history, undo on failure | A deploy where every step can be watched and confirmed | Interface design, closures, error wrapping | 5-7 h |
| 4 | Reverse proxy, server blocks, upstreams, TLS, Let's Encrypt | Writing an Nginx config by hand, `nginx -t`, reload | Nginx config generation, diff, `nginx -t`, reload, certificates | An app reachable over HTTPS on its domain | `text/template`, `os/exec`, file handling | 6-8 h |
| 5 | Daemons, systemd, HTTP APIs, tokens, WireGuard, SQLite | Writing a systemd unit by hand, calling the API with `curl` | Server daemon and HTTP API, CLI becomes a client, token auth, SQLite, systemd | Command from the Mac, deploy on the server | `net/http`, JSON, middleware, `database/sql` | 8-10 h |
| 6 | Health checks, restart policies | Killing a container and watching what happens | Health monitor and automatic restart | A crashed app comes back by itself | Goroutines, channels, `select`, tickers | 4-6 h |
| 7 | Blue-green deploys, switching traffic | Switching an upstream between two containers by hand | Zero-downtime deploy and `liman rollback` | Traffic moves if the new version is healthy, otherwise the old one stays | State machines, concurrency, testing | 6-8 h |
| 8 | Secret encryption, key management | Encrypting and decrypting a value by hand | Encrypted secret storage, read-only status panel | Status of every app in the browser | `crypto`, `html/template` | 5-7 h |

Coding total is roughly 45-60 hours.

## Milestones

- **End of stage 4:** the tool works end to end locally.
- **End of stage 5:** first real use on the server, starting with a single
  non-critical app.
- **End of stage 7:** the point where manual deploys can be dropped.

## Out of scope (first version)

Database provisioning and backups, multiple servers, autoscaling, metrics
collection.

## Target environment

- Server: Ubuntu 24.04, Docker, Nginx (reverse proxy), WireGuard
- Image source: GHCR
- First workloads: a Node.js backend and a Spring Boot backend

## Status

- [x] Stage 0
- [x] Stage 1
- [ ] Stage 2
- [ ] Stage 3
- [ ] Stage 4
- [ ] Stage 5
- [ ] Stage 6
- [ ] Stage 7
- [ ] Stage 8
