#!/usr/bin/env bash
# End-to-end test on a fresh Ubuntu machine, as CI runs it: install liman
# with install.sh, then deploy an app the way its pipeline would, through
# a registry and the trigger endpoint, and check what a user would see.
#
#   scripts/ci-e2e.sh            # on a throwaway machine only
#
# It installs packages, creates users and changes /etc, so it refuses to run
# unless CI=true or LIMAN_E2E_THROWAWAY=yes. Keys go to files and are never
# printed.
set -euo pipefail

if [ "${CI:-}" != true ] && [ "${LIMAN_E2E_THROWAWAY:-}" != yes ]; then
    echo "ci-e2e: this changes the machine it runs on; run it on a throwaway one (LIMAN_E2E_THROWAWAY=yes)" >&2
    exit 2
fi

cd "$(dirname "$0")/.."
STATE=/var/lib/liman
WORK=$(mktemp -d)
HOOKS=127.0.0.1:7443
DOMAIN=whoami.test

step() { echo "::endgroup::"; echo "::group::$*"; }
fail() { echo "::error::$*"; exit 1; }
# On any failure, what is needed to see why. None of it contains a key.
on_exit() {
    local code=$?
    echo "::endgroup::"
    if [ "$code" -ne 0 ]; then
        echo "::group::limand log"; sudo journalctl -u limand --no-pager -n 80 || true; echo "::endgroup::"
        echo "::group::nginx"; sudo nginx -t 2>&1 || true; sudo ls -l /etc/nginx/liman.d 2>/dev/null || true; echo "::endgroup::"
        echo "::group::containers"; docker ps -a || true; echo "::endgroup::"
    fi
    rm -rf "$WORK"
}
trap on_exit EXIT
echo "::group::start"

as_liman() { sudo -u liman env LIMAN_HOME="$STATE" /usr/local/bin/limand "$@"; }
# deploy_key / preview_key run the CLI as the pipeline does: a client.yaml
# with the key, and the variables in the environment.
deploy_key() { LIMAN_HOME="$WORK/deploy" GREETING=hello "$@"; }
preview_key() { LIMAN_HOME="$WORK/preview" "$@"; }
release_served() { curl -fsSI -H "Host: $DOMAIN" http://127.0.0.1/ | tr -d '\r' | sed -n 's/^X-Liman-Release: //Ip'; }

step "Build"
make release >/dev/null
dist/limand-linux-amd64 version | head -1
sudo install -m 0755 dist/liman-linux-amd64 /usr/local/bin/liman

step "install.sh refuses a stopped Nginx"
command -v nginx >/dev/null || { sudo apt-get update -qq; sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq nginx >/dev/null; }
sudo systemctl stop nginx
had_liman=no; id liman >/dev/null 2>&1 && had_liman=yes
# Named on the command line, and found stopped: refused before anything
# is changed.
if out=$(sudo deploy/install.sh --binary dist/limand-linux-amd64 --proxy nginx --listen 127.0.0.1:7070 --hooks-listen "$HOOKS" 2>&1); then
    fail "install.sh went ahead with Nginx stopped"
fi
grep -q 'Nginx is installed but not running' <<<"$out" || fail "unexpected refusal: $out"
# Not named, and none running: no guess, the flag to give.
if out=$(sudo deploy/install.sh --binary dist/limand-linux-amd64 --listen 127.0.0.1:7070 2>&1 </dev/null); then
    fail "install.sh picked a stopped proxy by itself"
fi
grep -q 'give --proxy' <<<"$out" || fail "unexpected refusal: $out"
[ "$had_liman" = yes ] || ! id liman >/dev/null 2>&1 || fail "install.sh changed the machine before refusing"
echo "refused, nothing changed"

step "install.sh, engine and proxy as found"
sudo systemctl enable --now nginx
# No --engine, no --proxy, no terminal: Docker because it runs (Podman is
# installed too), Nginx because it holds port 80.
sudo deploy/install.sh --binary dist/limand-linux-amd64 --listen 127.0.0.1:7070 --hooks-listen "$HOOKS" </dev/null | tee "$WORK/install.log"
grep -q 'Installing: docker, proxy nginx' "$WORK/install.log" || fail "not what was found"
systemctl is-active limand
sudo ss -ltn | grep -q " $HOOKS " || fail "nothing listens on $HOOKS"

step "install.sh again changes nothing"
sudo deploy/install.sh --binary dist/limand-linux-amd64 --listen 127.0.0.1:7070 --hooks-listen "$HOOKS" | tee "$WORK/again.log"
grep -q 'config.yaml exists, left as it is' "$WORK/again.log" || fail "config.yaml was rewritten"

step "Registry and image"
docker run -d --name registry -p 127.0.0.1:5000:5000 registry:2 >/dev/null
docker pull -q traefik/whoami:v1.10 >/dev/null
docker tag traefik/whoami:v1.10 localhost:5000/whoami:ci
# The digest the registry gives back, from the push itself: the local
# image's RepoDigests may name another registry's.
DIGEST=$(docker push localhost:5000/whoami:ci | grep -o 'sha256:[0-9a-f]\{64\}' | tail -1)
[ -n "$DIGEST" ] || fail "no digest in the push output"
echo "$DIGEST"

step "Register the app"
sudo install -d -o liman -g liman -m 0700 "$STATE/e2e"
as_liman app create whoami --repo localhost:5000/whoami --domain "$DOMAIN" \
    --write-client "$STATE/e2e/deploy.yaml" --url "http://$HOOKS" \
    --preview-key --preview-client "$STATE/e2e/preview.yaml"
for k in deploy preview; do
    mkdir -m 0700 "$WORK/$k"
    sudo install -m 0600 -o "$(id -u)" -g "$(id -g)" "$STATE/e2e/$k.yaml" "$WORK/$k/client.yaml"
done
sudo rm -rf "$STATE/e2e"
as_liman app list | tee "$WORK/list.log"
grep -q 'whoami-ci (deploy), whoami-pr (preview)' "$WORK/list.log" || fail "app list does not show both keys"

cat > "$WORK/app.yaml" <<YAML
name: whoami
image: localhost:5000/whoami
domain: $DOMAIN
port: 80
env:
  GREETING: \${GREETING}
healthcheck:
  path: /health
YAML

step "Preview, as on a pull request"
preview_key liman preview --file "$WORK/app.yaml" --digest "$DIGEST" | tee "$WORK/preview.log"
grep -q 'would be accepted' "$WORK/preview.log" || fail "the preview does not accept the definition"
grep -q 'must set: GREETING' "$WORK/preview.log" || fail "the preview does not name the variable"
if out=$(LIMAN_HOME="$WORK/preview" GREETING=x liman trigger --app whoami --digest "$DIGEST" --file "$WORK/app.yaml" 2>&1); then
    fail "a preview key deployed"
fi
grep -q 'preview key' <<<"$out" || fail "unexpected refusal of the preview key: $out"

step "Deploy, as after a merge"
deploy_key liman trigger --digest "$DIGEST" --file "$WORK/app.yaml"
# One request, kept whole: when it fails, what Nginx answered says why (a
# 502 from the app, or the default server).
answer=$(curl -sSI -H "Host: $DOMAIN" http://127.0.0.1/ 2>&1 | tr -d '\r')
first=$(sed -n 's/^X-Liman-Release: //Ip' <<<"$answer")
if [ -z "$first" ] || ! grep -q '^HTTP/[0-9.]* 200' <<<"$answer"; then
    fail "Nginx does not serve $DOMAIN; it answered: $(head -4 <<<"$answer" | tr '\n' ' ')"
fi
curl -fsS -H "Host: $DOMAIN" http://127.0.0.1/ | grep -q '^Hostname:' || fail "the app's answer is not whoami's"
echo "serving release $first"

step "The same run again does nothing"
deploy_key liman trigger --digest "$DIGEST" --file "$WORK/app.yaml" | tee "$WORK/again.log"
grep -q 'nothing to do' "$WORK/again.log" || fail "a repeated run deployed again"
[ "$(release_served)" = "$first" ] || fail "the release changed"

step "A changed definition: previewed, then deployed"
sed -i 's/^env:$/env:\n  LOG_LEVEL: debug/' "$WORK/app.yaml"
preview_key liman preview --file "$WORK/app.yaml" | tee "$WORK/preview2.log"
grep -q 'env.LOG_LEVEL' "$WORK/preview2.log" || fail "the preview does not show the added value"
deploy_key liman trigger --digest "$DIGEST" --file "$WORK/app.yaml"
second=$(release_served)
[ -n "$second" ] && [ "$second" != "$first" ] || fail "no new release is served ($first, then $second)"
echo "serving release $second"

step "Audit trail and its anchors in the system log"
as_liman audit verify
sudo journalctl -t limand --no-pager -o cat | grep -c 'msg=audit' | xargs echo "anchored records:"
sudo journalctl -t limand --no-pager -o cat | grep 'msg=audit' | grep -q 'action=deploy app=whoami' || fail "no deploy in the system log"

step "Backup service"
sudo systemctl start limand-backup.service
sudo sh -c "ls /var/backups/liman/*.db.age" >/dev/null || fail "no backup written"
sudo ls -l /var/backups/liman

step "A worker: no domain, no port, nothing routed"
docker tag traefik/whoami:v1.10 localhost:5000/mailer:ci
WORKER_DIGEST=$(docker push localhost:5000/mailer:ci | grep -o 'sha256:[0-9a-f]\{64\}' | tail -1)
sudo install -d -o liman -g liman -m 0700 "$STATE/e2e"
as_liman app create mailer --repo localhost:5000/mailer --worker \
    --write-client "$STATE/e2e/worker.yaml" --url "http://$HOOKS"
mkdir -m 0700 "$WORK/worker"
sudo install -m 0600 -o "$(id -u)" -g "$(id -g)" "$STATE/e2e/worker.yaml" "$WORK/worker/client.yaml"
sudo rm -rf "$STATE/e2e"
cat > "$WORK/worker.yaml" <<YAML
name: mailer
image: localhost:5000/mailer
kind: worker
# Any long-running process will do; this one listens only inside. The
# image's ENTRYPOINT is /whoami, so these are its arguments.
command: [--port, "8080"]
deploy:
  strategy: replace
YAML
LIMAN_HOME="$WORK/worker" liman trigger --digest "$WORKER_DIGEST" --file "$WORK/worker.yaml" | tee "$WORK/worker.log"
grep -q 'as a worker' "$WORK/worker.log" || fail "the worker was not deployed as one"
grep -q 'must stay up for 10s' "$WORK/worker.log" || fail "the worker was not given its settle time"
worker=$(docker ps --filter label=liman.app=mailer --format '{{.Names}}')
[ -n "$worker" ] || fail "the worker is not running"
# docker port lists what is published; an image's EXPOSE alone is not.
published=$(docker port "$worker")
[ -z "$published" ] || fail "the worker publishes a port: $published"
sudo test ! -e /etc/nginx/liman.d/mailer.conf || fail "the worker got an Nginx config"
# Its key cannot put it on the internet.
printf 'name: mailer\nimage: localhost:5000/mailer\ndomain: mail.test\nport: 8080\n' > "$WORK/worker-web.yaml"
if out=$(LIMAN_HOME="$WORK/worker" liman trigger --digest "$WORKER_DIGEST" --file "$WORK/worker-web.yaml" 2>&1); then
    fail "a worker's key deployed it as a web app"
fi
grep -q 'registered as a worker' <<<"$out" || fail "unexpected refusal: $out"
as_liman app list | grep mailer

step "A fixed port: tried first, then on its endpoint"
sudo install -d -o liman -g liman -m 0700 "$STATE/e2e"
as_liman app create edge --repo localhost:5000/whoami --publish 127.0.0.1:8090 \
    --write-client "$STATE/e2e/edge.yaml" --url "http://$HOOKS"
mkdir -m 0700 "$WORK/edge"
sudo install -m 0600 -o "$(id -u)" -g "$(id -g)" "$STATE/e2e/edge.yaml" "$WORK/edge/client.yaml"
sudo rm -rf "$STATE/e2e"
edge_yaml() {
    printf 'name: edge\nimage: localhost:5000/whoami\nport: 80\npublish: %s\nhealthcheck:\n  path: /health\n' "$1" > "$WORK/edge.yaml"
}
edge_yaml 127.0.0.1:8090
LIMAN_HOME="$WORK/edge" liman trigger --digest "$DIGEST" --file "$WORK/edge.yaml" | tee "$WORK/edge.log"
grep -q 'Try the new release on a free port' "$WORK/edge.log" || fail "the release was not tried first"
curl -fsS http://127.0.0.1:8090/ | grep -q '^Hostname:' || fail "nothing answers on 127.0.0.1:8090"
# Its key cannot move it onto every address: Docker's ports pass by ufw.
edge_yaml 0.0.0.0:8090
if out=$(LIMAN_HOME="$WORK/edge" liman trigger --digest "$DIGEST" --file "$WORK/edge.yaml" 2>&1); then
    fail "a key moved its app onto 0.0.0.0"
fi
grep -q 'registered with 127.0.0.1:8090' <<<"$out" || fail "unexpected refusal: $out"

step "Deleting the app revokes its keys"
as_liman app delete whoami | tee "$WORK/delete.log"
grep -q 'Revoked: whoami-ci, whoami-pr' "$WORK/delete.log" || fail "the keys were not revoked"
if out=$(deploy_key liman trigger --digest "$DIGEST" --file "$WORK/app.yaml" 2>&1); then
    fail "a revoked key deployed"
fi
grep -q 'invalid token' <<<"$out" || fail "unexpected refusal of a revoked key: $out"
[ "$(release_served)" = "$second" ] || fail "deleting the app stopped what runs"

step "Done"
echo "end-to-end test passed"
