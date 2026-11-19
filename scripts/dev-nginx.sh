#!/bin/sh
# Runs an Nginx in Docker for local development and writes the liman
# configuration that points at it.
#
#   scripts/dev-nginx.sh start
#   scripts/dev-nginx.sh stop
#
# Everything lives under .dev/ in the repository, which is git-ignored. Use
# it by setting LIMAN_HOME, as printed by "start".
#
# On a server none of this is needed: Nginx runs on the host and liman's
# defaults (nginx -t, nginx -s reload, upstream 127.0.0.1) apply.
set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
DEV_HOME="$ROOT/.dev"
NAME=liman-nginx
IMAGE=nginx:1.27-alpine
HTTP_PORT=${LIMAN_DEV_HTTP_PORT:-18080}
HTTPS_PORT=${LIMAN_DEV_HTTPS_PORT:-18443}

stop() {
    docker rm -f "$NAME" >/dev/null 2>&1 || true
}

start() {
    mkdir -p "$DEV_HOME/nginx" "$DEV_HOME/certs" "$DEV_HOME/acme"

    # Requests for a host name no app claims get a 404 instead of being
    # answered by whichever app happens to come first.
    cat > "$DEV_HOME/nginx/000-default.conf" <<CONF
# Written by scripts/dev-nginx.sh.
server {
    listen 80 default_server;
    server_name _;
    return 404;
}
CONF

    # The app containers publish their ports on the host's loopback
    # interface. From inside the Nginx container that is not 127.0.0.1 but
    # host.docker.internal.
    #
    # cert_dir and acme_root are mounted at the same path inside the
    # container as on the host, because the generated config refers to them
    # by the path liman sees.
    cat > "$DEV_HOME/config.yaml" <<CONF
# Written by scripts/dev-nginx.sh.
nginx:
  conf_dir: $DEV_HOME/nginx
  upstream_host: host.docker.internal
  probe_addr: 127.0.0.1:$HTTP_PORT
  test_command: [docker, exec, $NAME, nginx, -t]
  reload_command: [docker, exec, $NAME, nginx, -s, reload]
  cert_dir: $DEV_HOME/certs
  acme_root: $DEV_HOME/acme
CONF

    stop
    docker run -d --name "$NAME" \
        -p "127.0.0.1:$HTTP_PORT:80" \
        -p "127.0.0.1:$HTTPS_PORT:443" \
        --add-host host.docker.internal:host-gateway \
        -v "$DEV_HOME/nginx:/etc/nginx/conf.d:ro" \
        -v "$DEV_HOME/certs:$DEV_HOME/certs:ro" \
        -v "$DEV_HOME/acme:$DEV_HOME/acme:ro" \
        "$IMAGE" >/dev/null

    cat <<MSG
Nginx is running as container "$NAME".
  HTTP:  http://127.0.0.1:$HTTP_PORT
  HTTPS: https://127.0.0.1:$HTTPS_PORT

Point liman at it in this shell:

  export LIMAN_HOME=$DEV_HOME

Then:

  ./bin/liman deploy examples/whoami.yaml
  curl http://whoami.localhost:$HTTP_PORT
MSG
}

case "${1:-}" in
    start) start ;;
    stop) stop ;;
    *) echo "usage: $0 start|stop" >&2; exit 2 ;;
esac
