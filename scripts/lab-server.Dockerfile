# A stand-in for a small server: Ubuntu 24.04 that boots systemd and has
# Docker, Nginx and the WireGuard tools installed, as the real one would.
# Used by scripts/lab-server.sh; not meant for anything but practice.
FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive container=docker

RUN apt-get update && apt-get install -y --no-install-recommends \
        systemd systemd-sysv dbus sudo ca-certificates curl iproute2 iputils-ping \
        nginx docker.io wireguard-tools sqlite3 \
    && rm -rf /var/lib/apt/lists/* \
    # No terminals to log in on, and no login manager.
    && systemctl mask getty@tty1.service systemd-logind.service \
    # Ubuntu's sample site would answer for every host name.
    && rm -f /etc/nginx/sites-enabled/default

# systemd shuts down cleanly on this signal, not on SIGTERM.
STOPSIGNAL SIGRTMIN+3

# systemd as PID 1, exactly like on a real machine.
CMD ["/sbin/init"]
