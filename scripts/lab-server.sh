#!/bin/sh
# Runs a throwaway "server" in Docker for practising the installation:
# Ubuntu 24.04 with systemd, Docker, Nginx and WireGuard tools.
#
#   scripts/lab-server.sh start     build it, boot it, copy the installer in
#   scripts/lab-server.sh shell     a root shell on it
#   scripts/lab-server.sh stop      remove it and everything in it
#
# From the Mac the server's API is reachable on 127.0.0.1:17070 and its
# Nginx on 127.0.0.1:18081. Nothing is installed on it by this script: that
# is the exercise. "start" prints the commands.
#
# It is a container, so it differs from a real server in two ways that
# matter: it needs --privileged to run systemd and Docker inside, and its
# "private network" is Docker's bridge rather than WireGuard.
set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
NAME=liman-lab
IMAGE=liman-lab-server
API_PORT=${LIMAN_LAB_API_PORT:-17070}
HTTP_PORT=${LIMAN_LAB_HTTP_PORT:-18081}

stop() {
    # -v also removes the volumes that hold the inner Docker's images.
    docker rm -fv "$NAME" >/dev/null 2>&1 || true
}

address() {
    docker exec "$NAME" sh -c "ip -4 -o addr show eth0 | awk '{print \$4}' | cut -d/ -f1"
}

start() {
    # The binary must match the CPU the container runs on, which is the
    # CPU of the Docker host: arm64 on an Apple Silicon Mac.
    case "$(docker info --format '{{.Architecture}}')" in
        aarch64|arm64) arch=arm64 ;;
        x86_64|amd64) arch=amd64 ;;
        *) echo "unsupported Docker architecture" >&2; exit 1 ;;
    esac

    echo "==> Building liman for linux/$arch"
    (cd "$ROOT" && make release >/dev/null)

    echo "==> Building the server image (the first time takes a few minutes)"
    docker build -q -t "$IMAGE" -f "$ROOT/scripts/lab-server.Dockerfile" "$ROOT/scripts" >/dev/null

    stop
    echo "==> Booting"
    # --privileged and the cgroup mount let systemd and Docker run inside.
    # /var/lib/docker and /var/lib/containerd are volumes because Docker's
    # overlay storage cannot sit on top of a container's own overlay
    # file system.
    docker run -d --name "$NAME" --hostname lab \
        --privileged --cgroupns=host \
        -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
        -v /var/lib/docker -v /var/lib/containerd \
        --tmpfs /run --tmpfs /run/lock \
        -p "127.0.0.1:$API_PORT:7070" \
        -p "127.0.0.1:$HTTP_PORT:80" \
        "$IMAGE" >/dev/null

    # Wait until systemd has finished booting.
    tries=0
    until docker exec "$NAME" systemctl is-active --quiet docker 2>/dev/null; do
        tries=$((tries + 1))
        [ "$tries" -lt 60 ] || { echo "the server did not finish booting" >&2; exit 1; }
        sleep 1
    done

    docker exec "$NAME" mkdir -p /root/liman
    docker cp "$ROOT/deploy/." "$NAME:/root/liman/" >/dev/null
    docker cp "$ROOT/dist/limand-linux-$arch" "$NAME:/root/liman/limand" >/dev/null

    ip=$(address)
    cat <<NEXT

The server is up. Its private address is $ip.

  Get a shell on it:
    scripts/lab-server.sh shell

  Install liman (on the server):
    cd /root/liman
    ./install.sh --binary ./limand --listen $ip:7070

  Create a token (on the server), then use it from the Mac with
    url: http://127.0.0.1:$API_PORT

  An app deployed with "domain: whoami.localhost" answers on the Mac at
    curl -H 'Host: whoami.localhost' http://127.0.0.1:$HTTP_PORT/

  Remove everything:
    scripts/lab-server.sh stop
NEXT
}

case "${1:-}" in
    start) start ;;
    stop) stop; echo "Lab server removed" ;;
    shell) exec docker exec -it "$NAME" bash ;;
    *) echo "usage: $0 start|stop|shell" >&2; exit 2 ;;
esac
