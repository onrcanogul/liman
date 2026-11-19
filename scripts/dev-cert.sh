#!/bin/sh
# Creates a self-signed certificate for local HTTPS testing:
#
#   scripts/dev-cert.sh whoami.localhost
#
# The files land where liman looks for them (.dev/certs/<domain>/). Deploy
# the app again afterwards; liman then renders an HTTPS server block.
#
# Browsers and curl do not trust a self-signed certificate. That is expected
# here; real certificates come from Let's Encrypt on the server.
set -eu

DOMAIN=${1:-}
if [ -z "$DOMAIN" ]; then
    echo "usage: $0 <domain>" >&2
    exit 2
fi

ROOT=$(cd "$(dirname "$0")/.." && pwd)
DIR="$ROOT/.dev/certs/$DOMAIN"
mkdir -p "$DIR"

openssl req -x509 -newkey rsa:2048 -nodes -days 30 \
    -subj "/CN=$DOMAIN" \
    -addext "subjectAltName=DNS:$DOMAIN" \
    -keyout "$DIR/privkey.pem" \
    -out "$DIR/fullchain.pem" 2>/dev/null

# The key must be readable by the Nginx worker inside the container, which
# runs as a different user. Acceptable for a throwaway development key only.
chmod 644 "$DIR/privkey.pem" "$DIR/fullchain.pem"

echo "Created a self-signed certificate for $DOMAIN in $DIR"
echo "Deploy the app again to switch it to HTTPS."
