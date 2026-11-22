#!/bin/sh
# Downloads limand from a liman release, checks it, and installs it on this
# server as a systemd service:
#
#   curl -fsSL https://github.com/onrcanogul/liman/releases/latest/download/get.sh | sudo sh -s -- --listen 10.8.0.1:7070
#
# or, read before it runs:
#
#   curl -fsSLO https://github.com/onrcanogul/liman/releases/latest/download/get.sh
#   less get.sh
#   sudo sh get.sh --listen 10.8.0.1:7070
#
# It takes limand-linux-amd64 or -arm64, as this machine is, and SHA256SUMS
# from the latest release (--version v0.1.3 for another), into a directory
# of its own that is removed afterwards. It installs nothing unless the
# checksum matches, and, when gh is logged in here, the release workflow's
# attestation. Then "limand install --binary <the file>" does the work, with
# every other flag as given: --engine, --proxy, --listen, --hooks-listen,
# --no-hooks, --yes; see the README, Installing on a server. Without them,
# in a terminal, it asks; piped to sh, the questions are read from the
# terminal. Without a terminal (cloud-init, Ansible) flags decide.
#
# All of it is in main, called on the last line: a download cut short runs
# nothing.
set -eu

REPO=onrcanogul/liman
# Another place laid out like GitHub's releases (latest/download/<file>,
# download/<tag>/<file>); the end-to-end test serves one of its own.
RELEASES=${LIMAN_RELEASES:-https://github.com/$REPO/releases}

usage() {
    cat >&2 <<'EOF'
usage: curl -fsSL https://github.com/onrcanogul/liman/releases/latest/download/get.sh | sudo sh -s -- [--version <tag>] [limand install flags]

  --version <tag>   the release to install, e.g. v0.1.3; the latest without it

Every other flag goes to limand install unchanged: [--listen <address:port>]
[--engine docker|podman] [--proxy nginx|caddy|traefik|none]
[--hooks-listen <address:port> | --no-hooks] [--yes]. Run
"limand install --help" for what each does.
EOF
    exit "${1:-2}"
}

say() { printf '%s\n' "$*"; }
die() {
    printf 'get.sh: %s\n' "$*" >&2
    exit 1
}

# fetch URL FILE. A redirect may only go to https (GitHub sends downloads
# on to its own storage).
fetch() {
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL --proto-redir '=https' -o "$2" "$1"
    elif command -v wget >/dev/null 2>&1; then
        wget -q -O "$2" "$1"
    else
        die "needs curl or wget to download"
    fi
}

sha256() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{ print $1 }'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | awk '{ print $1 }'
    else
        die "needs sha256sum (coreutils) to check the download"
    fi
}

main() {
    version=
    n=$#
    # Take get.sh's own flags out, keep the rest in order.
    while [ "$n" -gt 0 ]; do
        case "$1" in
            --version)
                [ "$n" -ge 2 ] || usage
                version=$2; shift 2; n=$((n - 2)) ;;
            --version=*)
                version=${1#--version=}; shift; n=$((n - 1)) ;;
            --binary|--binary=*)
                die "get.sh downloads the binary itself; to install a file you have: sudo ./limand-linux-amd64 install" ;;
            -h|--help) usage 0 ;;
            *)
                set -- "$@" "$1"; shift; n=$((n - 1)) ;;
        esac
    done
    if [ -n "$version" ]; then
        case "$version" in
            v[0-9]*) ;;
            *) die "--version takes a release's tag, such as v0.1.3" ;;
        esac
        case "$version" in
            *[!A-Za-z0-9._-]*) die "--version $version is not a tag" ;;
        esac
    fi

    os=$(uname -s)
    [ "$os" = Linux ] ||
        die "limand runs on Linux servers; this is $os. The CLI for this machine is in the release too: https://github.com/$REPO/releases"
    machine=$(uname -m)
    case "$machine" in
        x86_64|amd64) arch=amd64 ;;
        aarch64|arm64) arch=arm64 ;;
        *) die "no limand is released for $machine, only for amd64 and arm64; build one from source with GOARCH set" ;;
    esac
    [ "$(id -u)" -eq 0 ] ||
        die "installing needs root: ... | sudo sh -s -- $*"

    if [ -n "$version" ]; then
        base=$RELEASES/download/$version
    else
        base=$RELEASES/latest/download
    fi
    file=limand-linux-$arch

    tmp=$(mktemp -d)
    trap 'rm -rf "$tmp"' EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM HUP

    say "Downloading $file and SHA256SUMS from $base"
    if ! fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS"; then
        [ -n "$version" ] || die "no SHA256SUMS at $base: no release is marked latest; pick one with --version <tag> (https://github.com/$REPO/releases)"
        die "no SHA256SUMS at $base: is $version a published release?"
    fi
    fetch "$base/$file" "$tmp/$file" || die "could not download $base/$file"

    want=$(awk -v f="$file" '$2 == f || $2 == "*" f { print $1 }' "$tmp/SHA256SUMS")
    [ -n "$want" ] || die "SHA256SUMS does not list $file; nothing was installed"
    got=$(sha256 "$tmp/$file")
    [ "$got" = "$want" ] ||
        die "$file does not match SHA256SUMS (got $got, listed $want); nothing was installed"
    say "Checksum: matches SHA256SUMS"

    # The checksum proves the file is the one listed next to it; the
    # attestation, signed by GitHub's builder, that this repository's
    # release workflow built it.
    if ! command -v gh >/dev/null 2>&1; then
        say "Attestation: not checked, gh is not installed"
    elif ! gh auth status >/dev/null 2>&1; then
        say "Attestation: not checked, gh is not logged in as $(id -un)"
    elif out=$(gh attestation verify "$tmp/$file" --repo "$REPO" 2>&1); then
        say "Attestation: built by $REPO's release workflow"
    else
        printf '%s\n' "$out" >&2
        die "the attestation of $file does not check out; nothing was installed"
    fi

    chmod 0755 "$tmp/$file"
    "$tmp/$file" version >"$tmp/version" 2>&1 ||
        die "$file does not run here; if $tmp is on a noexec mount, set TMPDIR to a directory that allows running files"
    head -1 "$tmp/version"
    say ""

    # limand install asks only when its input is a terminal. Piped to sh,
    # the input is the pipe, so the questions are read from the terminal
    # itself, if there is one and the output goes to it.
    status=0
    if [ ! -t 0 ] && [ -t 1 ] && (: </dev/tty) 2>/dev/null; then
        "$tmp/$file" install --binary "$tmp/$file" "$@" </dev/tty || status=$?
    else
        "$tmp/$file" install --binary "$tmp/$file" "$@" || status=$?
    fi
    exit "$status"
}

main "$@"
