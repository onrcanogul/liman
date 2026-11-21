#!/usr/bin/env bash
# liman on rootful Podman, as CI runs it: the engine's integration tests
# against Podman's socket, then deploys from a pipeline through a registry
# that wants a password, logged in to with podman login.
#
#   scripts/ci-podman.sh            # on a throwaway machine only
#
# It changes the machine it runs on (systemd units, registries.conf.d), so
# it refuses to run unless CI=true or LIMAN_E2E_THROWAWAY=yes.
set -euo pipefail

if [ "${CI:-}" != true ] && [ "${LIMAN_E2E_THROWAWAY:-}" != yes ]; then
    echo "ci-podman: this changes the machine it runs on; run it on a throwaway one (LIMAN_E2E_THROWAWAY=yes)" >&2
    exit 2
fi

cd "$(dirname "$0")/.."
SOCK=/run/podman/podman.sock
WORK=$(mktemp -d)
chmod 755 "$WORK"
# One auth file for podman login and for limand, wherever sudo's
# environment would otherwise put it.
AUTH="$WORK/auth.json"
LIMAN_HOME_DIR="$WORK/home"

step() { echo "::endgroup::"; echo "::group::$*"; }
fail() { echo "::error::$*"; exit 1; }
on_exit() {
    local code=$?
    echo "::endgroup::"
    if [ "$code" -ne 0 ]; then
        echo "::group::limand log"; sudo cat "$WORK/limand.log" 2>/dev/null | tail -60 || true; echo "::endgroup::"
        echo "::group::containers"; sudo podman ps -a || true; echo "::endgroup::"
    fi
}
trap on_exit EXIT
echo "::group::start"

as_root() { sudo env "PATH=$PATH" "HOME=$HOME" REGISTRY_AUTH_FILE="$AUTH" "$@"; }
limand() { as_root LIMAN_HOME="$LIMAN_HOME_DIR" bin/limand "$@"; }

step "Podman"
podman --version
sudo systemctl enable --now podman.socket
for _ in $(seq 1 20); do sudo test -S "$SOCK" && break; sleep 0.5; done
sudo test -S "$SOCK" || fail "no Podman socket at $SOCK"

step "The engine's integration tests, against Podman"
as_root "GOPATH=$(go env GOPATH)" "GOMODCACHE=$(go env GOMODCACHE)" "GOCACHE=$(go env GOCACHE)" \
    LIMAN_TEST_ENGINE=podman go test -count=1 -tags integration ./internal/docker/

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

step "limand on Podman"
make build >/dev/null
mkdir -p "$LIMAN_HOME_DIR"
cat > "$LIMAN_HOME_DIR/config.yaml" <<YAML
daemon:
  listen: 127.0.0.1:7070
hooks:
  listen: 127.0.0.1:7443
engine:
  driver: podman
monitor:
  disabled: true
YAML
limand app create edge --repo localhost:5000/whoami --publish 127.0.0.1:18090 \
    --write-client "$WORK/ci.yaml" --url http://127.0.0.1:7443
mkdir -m 0700 "$WORK/ci"
sudo install -m 0600 -o "$(id -u)" -g "$(id -g)" "$WORK/ci.yaml" "$WORK/ci/client.yaml"
(limand serve > "$WORK/limand.log" 2>&1 &)
for _ in $(seq 1 40); do curl -fs http://127.0.0.1:7070/v1/health >/dev/null && break; sleep 0.25; done
grep -o 'engine=[a-z]*' "$WORK/limand.log" | head -1

cat > "$WORK/app.yaml" <<YAML
name: edge
image: localhost:5000/whoami
port: 80
publish: 127.0.0.1:18090
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

step "An image the registry does not have is refused"
missing="sha256:$(printf '0%.0s' $(seq 1 64))"
if out=$(LIMAN_HOME="$WORK/ci" bin/liman trigger --digest "$missing" --file "$WORK/app.yaml" 2>&1); then
    fail "a digest the registry does not have was deployed"
fi
echo "$out" | tail -2
grep -qi 'not in the registry\|does not have\|not found' <<<"$out" || fail "unexpected refusal: $out"

step "Done"
echo "Podman test passed"
