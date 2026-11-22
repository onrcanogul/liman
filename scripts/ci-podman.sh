#!/usr/bin/env bash
# liman on Podman, as CI runs it, rootless (the default) or rootful:
# limand install as a service behind Caddy, the engine's integration tests
# against the socket the daemon uses, then deploys from a pipeline through
# a registry that wants a password, logged in to with podman login. Rootless
# also: the limits inside a container, and the apps back after the liman
# user's systemd restarts.
#
#   scripts/ci-podman.sh rootless   # on a throwaway machine only
#   scripts/ci-podman.sh rootful
#
# It changes the machine it runs on (systemd units, registries.conf.d), so
# it refuses to run unless CI=true or LIMAN_E2E_THROWAWAY=yes.
set -euo pipefail

if [ "${CI:-}" != true ] && [ "${LIMAN_E2E_THROWAWAY:-}" != yes ]; then
    echo "ci-podman: this changes the machine it runs on; run it on a throwaway one (LIMAN_E2E_THROWAWAY=yes)" >&2
    exit 2
fi

MODE=${1:-rootless}
case "$MODE" in rootless|rootful) ;; *) echo "ci-podman: rootless or rootful, not $MODE" >&2; exit 2 ;; esac

cd "$(dirname "$0")/.."
SOCK=/run/podman/podman.sock
WORK=$(mktemp -d)
chmod 755 "$WORK"
# One auth file for podman login and for limand, wherever sudo's
# environment would otherwise put it.
AUTH="$WORK/auth.json"

step() { echo "::endgroup::"; echo "::group::$*"; }
fail() { echo "::error::$*"; exit 1; }
on_exit() {
    local code=$?
    echo "::endgroup::"
    if [ "$code" -ne 0 ]; then
        echo "::group::limand log"; sudo journalctl -u limand --no-pager -n 60 || true; echo "::endgroup::"
        echo "::group::containers"; sudo podman ps -a || true; if [ -n "${U:-}" ]; then pod ps -a || true; fi; echo "::endgroup::"
        if [ -n "${U:-}" ]; then echo "::group::the liman user's journal"; sudo journalctl "_UID=$U" --no-pager -n 60 || true; echo "::endgroup::"; fi
    fi
}
trap on_exit EXIT
echo "::group::start"

as_root() { sudo env "PATH=$PATH" "HOME=$HOME" REGISTRY_AUTH_FILE="$AUTH" "$@"; }
limand() { sudo -u liman env LIMAN_HOME=/var/lib/liman /usr/local/bin/limand "$@"; }
# As the liman user, in a clean environment: sudo would bring some of
# this one's along.
as_liman() { (cd / && sudo -u liman env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    HOME=/var/lib/liman USER=liman LOGNAME=liman "XDG_RUNTIME_DIR=/run/user/$U" "$@"); }
# The Podman that runs liman's apps.
pod() { if [ "$MODE" = rootless ]; then as_liman podman "$@"; else sudo podman "$@"; fi; }

step "Podman"
podman --version
if [ "$MODE" = rootless ]; then
    # The runner writes its own XDG_RUNTIME_DIR and XDG_CONFIG_HOME into
    # /etc/environment, which PAM loads into every session, the liman
    # user's systemd too: its Podman then looks in the runner's
    # /run/user/1001 and fails (a trial). A server has no such lines.
    sudo sed -i '/^XDG_/d' /etc/environment
else
    sudo systemctl enable --now podman.socket
    for _ in $(seq 1 20); do sudo test -S "$SOCK" && break; sleep 0.5; done
    sudo test -S "$SOCK" || fail "no Podman socket at $SOCK"

    step "The engine's integration tests, against rootful Podman"
    as_root "GOPATH=$(go env GOPATH)" "GOMODCACHE=$(go env GOMODCACHE)" "GOCACHE=$(go env GOCACHE)" \
        LIMAN_TEST_ENGINE=podman go test -v -count=1 -tags integration ./internal/docker/
fi

step "A registry that wants a password"
# Docker takes localhost for a plain-HTTP registry; Podman must be told.
printf '[[registry]]\nlocation = "localhost:5000"\ninsecure = true\n' | sudo tee /etc/containers/registries.conf.d/50-liman-ci.conf >/dev/null
mkdir -p "$WORK/htpasswd"
sudo podman run --rm --entrypoint htpasswd docker.io/library/httpd:2 -Bbn ci s3cret | tee "$WORK/htpasswd/file" >/dev/null
sudo podman run -d --name registry -p 127.0.0.1:5000:5000 -v "$WORK/htpasswd:/auth:Z" \
    -e REGISTRY_AUTH=htpasswd -e REGISTRY_AUTH_HTPASSWD_REALM=ci -e REGISTRY_AUTH_HTPASSWD_PATH=/auth/file \
    docker.io/library/registry:2 >/dev/null
for _ in $(seq 1 20); do curl -s -o /dev/null http://127.0.0.1:5000/v2/ && break; sleep 0.5; done
echo s3cret | as_root podman login -u ci --password-stdin localhost:5000
sudo podman pull -q docker.io/traefik/whoami:v1.10 >/dev/null
sudo podman tag docker.io/traefik/whoami:v1.10 localhost:5000/whoami:ci
as_root podman push -q --digestfile "$WORK/digest" localhost:5000/whoami:ci
DIGEST=$(cat "$WORK/digest")
[ -n "$DIGEST" ] || fail "no digest from the push"
echo "$DIGEST"
# Pulled from the registry, not found here.
sudo podman rmi -f localhost:5000/whoami:ci >/dev/null

step "limand install: Podman $MODE and Caddy, as a service"
sudo apt-get update -qq && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq caddy >/dev/null
caddy version
make build >/dev/null
# Docker is running here too: Podman is named. Caddy is the only proxy
# running, on port 80, so it is found. Rootless unless --rootful.
rootful=(); [ "$MODE" = rootful ] && rootful=(--rootful)
sudo bin/limand install --binary bin/limand --engine podman "${rootful[@]}" --listen 127.0.0.1:7070 --hooks-listen 127.0.0.1:7443 </dev/null | tee "$WORK/install.log"
grep -q "Installing: podman ($MODE), proxy caddy" "$WORK/install.log" || fail "not Podman $MODE and Caddy"
grep -q 'import /etc/caddy/liman.d/\*.caddy' /etc/caddy/Caddyfile || fail "the Caddyfile does not import liman's directory"
systemctl is-active limand
U=$(id -u liman)
if [ "$MODE" = rootless ]; then
    S=/run/user/$U/podman/podman.sock
    grep '^liman:' /etc/subuid /etc/subgid || fail "liman has no subordinate ids"
    sudo grep -q "socket: $S" /var/lib/liman/config.yaml || fail "config.yaml does not name liman's own socket"
    as_liman curl -fs --unix-socket "$S" http://d/_ping >/dev/null || fail "the liman user cannot reach its own Podman"
    if sudo -u nobody curl -fs --unix-socket "$S" http://d/_ping >/dev/null 2>&1; then fail "another user reaches liman's Podman"; fi

    step "The engine's integration tests, against liman's rootless Podman"
    go test -c -tags integration -o "$WORK/docker.test" ./internal/docker/
    chmod 755 "$WORK/docker.test"
    # limand is stopped while they run: its health monitor looks after every
    # container with a liman.app label on this Podman, the tests' too, and
    # would restart the one they have just killed or stopped. On a server a
    # deploy or a stop holds the app's lock and the monitor waits; the tests
    # do not take it. (Against rootful Podman they run before the install.)
    sudo systemctl stop limand
    as_liman LIMAN_TEST_ENGINE=podman-rootless LIMAN_TEST_SOCKET="$S" "$WORK/docker.test" -test.v -test.count=1 ||
        fail "the integration tests fail on rootless Podman"
    sudo systemctl start limand
    for _ in $(seq 1 20); do curl -s -o /dev/null http://127.0.0.1:7070/ && break; sleep 0.5; done
else
    # The socket's directory, as liman's tmpfiles rule makes it at boot
    # even with Podman's own rule there: all of them, as at boot.
    sudo systemd-tmpfiles --create 2>&1 | grep -i podman || true
    stat -c '%A %U:%G' /run/podman
    sudo -u liman curl -fs --unix-socket /run/podman/podman.sock http://d/_ping >/dev/null || fail "the liman user cannot reach Podman's socket"
fi
# The daemon's login: podman login, into the file the liman user reads.
sudo install -d -o liman -g liman -m 0700 /var/lib/liman/.config/containers
echo s3cret | sudo REGISTRY_AUTH_FILE=/var/lib/liman/.config/containers/auth.json podman login -u ci --password-stdin localhost:5000
sudo chown liman:liman /var/lib/liman/.config/containers/auth.json
# The client files are written as the liman user, so into a directory it
# owns, then handed to this one.
sudo install -d -o liman -g liman -m 0700 "$WORK/from-liman"
limand app create edge --repo localhost:5000/whoami --publish 127.0.0.1:18090 \
    --write-client "$WORK/from-liman/ci.yaml" --url http://127.0.0.1:7443
limand app create web --repo localhost:5000/whoami --domain web.localhost \
    --write-client "$WORK/from-liman/web.yaml" --url http://127.0.0.1:7443
for k in ci web; do
    mkdir -m 0700 "$WORK/$k"
    sudo install -m 0600 -o "$(id -u)" -g "$(id -g)" "$WORK/from-liman/$k.yaml" "$WORK/$k/client.yaml"
done

cat > "$WORK/app.yaml" <<YAML
name: edge
image: localhost:5000/whoami
port: 80
publish: 127.0.0.1:18090
resources:
  memory: 64m
  cpus: 0.5
healthcheck:
  path: /health
YAML

step "Preview: liman asks the registry itself"
LIMAN_HOME="$WORK/ci" bin/liman preview --file "$WORK/app.yaml" --digest "$DIGEST" | tee "$WORK/preview.log"
grep -q 'in the registry' "$WORK/preview.log" || fail "the preview did not find the image in the registry"

step "Trigger: the image is pulled with podman login's credentials"
LIMAN_HOME="$WORK/ci" bin/liman trigger --digest "$DIGEST" --file "$WORK/app.yaml" | tee "$WORK/trigger.log"
grep -q 'Authenticating to localhost:5000 as ci' "$WORK/trigger.log" || fail "the pull did not use podman login's credentials"
curl -fsS http://127.0.0.1:18090/ | grep -q '^Hostname:' || fail "nothing answers on 127.0.0.1:18090"
# The kernel's own record of the limits, read from the host: the image has
# no shell to ask from inside.
id=$(pod ps --filter label=liman.app=edge --format '{{.ID}}' --no-trunc | head -1)
pod inspect --format 'log driver: {{.HostConfig.LogConfig.Type}}' "$id"
mem=$(sudo find /sys/fs/cgroup -path "*libpod-$id*" -name memory.max -exec cat {} + | sort -u | tr '\n' ' ')
cpu=$(sudo find /sys/fs/cgroup -path "*libpod-$id*" -name cpu.max -exec cat {} + | sort -u | tr '\n' ' ')
echo "cgroup of $id: memory.max $mem, cpu.max $cpu"
if ! grep -qw 67108864 <<<"$mem" || ! grep -q '50000 100000' <<<"$cpu"; then
    fail "the limits are not applied: memory.max $mem, cpu.max $cpu"
fi

step "Through Caddy, as installed"
printf 'name: web\nimage: localhost:5000/whoami\nport: 80\ndomain: web.localhost\nhealthcheck:\n  path: /health\n' > "$WORK/web-app.yaml"
LIMAN_HOME="$WORK/web" bin/liman trigger --digest "$DIGEST" --file "$WORK/web-app.yaml" | tee "$WORK/web.log"
grep -q 'Reload Caddy' "$WORK/web.log" || fail "Caddy was not reloaded"
curl -fsS -H 'Host: web.localhost' http://127.0.0.1/ | grep -q '^Hostname:' || fail "Caddy does not route web.localhost"

step "An image the registry does not have is refused"
missing="sha256:$(printf '0%.0s' $(seq 1 64))"
if out=$(LIMAN_HOME="$WORK/ci" bin/liman trigger --digest "$missing" --file "$WORK/app.yaml" 2>&1); then
    fail "a digest the registry does not have was deployed"
fi
echo "$out" | tail -2
grep -qi 'not in the registry\|does not have\|not found' <<<"$out" || fail "unexpected refusal: $out"

if [ "$MODE" = rootless ]; then
    step "The liman user's systemd restarted: the health monitor brings the apps back"
    # As at a reboot, or an upgrade of systemd: its processes, Podman's and
    # the containers', stop with it.
    sudo systemctl restart "user@$U"
    back=no
    for _ in $(seq 1 30); do
        if curl -fs -o /dev/null http://127.0.0.1:18090/ && curl -fs -o /dev/null -H 'Host: web.localhost' http://127.0.0.1/; then back=yes; break; fi
        sleep 2
    done
    pod ps --format '{{.Names}} {{.Status}}'
    [ "$back" = yes ] || fail "the apps did not come back after user@$U restarted"
    sudo journalctl -u limand --no-pager -n 40 | grep -i 'restart' | tail -4
fi

step "Done"
echo "Podman ($MODE) test passed"
