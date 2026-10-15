#!/usr/bin/env bash
# Generates every secret this stack needs and the FileBrowser config.
# Safe to re-run: anything that already exists is kept, so logins and 2FA survive.
set -euo pipefail
cd "$(dirname "$0")/.."

AUTHELIA_IMAGE="authelia/authelia:4.39"
S="authelia/secrets"

mkdir -p "$S" caddy/data caddy/config filebrowser
umask 077

for f in jwt_secret session_secret storage_key oidc_hmac; do
  if [ ! -s "$S/$f" ]; then
    openssl rand -hex 64 > "$S/$f"
    echo "created $S/$f"
  fi
done

if [ ! -s "$S/oidc_jwks.pem" ]; then
  openssl genrsa -out "$S/oidc_jwks.pem" 4096 2>/dev/null
  echo "created $S/oidc_jwks.pem"
fi

# FileBrowser <-> Authelia OIDC client secret: plain value for FileBrowser,
# pbkdf2 digest for Authelia.
if [ ! -s "$S/filebrowser_client_secret" ]; then
  openssl rand -hex 32 > "$S/filebrowser_client_secret"
  rm -f "$S/filebrowser_client_secret.digest"
  echo "created $S/filebrowser_client_secret"
fi
if [ ! -s "$S/filebrowser_client_secret.digest" ]; then
  docker run --rm "$AUTHELIA_IMAGE" authelia crypto hash generate pbkdf2 --variant sha512 \
    --password "$(cat "$S/filebrowser_client_secret")" \
    | sed -n 's/^Digest: //p' > "$S/filebrowser_client_secret.digest"
  if [ ! -s "$S/filebrowser_client_secret.digest" ]; then
    rm -f "$S/filebrowser_client_secret.digest"
    echo "error: could not hash the FileBrowser client secret with $AUTHELIA_IMAGE" >&2
    exit 1
  fi
  echo "created $S/filebrowser_client_secret.digest"
fi
chmod 600 "$S"/*

if [ ! -f filebrowser/config.yaml ]; then
  sed "s|__FILEBROWSER_CLIENT_SECRET__|$(cat "$S/filebrowser_client_secret")|" \
    filebrowser/config.yaml.example > filebrowser/config.yaml
  echo "created filebrowser/config.yaml"
fi

if [ ! -f authelia/config/users.yml ]; then
  cat <<EOF

Next: create authelia/config/users.yml from authelia/config/users.yml.example.
Hash your password with (you type it at the prompt):
  docker run --rm -it $AUTHELIA_IMAGE authelia crypto hash generate argon2
EOF
fi

cat <<'EOF'

Then:
  sudo mkdir -p /srv/files
  sudo chown -R 1000:1000 /srv/files filebrowser
  docker compose up -d --build
EOF
