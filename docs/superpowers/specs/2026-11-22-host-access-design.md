# Reaching services on the server from an app

Approved 2026-11-22. Size: a small change.

## Why

The first real use of liman is the author's own Spring Boot apps, which
today are cloned and started with Docker Compose on the server. Their
database is the server's own PostgreSQL: one instance, one database and
one user per app. In a container, `localhost` is the container itself, so
the app needs a name for the server. Docker Desktop and Podman provide
`host.docker.internal`; Docker on Linux does not, and liman does not add
it. Without it the only way is a gateway IP such as `172.17.0.1`, which
differs between servers. Other users will hit the same wall: a database
on the server, or in a container next to it, is the common case.

## Goals

- In every engine liman supports (Docker, rootful Podman, rootless
  Podman), an app reaches the server as `host.docker.internal`, with no
  setting in `app.yaml`.
- CI proves it on all three engines, and proves the PostgreSQL recipe
  in the README end to end through liman.
- The README says how to point an app at the server's PostgreSQL: the
  URL, `listen_addresses`, the `pg_hba.conf` line, and the boot-order
  trap.

## Non-goals

- Running databases or other stateful services under liman (volumes,
  "accessories"). Decided against for now; revisit if users ask.
- `network_mode: host`.
- Configuring PostgreSQL for the user. liman only documents it.

## Anti-goals

- A new `app.yaml` field for something that opens no new access.
- Different instructions per engine for the app's side.

## Approaches

1. **Always add the name on Docker (chosen).** Docker gets
   `host.docker.internal:host-gateway` on every container; Podman adds
   the name itself. The name opens nothing new: the gateway address is
   reachable today.
2. **An opt-in `app.yaml` field.** A setting for a name every other
   engine sets by default. Rejected as noise.
3. **Host networking.** Gives up port isolation and breaks overlapping
   releases, two of which cannot listen on one port. Rejected.

## Design

- `docker.Options` gains a switch for adding the host-gateway entry;
  `ForDocker` turns it on, the Podman presets leave it to Podman.
  `Start` adds `host.docker.internal:host-gateway` to `ExtraHosts` when
  it is on.
- An integration test, run on all three engines: a listener on the host,
  a short-lived container (a small image with a shell, such as busybox)
  that requests `http://host.docker.internal:<port>/<token>`, and the
  test waits for that request. It logs the source address the host saw,
  which is what `pg_hba.conf` must allow on that engine.
- Rootless Podman (pasta) is the unknown: the test shows whether it
  reaches the host and from which address. If it cannot, the README says
  so and the fix is decided then.
- `scripts/ci-e2e.sh` (Docker, Nginx) installs PostgreSQL, applies the
  README's settings, and deploys through liman a worker that writes to
  its database with a `DATABASE_URL` secret using `host.docker.internal`;
  the script then checks the write on the host.
- README: a section on reaching the server's PostgreSQL, linked from
  the Compose comparison if one exists by then.

## Left to the builder

- The switch's name, and whether the integration test lives in the
  existing lifecycle test file or its own.
- The image for the probe container, pinned to a fixed tag.
- The exact boot-order fix to document (listening on `*` behind
  `pg_hba.conf`, or a systemd ordering on Docker), chosen by what CI and
  the docs of PostgreSQL and Docker support.
