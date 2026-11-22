# Reaching the server from an app: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every app container reaches the server as `host.docker.internal`
on Docker, rootful Podman and rootless Podman, proven in CI, with a
README recipe for the server's PostgreSQL.

**Architecture:** One switch in `docker.Options`, on for Docker, which
makes `Start` add `host.docker.internal:host-gateway` to the container's
extra hosts (Podman adds the name itself). An integration test on all
three engines, and an end-to-end step that deploys through liman an app
that writes to the server's PostgreSQL.

**Tech Stack:** Go 1.27, the moby client (`github.com/moby/moby/client`),
POSIX shell CI scripts on Ubuntu runners.

**Spec:** `docs/superpowers/specs/2026-11-22-host-access-design.md`

## Global Constraints

- No new `app.yaml` field.
- The name is exactly `host.docker.internal`, the same on every engine.
- Images used in tests have fixed tags (`latest` is refused by liman).
- Repo text is English. Commits end with
  `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- `go test ./...` and `make lint` pass after every task.

## Review Focus

- An image that already sets `host.docker.internal` (or an app whose
  `/etc/hosts` is read early): Docker must still start the container;
  a duplicate extra host is accepted by Docker. Covered by the
  integration test, which starts a plain busybox.
- Rootless Podman cannot reach the host, or reaches it from an address
  no `pg_hba.conf` line expects: the integration test logs the source
  address on every engine, and Task 2 records what it showed in the
  README rather than assuming Docker's answer.
- PostgreSQL started before Docker's bridge exists (boot order): Task 3
  documents the fix that CI's configuration actually uses.
- A server firewall (ufw) dropping container-to-host traffic: the README
  section names it as the next thing to check.
- The URL in `DATABASE_URL` written with `localhost`: the README says in
  one line why that reaches the container itself.

---

### Task 1: The host-gateway entry on Docker

**Files:**
- Modify: `internal/docker/docker.go` (`Options`, `ForDocker`, `Start`)
- Test: `internal/docker/podman_test.go` (preset test, next to the other
  preset tests)

**Interfaces:**
- Produces: `Options.HostGateway bool`, documented as "adds
  host.docker.internal, pointing at the server, to every container;
  Podman adds it itself". `ForDocker` sets it to true; `ForPodman` and
  `ForPodmanRootless` leave it false.

- [ ] **Step 1: Write the failing test**

```go
func TestOnlyDockerNeedsTheHostGatewayEntry(t *testing.T) {
	if !ForDocker("").HostGateway {
		t.Error("ForDocker: HostGateway = false, want true: Docker on Linux has no host.docker.internal")
	}
	if ForPodman("").HostGateway || ForPodmanRootless("/tmp/p.sock").HostGateway {
		t.Error("Podman presets: HostGateway = true, want false: Podman adds the name itself")
	}
}
```

- [ ] **Step 2: Run it to see it fail to compile**

Run: `go test ./internal/docker -run TestOnlyDockerNeedsTheHostGatewayEntry`
Expected: FAIL, `HostGateway` undefined.

- [ ] **Step 3: Add the field, set it in `ForDocker`, and in `Start` set
  `hostCfg.ExtraHosts = []string{"host.docker.internal:host-gateway"}`
  when it is on.** The comment on the field says why (Docker Desktop and
  Podman already do this; the name opens nothing new, the gateway is
  reachable today).

- [ ] **Step 4: Run the package tests**

Run: `go test ./internal/docker && make lint`
Expected: PASS.

- [ ] **Step 5: Commit** (`Name the server host.docker.internal in Docker containers`)

### Task 2: Prove it on all three engines

**Files:**
- Modify: `internal/docker/docker_integration_test.go` (new test after
  `TestFilesAreInTheContainerAndNotInItsConfig`)

**Interfaces:**
- Consumes: `Connect`, `testEngine()`, `Client.Pull`, `Client.Start`,
  `Client.Remove`, `engine.Spec{App, Name, Image, Command, Kind}`.

- [ ] **Step 1: Write the test `TestTheServerIsHostDockerInternal`**

  A `net.Listen("tcp", "0.0.0.0:0")` HTTP server on the host that sends
  each request's path and `RemoteAddr` on a channel. A worker container
  (`Kind: "worker"`, no port) from `busybox:1.37` with
  `Command: []string{"wget", "-q", "-O", "/dev/null", "http://host.docker.internal:<port>/<random token>"}`,
  named `liman-integration-host`, removed before and in `t.Cleanup`.
  Assertions:
  - a request with path `/<token>` arrives within 30 seconds, else
    `t.Fatalf("no request from the container reached the host on %s", engine name)`;
  - `t.Logf("%s: the host saw the container as %s", engine name, remote)`.

- [ ] **Step 2: Run it against local Docker before Task 1's change is
  in effect** (`git stash` the `Start` change, or run on `main`):

  Run: `go test -tags integration ./internal/docker -run TestTheServerIsHostDockerInternal -v`
  Expected on Linux Docker: FAIL (name does not resolve). On Docker
  Desktop it passes either way; then rely on CI for the red run.

- [ ] **Step 3: Run it with Task 1 in place:** same command, expected PASS
  and a log line with the source address.

- [ ] **Step 4: Commit** (`Test that every engine reaches the server as host.docker.internal`)

- [ ] **Step 5: Read the three log lines in CI** (Docker, rootful,
  rootless jobs) once the branch's draft PR has run, and note them for
  Task 3. If rootless fails, stop and bring the failure back to the user
  before going on.

### Task 3: The server's PostgreSQL, end to end, and the README

**Files:**
- Modify: `scripts/ci-e2e.sh` (new step after "A worker: no domain, no
  port, nothing routed")
- Modify: `README.md` (new section "A database on the server", after
  "Secrets as files")

**Interfaces:**
- Consumes: the e2e script's `step`, `fail`, `as_liman`, the local
  registry at `localhost:5000`, `liman trigger` with a worker key, and
  env expansion in `app.yaml` (`${VAR}` taken from the pipeline's
  environment, as `GREETING` is today).

- [ ] **Step 1: Add the e2e step "A database on the server"**

  - Install `postgresql` with apt; apply exactly the README's settings:
    `listen_addresses`, one `pg_hba.conf` line for Docker's bridge with
    `scram-sha-256`, the boot-order fix; restart PostgreSQL.
  - Create role and database `shop` with a password, owner `shop`,
    `REVOKE CONNECT ... FROM PUBLIC`.
  - Push `postgres:17-alpine` to `localhost:5000/dbcheck:ci`; register
    app `dbcheck` as a worker; deploy with `DATABASE_URL` from the
    environment (`postgresql://shop:<pw>@host.docker.internal:5432/shop`)
    and `command: [sh, -c, 'psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -c "create table if not exists liman_ok (at timestamptz default now())" && exec sleep infinity']`.
  - Check on the host:
    `sudo -u postgres psql -d shop -tAc "select to_regclass('liman_ok')"` prints `liman_ok`,
    else `fail "the app did not reach the server's PostgreSQL"`.

- [ ] **Step 2: Write the README section.** The URL with
  `host.docker.internal` and why `localhost` does not work, the
  `listen_addresses` and `pg_hba.conf` lines as CI applies them, the
  boot-order fix CI uses, the per-app user and `REVOKE CONNECT`, the
  source address each engine showed in Task 2, and "a firewall (ufw)
  must allow it" as the next check.

- [ ] **Step 3: Verify**

  Run: `bash -n scripts/ci-e2e.sh && go test ./... && make lint`, then
  push the branch as a draft PR.
  Expected: all CI jobs pass, including "end to end (fresh server)"
  with the new step.

- [ ] **Step 4: Commit** (`CI and README: an app on the server's PostgreSQL`)
