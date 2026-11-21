#!/bin/sh
# Installs, or upgrades, the liman daemon on a server as a systemd service.
#
#   sudo ./install.sh --binary ./limand-linux-amd64 --listen 10.8.0.1:7070
#
# The work is done by the binary itself, "limand install"; this script is
# kept for the command lines written for it, and passes every other flag
# on. Without flags, in a terminal, limand install looks at the server and
# asks: the container engine (Docker or Podman), the proxy (Nginx, Caddy,
# Traefik or none), the API's address, the trigger endpoint. See
#
#   ./limand-linux-amd64 install --help     or the README, Installing on a server
set -eu

usage() {
    echo "usage: sudo $0 --binary <path to limand> [--listen <address:port>] [--engine docker|podman] [--proxy nginx|caddy|traefik|none] [--hooks-listen <address:port> | --no-hooks] [--no-nginx] [--yes]" >&2
    exit 2
}

BINARY=
n=$#
# Take --binary out, keep the rest in order.
while [ "$n" -gt 0 ]; do
    case "$1" in
        --binary)
            [ "$n" -ge 2 ] || usage
            BINARY=$2; shift 2; n=$((n - 2)) ;;
        -h|--help) usage ;;
        *)
            set -- "$@" "$1"; shift; n=$((n - 1)) ;;
    esac
done
[ -n "$BINARY" ] || usage
[ -f "$BINARY" ] || { echo "install: $BINARY does not exist" >&2; exit 1; }
[ -x "$BINARY" ] || chmod +x "$BINARY"

exec "$BINARY" install --binary "$BINARY" "$@"
