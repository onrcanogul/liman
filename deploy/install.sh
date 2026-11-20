#!/bin/sh
# Installs the liman daemon on a server as a systemd service.
#
#   sudo ./install.sh --binary ./limand-linux-amd64 --listen 10.8.0.1:7070
#
# What it does, in order. Every step can be run again: nothing that already
# exists is overwritten, except the binary and the unit file.
#
#   1. creates the system user "liman" (in the docker group)
#   2. copies the binary to /usr/local/bin/limand
#   3. writes /var/lib/liman/config.yaml, unless there is one
#   4. creates /etc/nginx/liman.d and makes Nginx include it
#   5. lets the liman user test and reload Nginx through sudo, nothing else
#   6. installs and starts the systemd unit
#   7. schedules a daily backup of the database into /var/backups/liman
#
# Requirements: Ubuntu or Debian with systemd, Docker and (unless --no-nginx
# is given) Nginx installed. The script installs no packages.
set -eu

BINARY=
LISTEN=
NGINX=yes
STATE=/var/lib/liman
BACKUPS=/var/backups/liman
CONF_DIR=/etc/nginx/liman.d
HERE=$(cd "$(dirname "$0")" && pwd)
# Set once this run has written config.yaml, so a failed first start can
# take it back and the next run starts clean.
WROTE_CONFIG=no

usage() {
    echo "usage: sudo $0 --binary <path to limand> --listen <private address:port> [--no-nginx]" >&2
    exit 2
}

while [ $# -gt 0 ]; do
    case "$1" in
        --binary) [ $# -ge 2 ] || usage; BINARY=$2; shift 2 ;;
        --listen) [ $# -ge 2 ] || usage; LISTEN=$2; shift 2 ;;
        --no-nginx) NGINX=no; shift ;;
        *) usage ;;
    esac
done
[ -n "$BINARY" ] && [ -n "$LISTEN" ] || usage

fail() { echo "install: $*" >&2; exit 1; }
step() { echo "==> $*"; }

[ "$(id -u)" -eq 0 ] || fail "run this as root (sudo)"
[ -f "$BINARY" ] || fail "$BINARY does not exist"
for unit in limand.service limand-backup.service limand-backup.timer; do
    [ -f "$HERE/$unit" ] || fail "$unit must be next to this script"
done
command -v systemctl >/dev/null || fail "systemd is required"
command -v docker >/dev/null || fail "Docker is not installed"
getent group docker >/dev/null || fail "there is no docker group; is Docker installed from a package?"
if [ "$NGINX" = yes ]; then
    command -v nginx >/dev/null || fail "Nginx is not installed (or pass --no-nginx)"
    command -v sudo >/dev/null || fail "sudo is required for reloading Nginx"
    [ -d /etc/nginx/conf.d ] || fail "/etc/nginx/conf.d does not exist; this Nginx is laid out differently"
fi

# Run the binary before anything is changed: a file built for another
# architecture fails here, with a clear message, instead of in the unit.
"$BINARY" version >/dev/null 2>&1 || fail "$BINARY does not run on this machine (wrong architecture?)"

step "User liman"
if id liman >/dev/null 2>&1; then
    echo "    exists"
else
    # --system: no password, no login, a low uid. Its home is the state
    # directory, so that "docker login" run as this user stores registry
    # credentials where the daemon looks for them.
    useradd --system --home-dir "$STATE" --shell /usr/sbin/nologin --groups docker liman
    echo "    created"
fi
# Also for a user that already existed.
usermod -aG docker liman

step "Binary /usr/local/bin/limand"
# install copies to a temporary name and renames, so a running daemon keeps
# its old file until it is restarted.
install -m 0755 "$BINARY" /usr/local/bin/limand
/usr/local/bin/limand version | sed 's/^/    /'

step "State directory $STATE"
install -d -m 0700 -o liman -g liman "$STATE"
if [ -f "$STATE/config.yaml" ]; then
    echo "    config.yaml exists, left as it is"
else
    {
        echo "# Written by install.sh. See the README for every setting."
        echo "daemon:"
        echo "  listen: $LISTEN"
        echo "backup:"
        echo "  dir: $BACKUPS"
        if [ "$NGINX" = yes ]; then
            echo "nginx:"
            echo "  conf_dir: $CONF_DIR"
            echo "  test_command: [sudo, -n, /usr/sbin/nginx, -t]"
            echo "  reload_command: [sudo, -n, /usr/bin/systemctl, reload, nginx]"
        fi
    } > "$STATE/config.yaml"
    chown liman:liman "$STATE/config.yaml"
    chmod 0600 "$STATE/config.yaml"
    WROTE_CONFIG=yes
    echo "    wrote config.yaml (listen: $LISTEN)"
fi

if [ "$NGINX" = yes ]; then
    step "Nginx"
    # liman owns one directory and writes one file per app into it. The
    # only change to the existing Nginx setup is one line that includes it.
    install -d -m 0755 -o liman -g liman "$CONF_DIR"
    if [ -f /etc/nginx/conf.d/liman.conf ]; then
        echo "    /etc/nginx/conf.d/liman.conf exists, left as it is"
    else
        printf '# Apps deployed by liman. Written by install.sh.\ninclude %s/*.conf;\n' "$CONF_DIR" > /etc/nginx/conf.d/liman.conf
        echo "    wrote /etc/nginx/conf.d/liman.conf"
    fi
    nginx -t >/dev/null 2>&1 || fail "nginx -t fails; fix the Nginx configuration and run this again"

    step "sudo rule /etc/sudoers.d/liman"
    # Written to a temporary file and checked first: a syntax error in a
    # sudoers file locks everybody out of sudo.
    tmp=$(mktemp)
    printf '# The liman daemon may run these two commands as root, with exactly these\n# arguments. Note what that still means: root then parses the files in\n# %s, which the liman user writes.\n' "$CONF_DIR" > "$tmp"
    printf 'liman ALL=(root) NOPASSWD: /usr/sbin/nginx -t, /usr/bin/systemctl reload nginx\n' >> "$tmp"
    visudo -cf "$tmp" >/dev/null || { rm -f "$tmp"; fail "the sudo rule did not pass visudo"; }
    install -m 0440 -o root -g root "$tmp" /etc/sudoers.d/liman
    rm -f "$tmp"
    echo "    installed"
fi

step "systemd unit"
install -m 0644 "$HERE/limand.service" /etc/systemd/system/limand.service
systemctl daemon-reload
systemctl enable limand >/dev/null 2>&1
# restart rather than start: on an upgrade the new binary must be picked up.
systemctl restart limand

# Give it a moment, then say plainly whether it stayed up.
sleep 2
if systemctl is-active --quiet limand; then
    echo "    limand is running"
else
    journalctl -u limand -n 20 --no-pager >&2 || true
    # Do not leave it restarting every five seconds.
    systemctl stop limand || true
    if [ "$WROTE_CONFIG" = yes ]; then
        rm -f "$STATE/config.yaml"
        fail "limand did not stay up; its last log lines are above. The config.yaml this run wrote was removed: fix the cause and run the script again"
    fi
    fail "limand did not stay up; its last log lines are above. Fix $STATE/config.yaml, then: systemctl restart limand"
fi

step "Daily backup"
install -d -m 0700 -o liman -g liman "$BACKUPS"
install -m 0644 "$HERE/limand-backup.service" /etc/systemd/system/limand-backup.service
install -m 0644 "$HERE/limand-backup.timer" /etc/systemd/system/limand-backup.timer
systemctl daemon-reload
# A timer is enabled and started like a service; the service it triggers
# is not enabled itself.
systemctl enable --now limand-backup.timer >/dev/null 2>&1
echo "    $BACKUPS, the newest 14 are kept"

cat <<NEXT

Done. Next:

  1. Create a token for your machine (it is shown once):
       sudo -u liman env LIMAN_HOME=$STATE limand token create laptop

  2. On your machine, in ~/.liman/client.yaml:
       url: http://$LISTEN
       token: <the token>

  3. Copy $STATE/master.key to somewhere that is not this server, once.
     It is not in the backups, and they are useless without it.

  4. The backups in $BACKUPS are on this server's disk. Copy them
     elsewhere regularly (rsync, restic, ...): a backup that dies with the
     disk it protects is only half of one.

  Logs:    journalctl -u limand -f
  Status:  systemctl status limand
  Backups: systemctl list-timers limand-backup
NEXT
