# A health check timeout of the app's own, and logs that stop growing

Approved 2026-11-22. Size: a small change.

## Why

The first real app moved to liman, a Spring Boot API, starts in 32
seconds on its server (34.7 until the process is up). liman gives every
app 30 seconds to become healthy, so its first deploy would fail and be
undone. Its Compose file also capped the logs at 20 MB × 5; liman sets no
log options, and Docker's default `json-file` grows without limit until
the disk is full. Both hit any JVM app, and most apps that log.

## Goals

- `healthcheck.timeout` in `app.yaml`: how long a release may take to
  become healthy. Default 30s, the time every app gets today; between 1s
  and 10m.
- It applies however health is decided: the path, the image's
  `HEALTHCHECK`, `settle`, and the trial on a free port of an app on one
  fixed port. A rollback uses the timeout the release was deployed with.
- Containers' logs are capped where liman can tell the driver: `json-file`
  gets `max-size=20m`, `max-file=5`.

## Non-goals

- A start-up probe separate from the health check, or per-step timeouts.
- Log shipping, or a setting for the cap in `app.yaml` or `config.yaml`.

## Anti-goals

- Changing the checksum of a definition that does not use the new field:
  an upgrade of liman must redeploy nothing by itself.
- Overriding a log driver the administrator chose (journald, loki).

## Approaches

Timeout:

1. **A field in `app.yaml`, stored as a label (chosen).** The app knows
   how long it takes; a rollback reads the label, as it reads the path.
2. A daemon-wide setting. One slow app would slow every failed deploy.

Logs:

1. **liman sets the cap when the driver is `json-file` (chosen).** Cost:
   an administrator's own `max-size`/`max-file` in `daemon.json` lose to
   liman's on liman's containers; the README says so.
2. README only (`daemon.json`). Nothing changes until someone reads it.
3. Switching to the `local` driver. Overrides the administrator's choice.

## Design

- `Healthcheck.Timeout Duration`; `App.HealthTimeout()` returns it or
  30s. Validation: 1s to 10m when set. The checksum fields gain
  `healthcheck.timeout` only when it is set.
- The deployer's state carries the timeout; `waitHealthy` uses it where
  it used the daemon-wide value. The step's detail line names it.
- Label `liman.healthcheck.timeout` (empty when not set), read back
  through `trusted` with the same range; rollback uses it.
- The engine's log driver is known at `Connect`: the preset's
  (`k8s-file` for rootless Podman) or the engine's default from `/info`.
  For `json-file`, `Start` sets the two options.
- Podman: whether its Docker-compatible API accepts a cap for `k8s-file`
  is found by an integration test on all three engines; if it does, the
  cap is `max-size=20m` (Podman truncates, it does not keep old files).
  If not, the README says what to set in `containers.conf`.

## Left to the builder

- Names of the helpers, and whether the log options live in `Options`
  or on the client.
