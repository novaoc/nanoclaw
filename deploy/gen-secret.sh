#!/bin/sh
# Generate NANOCLAW_SECRET and write it to a root-only file that the service
# reads via EnvironmentFile — so the secret is never typed, pasted, echoed,
# or committed. Run this ON the box (or wherever the service runs), once.
#
#   sudo sh deploy/gen-secret.sh [/path/to/secret.env]
#
# Default path is /etc/nanoclaw.secret. Keep this OFF the data microSD if you
# want a stolen data card to be undecryptable (see README). On a single-SD
# board, at minimum keep it out of the data dir and root-600.

set -eu
OUT="${1:-/etc/nanoclaw.secret}"

if [ -f "$OUT" ]; then
  echo "refusing to overwrite existing $OUT (delete it first to rotate)" >&2
  exit 1
fi

if command -v openssl >/dev/null 2>&1; then
  SECRET="$(openssl rand -hex 32)"
elif [ -r /dev/urandom ]; then
  SECRET="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
else
  echo "no openssl and no /dev/urandom — cannot generate a secret" >&2
  exit 1
fi

umask 077
printf 'NANOCLAW_SECRET=%s\n' "$SECRET" > "$OUT"
chmod 600 "$OUT"
# scrub the value from this shell
SECRET=""
echo "wrote $OUT (root-only). Point the service at it, e.g. add to the unit:"
echo "  EnvironmentFile=$OUT"
echo "The value was never printed. Back it up somewhere safe — losing it makes"
echo "every connected wallet unrecoverable (users just /connect again)."
