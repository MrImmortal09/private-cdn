# Handoff: private file CDN on axiosiiitl.dev

You are continuing a self-hosted setup on the user's Linux server. Most of it may already be in place. Your job is to **audit what exists, fill the gaps from the repo, and verify everything end to end**, without breaking what already works.

All files live in the private GitHub repo **[MrImmortal09/private-cdn](https://github.com/MrImmortal09/private-cdn)**. Every file is also reproduced in §8, so you can work even if the server can't clone the repo.

## 1. Goal

A private file server where nothing is reachable without the user's login:

| Subdomain | Service | Purpose | Login |
| --- | --- | --- | --- |
| `auth.axiosiiitl.dev` | Authelia | Login portal: password + 2FA, single sign-on for all subdomains | — (it *is* the login) |
| `index.axiosiiitl.dev` | FileBrowser Quantum | Web UI: upload, browse, delete, make share links for guests | "Sign in with Authelia" (OIDC) |
| `cdn.axiosiiitl.dev` | `rclone serve http` | Direct URL for every file; opens `.zip` files as folders, so `/movies.zip/ep1.mp4` streams | Caddy `forward_auth` → Authelia |
| `fetch.axiosiiitl.dev` | Fetcher (small Flask app in `fetcher/`) | Paste Google Drive / Dropbox / direct links + a folder; files download onto the server | Caddy `forward_auth` → Authelia |

Caddy is the only container with published ports (80, 443). All content lives in `/srv/files` on the host. The project lives in `/opt/private-cdn`.

```
Browser ──HTTPS──> Caddy :443
                    ├── auth.axiosiiitl.dev  ──> authelia:9091
                    ├── index.axiosiiitl.dev ──> filebrowser:80   (OIDC login via Authelia)
                    ├── cdn.axiosiiitl.dev   ──[forward_auth]──> rclone:8080   (/srv/files read-only)
                    └── fetch.axiosiiitl.dev ──[forward_auth]──> fetcher:8000  (/srv/files read-write)
```

## 2. Ground rules

1. **Audit before you change anything** (§4). If a component already exists and works, keep it. Adapt to the real state rather than overwriting.
2. **Back up any file before editing it**: `cp file file.bak.$(date +%Y%m%d%H%M%S)` (the repo's `.gitignore` ignores these).
3. **Ask the user before** anything destructive or hard to undo: deleting files or containers, stopping a service that is currently serving traffic, replacing an existing reverse proxy, changing firewall rules, or reissuing certificates.
4. **Secrets never leave the server.** `scripts/setup-secrets.sh` generates them into `authelia/secrets/` (mode `600`, gitignored). Never print them in replies or logs, and never commit them. The user's login password is hashed by **the user** typing it into an interactive command (§6 step 3); you never see it.
5. **Don't guess when a check fails.** Read the container logs, fix the specific line they name, and retry. Report anything you couldn't resolve.
6. Pin versions as written. If a tag doesn't exist (`docker pull` fails), use the closest existing tag and say so in your report.
7. If you change any file in the repo, commit it with a clear message. Never commit anything from `authelia/secrets/`, `users.yml`, `filebrowser/config.yaml` or databases.

## 3. What the user has told us

- `cdn.axiosiiitl.dev`, `index.axiosiiitl.dev` and `fetch.axiosiiitl.dev` already point to this server.
- "Most of the setup" is done, but we don't know exactly which parts. Find out in §4.
- `auth.axiosiiitl.dev` was **not** mentioned. Authelia needs it (§5).
- `.dev` domains are HSTS-preloaded: browsers only use HTTPS, so every site needs a valid certificate (Caddy does this automatically).

## 4. Step 0: audit the current state

Run these and summarize the results before changing anything:

```bash
# What is running, and who owns ports 80/443?
docker ps --format 'table {{.Names}}\t{{.Image}}\t{{.Status}}\t{{.Ports}}'
sudo ss -tlnp | grep -E ':(80|443)\s'
systemctl is-active nginx apache2 caddy 2>/dev/null

# Existing project files (is it already a checkout of the repo?)
ls -la /opt/private-cdn 2>/dev/null && git -C /opt/private-cdn remote -v 2>/dev/null
find /opt/private-cdn -maxdepth 3 -not -path '*/caddy/data/*' -not -path '*/.git/*' 2>/dev/null | sort
ls -la /srv/files 2>/dev/null | head

# Docker + Compose versions
docker --version && docker compose version

# DNS: all four names must return this server's public IP
curl -s https://ifconfig.me; echo
for h in auth index cdn fetch; do printf '%-6s ' $h; dig +short $h.axiosiiitl.dev A | tr '\n' ' '; echo; done

# What is currently served on each name (if anything)
for h in auth index cdn fetch; do printf '%-6s ' $h; curl -s -o /dev/null -w '%{http_code} -> %{redirect_url}\n' https://$h.axiosiiitl.dev/ ; done
```

Decide from the results:

- **Another web server (nginx, Apache, a different Caddy) holds 80/443:** stop and ask the user whether to replace it with the Caddy container here or to add these sites to the existing proxy. Don't stop it on your own.
- **`/opt/private-cdn` exists but isn't a checkout of the repo:** don't clone over it. Clone to `/opt/private-cdn-repo`, diff the two, and bring over only what's missing. Keep existing secrets, users and databases (`authelia/config/db.sqlite3`, `users.yml`, `authelia/secrets/*`, `filebrowser/*.db`); regenerating them logs the user out and wipes 2FA devices.
- **Cloudflare in front** (the DNS answers are Cloudflare IPs rather than the server's): Caddy's certificate challenge needs the zone's SSL mode set to **Full (strict)**, and heavy video through Cloudflare's proxy may break their terms. Recommend `cdn` be DNS-only (grey cloud). Ask before assuming.

## 5. The `auth` subdomain

Authelia needs its own address for its login page and as the OIDC issuer.

- **Preferred:** the user adds an A record `auth.axiosiiitl.dev` → the server's IP. You can't do this yourself; ask the user, then wait until `dig +short auth.axiosiiitl.dev` returns the server IP.
- **If the user can't add DNS records:** Authelia can run on a subpath (`https://index.axiosiiitl.dev/authelia`). That needs `server.address: 'tcp://:9091/authelia'`, `authelia_url: 'https://index.axiosiiitl.dev/authelia'`, FileBrowser's `issuerUrl` set to `https://index.axiosiiitl.dev/authelia`, and a Caddy `handle /authelia*` block on the `index` site before the FileBrowser proxy, as in [Authelia's Caddy subpath example](https://www.authelia.com/integration/proxies/caddy/#subpath). Only use this if the user confirms they can't add the record.

## 6. Build steps (only the ones the audit shows are missing)

1. **Get the files**
   ```bash
   sudo mkdir -p /opt/private-cdn /srv/files && sudo chown "$USER" /opt/private-cdn
   git clone https://github.com/MrImmortal09/private-cdn /opt/private-cdn
   cd /opt/private-cdn
   ```
   The repo is private. If the server has no GitHub access, ask the user to run `gh auth login` on the server or add a read-only deploy key, or recreate the files from §8.
2. **Secrets and FileBrowser config** (idempotent: keeps anything that already exists):
   ```bash
   ./scripts/setup-secrets.sh
   ```
   It creates `authelia/secrets/*` (random keys, the OIDC signing key, the FileBrowser client secret and its pbkdf2 digest) and writes `filebrowser/config.yaml` from the example with the client secret filled in. Authelia reads the digest via the `{{ secret … }}` template in its config, so no config edits are needed.
3. **The user's login.** Ask the user for a username, display name and email. Then have **the user** run this and type their password at the prompt:
   ```bash
   docker run --rm -it authelia/authelia:4.39 authelia crypto hash generate argon2
   ```
   Create `authelia/config/users.yml` from `users.yml.example` with the printed `Digest`, then `chmod 600 authelia/config/users.yml`. If `users.yml` already exists, leave it alone.
4. **Ownership**
   ```bash
   sudo chown -R 1000:1000 /srv/files /opt/private-cdn/filebrowser
   ```
5. **Firewall** (ask first; make sure SSH stays allowed):
   ```bash
   sudo ufw allow 22/tcp && sudo ufw allow 80/tcp && sudo ufw allow 443/tcp && sudo ufw allow 443/udp
   sudo ufw status   # enable only after confirming with the user: sudo ufw enable
   ```
6. **Start**
   ```bash
   docker compose pull && docker compose up -d --build
   docker compose ps
   docker compose logs --tail=80 caddy authelia filebrowser rclone fetcher
   ```
7. **2FA enrolment** (user does this in a browser): open `https://auth.axiosiiitl.dev`, sign in, register an authenticator app or passkey. Authelia writes the one-time verification code to `authelia/config/notification.txt`; read it with `sudo tail -n 20 authelia/config/notification.txt` and give the user the code.
8. **Lock FileBrowser to Authelia.** Once "Sign in with Authelia" works on `index.axiosiiitl.dev`, set `auth.methods.password.enabled: false` in `filebrowser/config.yaml` and run `docker compose restart filebrowser`. (It ships enabled so the user isn't locked out mid-setup.)
9. **Run the acceptance tests in §7.**

## 7. Acceptance tests

All must pass. Report each result.

```bash
cd /opt/private-cdn

# 1. Containers healthy
docker compose ps

# 2. Authelia up, OIDC issuer correct
curl -s https://auth.axiosiiitl.dev/api/health; echo
curl -s https://auth.axiosiiitl.dev/.well-known/openid-configuration | grep -o '"issuer":"[^"]*"'

# 3. Gate works: both must redirect (302/303) to auth.axiosiiitl.dev, never return content
curl -s -o /dev/null -w 'cdn   %{http_code} -> %{redirect_url}\n' https://cdn.axiosiiitl.dev/
curl -s -o /dev/null -w 'fetch %{http_code} -> %{redirect_url}\n' https://fetch.axiosiiitl.dev/

# 4. FileBrowser answers
curl -s -o /dev/null -w 'index %{http_code}\n' https://index.axiosiiitl.dev/

# 5. Internal services answer (from inside the Docker network)
docker compose exec caddy wget -qO- http://fetcher:8000/healthz; echo
docker compose exec rclone rclone version | head -1        # must be v1.72 or newer (archive backend)
docker compose exec fetcher python -c "import gdown; print('gdown', gdown.__version__)"

# 6. Zip browsing: a stored (uncompressed) zip must list as a folder
mkdir -p /tmp/ziptest && head -c 2000000 /dev/urandom > /tmp/ziptest/clip.mp4
(cd /tmp/ziptest && zip -0 -q test.zip clip.mp4) && sudo cp /tmp/ziptest/test.zip /srv/files/ && sudo chown 1000:1000 /srv/files/test.zip
docker compose exec caddy wget -qO- http://rclone:8080/test.zip/ | grep -o 'clip.mp4' | head -1
```

Then ask the user to check in a browser (a private window, signed out):

1. `https://cdn.axiosiiitl.dev/test.zip/clip.mp4` → Authelia login → after password + 2FA, the file downloads.
2. `https://index.axiosiiitl.dev` → "Sign in with Authelia" works without asking again; `test.zip` is listed.
3. In FileBrowser, create a share link with an expiry; it opens in another private window **without** login.
4. `https://fetch.axiosiiitl.dev` → paste a public Google Drive file link and a direct `.zip` link into folder `fetch-test`; both finish and show working `cdn.` links. (The Drive path uses `gdown` and has not been tested against Google before this.)

Clean up afterwards: `sudo rm -f /srv/files/test.zip` and remove `fetch-test` (ask first).

## 8. Files in the repo

Reference copies; the repo is the source of truth.

### `docker-compose.yml`

```yaml
services:
  caddy:
    image: caddy:2
    restart: unless-stopped
    ports:
      - "80:80"
      - "443:443"
      - "443:443/udp"
    volumes:
      - ./caddy/Caddyfile:/etc/caddy/Caddyfile:ro
      - ./caddy/data:/data
      - ./caddy/config:/config

  authelia:
    image: authelia/authelia:4.39
    restart: unless-stopped
    environment:
      X_AUTHELIA_CONFIG_FILTERS: template
      AUTHELIA_IDENTITY_VALIDATION_RESET_PASSWORD_JWT_SECRET_FILE: /secrets/jwt_secret
      AUTHELIA_SESSION_SECRET_FILE: /secrets/session_secret
      AUTHELIA_STORAGE_ENCRYPTION_KEY_FILE: /secrets/storage_key
      AUTHELIA_IDENTITY_PROVIDERS_OIDC_HMAC_SECRET_FILE: /secrets/oidc_hmac
    volumes:
      - ./authelia/config:/config
      - ./authelia/secrets:/secrets:ro

  filebrowser:
    image: ghcr.io/gtsteffaniak/filebrowser:2.0-stable
    restart: unless-stopped
    user: "1000:1000"
    volumes:
      - /srv/files:/srv
      - ./filebrowser:/home/filebrowser/data

  rclone:
    image: rclone/rclone:latest
    restart: unless-stopped
    user: "1000:1000"
    command: serve http ":archive:/data" --addr :8080 --read-only
    volumes:
      - /srv/files:/data:ro

  fetcher:
    build: ./fetcher
    restart: unless-stopped
    environment:
      CDN_BASE: https://cdn.axiosiiitl.dev
    volumes:
      - /srv/files:/data
```

### `caddy/Caddyfile`

`index` deliberately has **no** `forward_auth`: FileBrowser logs in through Authelia itself (OIDC), and guest share links must stay reachable without an account.

```text
# Every site that imports this snippet requires an Authelia login.
(authelia) {
	forward_auth authelia:9091 {
		uri /api/authz/forward-auth
		copy_headers Remote-User Remote-Groups Remote-Email Remote-Name
	}
}

# Login portal and OIDC issuer.
auth.axiosiiitl.dev {
	reverse_proxy authelia:9091
}

# FileBrowser Quantum. No forward_auth here on purpose: FileBrowser signs you in
# through Authelia itself (OIDC), and guest share links must work without an account.
index.axiosiiitl.dev {
	reverse_proxy filebrowser:80
}

# Direct file links, including files inside .zip archives.
cdn.axiosiiitl.dev {
	import authelia
	reverse_proxy rclone:8080
}

# Paste-a-link downloader.
fetch.axiosiiitl.dev {
	import authelia
	reverse_proxy fetcher:8000
}
```

### `authelia/config/configuration.yml`

The OIDC client follows [Authelia's FileBrowser Quantum guide](https://www.authelia.com/integration/openid-connect/clients/filebrowser-quantum/) (tested there with Authelia 4.39.28). The `{{ secret … }}` lines need `X_AUTHELIA_CONFIG_FILTERS=template`, which the Compose file sets.

```yaml
# Authelia configuration. Secrets are not in this file: they are read from
# authelia/secrets/ (see docker-compose.yml and scripts/setup-secrets.sh).
server:
  address: 'tcp://:9091'

log:
  level: 'info'

totp:
  issuer: 'axiosiiitl.dev'

authentication_backend:
  file:
    path: '/config/users.yml'

access_control:
  default_policy: 'deny'
  rules:
    - domain:
        - 'cdn.axiosiiitl.dev'
        - 'fetch.axiosiiitl.dev'
      policy: 'two_factor'

session:
  cookies:
    - domain: 'axiosiiitl.dev'
      authelia_url: 'https://auth.axiosiiitl.dev'
      default_redirection_url: 'https://index.axiosiiitl.dev'

storage:
  local:
    path: '/config/db.sqlite3'

# Password-reset and 2FA-enrolment messages are written here instead of emailed.
notifier:
  filesystem:
    filename: '/config/notification.txt'

identity_providers:
  oidc:
    jwks:
      - key: {{ secret "/secrets/oidc_jwks.pem" | mindent 10 "|" | msquote }}
    clients:
      - client_id: 'filebrowser'
        client_name: 'FileBrowser Quantum'
        client_secret: {{ secret "/secrets/filebrowser_client_secret.digest" | msquote }}
        public: false
        authorization_policy: 'two_factor'
        require_pkce: false
        pkce_challenge_method: ''
        redirect_uris:
          - 'https://index.axiosiiitl.dev/api/auth/oidc/callback'
        scopes: ['openid', 'profile', 'groups', 'email']
        response_types: ['code']
        grant_types: ['authorization_code']
        access_token_signed_response_alg: 'none'
        userinfo_signed_response_alg: 'none'
        token_endpoint_auth_method: 'client_secret_basic'
```

### `authelia/config/users.yml.example`

```yaml
# Copy to users.yml (gitignored) and fill in.
# Hash your password interactively; you type it, nothing else sees it:
#   docker run --rm -it authelia/authelia:4.39 authelia crypto hash generate argon2
# Paste the printed Digest below. Members of 'admin' are admins in FileBrowser.
users:
  yourname:
    displayname: 'Your Name'
    password: '$argon2id$v=19$m=65536,t=3,p=4$REPLACE_WITH_YOUR_DIGEST'
    email: 'you@example.com'
    groups:
      - 'admin'
```

### `filebrowser/config.yaml.example`

Quantum validates its config strictly: an unknown or misplaced key stops the container and the log names it. Key names match the project's [default config](https://github.com/gtsteffaniak/filebrowser/blob/main/frontend/public/config.generated.yaml) as of October 2026; if the running version complains, check that file.

```yaml
# scripts/setup-secrets.sh copies this to config.yaml (gitignored) and fills in
# the OIDC client secret. FileBrowser Quantum validates this file strictly: an
# unknown or misplaced key stops the container, and the log names the key.
server:
  cacheDir: '/home/filebrowser/data/tmp'
  sources:
    - path: '/srv'
      name: 'Files'
      config:
        defaultEnabled: true

http:
  port: 80
  baseURL: '/'
  externalUrl: 'https://index.axiosiiitl.dev'
  trustProxyHeaders: true

auth:
  methods:
    password:
      # Leave enabled until "Sign in with Authelia" works, then set to false.
      enabled: true
      signup: false
    oidc:
      enabled: true
      clientId: 'filebrowser'
      clientSecret: '__FILEBROWSER_CLIENT_SECRET__'
      issuerUrl: 'https://auth.axiosiiitl.dev'
      scopes: 'openid email profile groups'
      userIdentifier: 'preferred_username'
      groupsClaim: 'groups'
      adminGroup: 'admin'

userDefaults:
  account:
    permissions:
      share: true
```

### `scripts/setup-secrets.sh`

```bash
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
```

### `fetcher/Dockerfile`

```dockerfile
FROM python:3.12-slim

RUN pip install --no-cache-dir "flask>=3.0" "requests>=2.32" "gdown>=5.2" "gunicorn>=22"

WORKDIR /app
COPY app.py .

ENV DATA_DIR=/data
USER 1000:1000
EXPOSE 8000

# One worker so all downloads share one job list; threads handle the page and downloads.
CMD ["gunicorn", "--workers", "1", "--threads", "16", "--bind", "0.0.0.0:8000", "app:app"]
```

### `fetcher/app.py`

Tested for direct downloads, duplicate names (saved as `name (1).ext`), folder-escape attempts (`../`), cancelling (partial `.part` file removed) and UTF-8 file names. Environment: `DATA_DIR` (default `/data`), `CDN_BASE` (prefix for links shown after a download), `MAX_PARALLEL` (default 3), `ALLOW_PRIVATE_HOSTS=1` (testing only; disables the block on localhost/private-IP links).

```python
"""Fetcher: paste a link, and the file is saved into your files folder.

Handles Google Drive file and folder links (through gdown), Dropbox share
links, and any direct download link. Runs behind Caddy + Authelia, so it has
no login of its own.
"""
import ipaddress
import os
import re
import socket
import threading
import time
import uuid
from email.message import Message
from pathlib import Path
from urllib.parse import parse_qs, quote, unquote, urlencode, urljoin, urlparse, urlunparse

import requests
from flask import Flask, jsonify, redirect, render_template_string, request

DATA = Path(os.environ.get("DATA_DIR", "/data")).resolve()
CDN_BASE = os.environ.get("CDN_BASE", "").rstrip("/")
ALLOW_PRIVATE = os.environ.get("ALLOW_PRIVATE_HOSTS") == "1"
MAX_PARALLEL = int(os.environ.get("MAX_PARALLEL", "3"))
CHUNK = 1024 * 1024
UA = "Mozilla/5.0 (X11; Linux x86_64) Fetcher/1.0"

app = Flask(__name__)
jobs = {}
jobs_lock = threading.Lock()
slots = threading.Semaphore(MAX_PARALLEL)

DRIVE_HOSTS = {"drive.google.com", "docs.google.com", "drive.usercontent.google.com"}
DRIVE_ID = re.compile(r"(?:/file/d/|/folders/|/d/|[?&]id=)([A-Za-z0-9_-]{20,})")


class FetchError(Exception):
    pass


class Cancelled(Exception):
    pass


# ---------- paths and names ----------

def target_dir(folder: str) -> Path:
    """Resolve a folder under DATA, refusing anything that escapes it."""
    path = (DATA / folder.strip().strip("/")).resolve()
    if path != DATA and DATA not in path.parents:
        raise FetchError("The folder must stay inside your files folder.")
    path.mkdir(parents=True, exist_ok=True)
    return path


def clean_name(name: str) -> str:
    name = unquote(name).replace(chr(92), "/").rsplit("/", 1)[-1].strip().strip('"')
    name = "".join("_" if ord(c) < 32 or c in '<>:"|?*' else c for c in name)
    if name in ("", ".", ".."):
        name = "download"
    return name[:200]


def free_path(path: Path) -> Path:
    """Never overwrite: add (1), (2) … when the name is taken."""
    if not path.exists() and not path.with_name(path.name + ".part").exists():
        return path
    for i in range(1, 10_000):
        candidate = path.with_name(f"{path.stem} ({i}){path.suffix}")
        if not candidate.exists() and not candidate.with_name(candidate.name + ".part").exists():
            return candidate
    raise FetchError("Too many files with that name.")


def cdn_link(path: Path) -> str:
    rel = path.relative_to(DATA).as_posix()
    return f"{CDN_BASE}/{quote(rel)}" if CDN_BASE else ""


# ---------- link handling ----------

def normalize(url: str) -> str:
    """Turn share-page links into direct-download links where we know how."""
    parsed = urlparse(url)
    host = (parsed.hostname or "").lower()
    if host.endswith("dropbox.com"):
        query = parse_qs(parsed.query)
        query["dl"] = ["1"]
        return urlunparse(parsed._replace(query=urlencode(query, doseq=True)))
    return url


def is_drive(url: str) -> bool:
    return (urlparse(url).hostname or "").lower() in DRIVE_HOSTS


def drive_id(url: str) -> str:
    match = DRIVE_ID.search(url)
    if not match:
        raise FetchError("Couldn't find the file ID in that Google Drive link.")
    return match.group(1)


def check_host(url: str) -> None:
    """Block links to the server itself or the private network."""
    parsed = urlparse(url)
    if parsed.scheme not in ("http", "https") or not parsed.hostname:
        raise FetchError("Only http and https links are supported.")
    if ALLOW_PRIVATE:
        return
    try:
        infos = socket.getaddrinfo(parsed.hostname, parsed.port or 443)
    except socket.gaierror:
        raise FetchError(f"Couldn't find the host {parsed.hostname}.")
    for info in infos:
        if not ipaddress.ip_address(info[4][0]).is_global:
            raise FetchError("Links to private or local addresses are blocked.")


def name_from_response(resp: requests.Response, url: str) -> str:
    raw = resp.headers.get("Content-Disposition", "")
    if "filename*" in raw.lower():  # prefer the UTF-8 name when both are given
        raw = "attachment;" + ";".join(p for p in raw.split(";") if "filename*" in p.lower())
    header = Message()
    header["content-disposition"] = raw
    filename = header.get_filename()
    return clean_name(filename or urlparse(url).path)


# ---------- downloaders ----------

def fetch_direct(job: dict, url: str, folder: Path) -> None:
    session = requests.Session()
    session.headers["User-Agent"] = UA
    for _ in range(10):  # follow redirects by hand so every hop is checked
        check_host(url)
        resp = session.get(url, stream=True, allow_redirects=False, timeout=30)
        if resp.is_redirect:
            url = urljoin(url, resp.headers["Location"])
            resp.close()
            continue
        break
    else:
        raise FetchError("Too many redirects.")

    if resp.status_code >= 400:
        raise FetchError(f"The server answered {resp.status_code} {resp.reason}.")
    if "text/html" in resp.headers.get("Content-Type", "") and not job["name"]:
        resp.close()
        raise FetchError("That link opens a web page, not a file. Copy the direct download link instead.")

    final = free_path(folder / (job["name"] or name_from_response(resp, url)))
    part = final.with_name(final.name + ".part")
    job.update(total=int(resp.headers.get("Content-Length") or 0), file=final.name)
    try:
        with open(part, "wb") as out:
            for chunk in resp.iter_content(CHUNK):
                if job["cancel"]:
                    raise Cancelled()
                out.write(chunk)
                job["done"] += len(chunk)
        part.rename(final)
    except BaseException:
        part.unlink(missing_ok=True)
        raise
    job["saved"] = [final]


def fetch_drive(job: dict, url: str, folder: Path) -> None:
    import gdown  # installed in the container; loaded only when needed

    if "/folders/" in url:
        job["file"] = "Google Drive folder"
        files = gdown.download_folder(url=url, output=str(folder), quiet=True, use_cookies=False)
        if not files:
            raise FetchError("Google Drive returned no files. Is the folder shared with anyone who has the link?")
        saved = [Path(f).resolve() for f in files]
    else:
        file_id = drive_id(url)
        output = str(free_path(folder / job["name"])) if job["name"] else str(folder) + os.sep
        job["file"] = job["name"] or "Google Drive file"
        result = gdown.download(id=file_id, output=output, quiet=True, use_cookies=False)
        if not result:
            raise FetchError("Google Drive refused the download. Is the file shared with anyone who has the link?")
        saved = [Path(result).resolve()]

    for path in saved:  # make sure nothing landed outside DATA
        if DATA not in path.parents:
            path.unlink(missing_ok=True)
            raise FetchError("Google Drive returned an unsafe file name.")
    job["file"] = saved[0].name if len(saved) == 1 else f"{len(saved)} files"
    job["saved"] = saved


def run(job: dict) -> None:
    with slots:
        job["status"] = "downloading"
        job["started"] = time.time()
        try:
            folder = target_dir(job["folder"])
            url = normalize(job["url"])
            if is_drive(url):
                fetch_drive(job, url, folder)
            else:
                fetch_direct(job, url, folder)
            job["links"] = [cdn_link(p) for p in job["saved"] if p.is_file()][:20]
            job["status"] = "done"
        except Cancelled:
            job["status"] = "cancelled"
        except FetchError as err:
            job.update(status="failed", error=str(err))
        except Exception as err:  # network errors, gdown errors, disk full …
            job.update(status="failed", error=f"{type(err).__name__}: {err}"[:300])
        finally:
            job["finished"] = time.time()


def add_job(url: str, folder: str, name: str) -> dict:
    job = {
        "id": uuid.uuid4().hex[:10], "url": url, "folder": folder, "name": clean_name(name) if name else "",
        "status": "queued", "file": "", "done": 0, "total": 0, "error": "", "links": [],
        "saved": [], "cancel": False, "created": time.time(), "started": 0, "finished": 0,
    }
    with jobs_lock:
        jobs[job["id"]] = job
    threading.Thread(target=run, args=(job,), daemon=True).start()
    return job


# ---------- web ----------

def existing_folders(limit: int = 300) -> list[str]:
    found = []
    for root, dirs, _ in os.walk(DATA):
        dirs[:] = sorted(d for d in dirs if not d.startswith("."))
        rel = Path(root).relative_to(DATA)
        if rel.parts and len(rel.parts) <= 3:
            found.append(rel.as_posix())
        if len(rel.parts) >= 3:
            dirs[:] = []
        if len(found) >= limit:
            break
    return found


@app.get("/")
def index():
    return render_template_string(PAGE, folders=existing_folders())


@app.post("/add")
def add():
    urls = [u.strip() for u in request.form.get("urls", "").splitlines() if u.strip()]
    folder = request.form.get("folder", "")
    name = request.form.get("name", "").strip() if len(urls) == 1 else ""
    for url in urls[:50]:
        add_job(url, folder, name)
    return redirect("/")


@app.get("/jobs")
def list_jobs():
    with jobs_lock:
        items = sorted(jobs.values(), key=lambda j: j["created"], reverse=True)
        public = [{k: v for k, v in j.items() if k not in ("saved", "cancel")} for j in items]
    return jsonify(public)


@app.post("/cancel/<job_id>")
def cancel(job_id):
    if job_id in jobs:
        jobs[job_id]["cancel"] = True
    return ("", 204)


@app.post("/clear")
def clear():
    with jobs_lock:
        for job_id in [k for k, j in jobs.items() if j["status"] in ("done", "failed", "cancelled")]:
            del jobs[job_id]
    return ("", 204)


@app.get("/healthz")
def healthz():
    return "ok"


PAGE = """<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Fetcher</title>
<style>
:root{--bg:#f7f7f5;--card:#fff;--ink:#1d1d1b;--quiet:#6b6b66;--line:#e2e1dc;--accent:#2f6fde;--bad:#c2410c;--good:#15803d}
@media (prefers-color-scheme:dark){:root{--bg:#171716;--card:#21211f;--ink:#ededea;--quiet:#a3a39d;--line:#34342f;--accent:#7aa5ff;--bad:#fb923c;--good:#4ade80}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--ink);font:15px/1.5 system-ui,sans-serif}
main{max-width:760px;margin:0 auto;padding:32px 16px}h1{font-size:22px;margin:0 0 4px}p.sub{color:var(--quiet);margin:0 0 20px}
form,.job{background:var(--card);border:1px solid var(--line);border-radius:10px}
form{padding:16px;display:grid;gap:12px}label{font-size:13px;color:var(--quiet);display:grid;gap:4px}
textarea,input{font:inherit;color:var(--ink);background:var(--bg);border:1px solid var(--line);border-radius:8px;padding:9px 10px;width:100%}
textarea{min-height:84px;resize:vertical}.row{display:grid;grid-template-columns:1fr 1fr;gap:12px}
@media (max-width:560px){.row{grid-template-columns:1fr}}
button{font:inherit;border:0;border-radius:8px;padding:10px 16px;background:var(--accent);color:#fff;cursor:pointer;justify-self:start}
button.link{background:none;color:var(--quiet);padding:0;font-size:13px;text-decoration:underline}
.head{display:flex;justify-content:space-between;align-items:baseline;margin:28px 0 10px}h2{font-size:16px;margin:0}
.job{padding:12px 14px;margin-bottom:8px}.top{display:flex;justify-content:space-between;gap:12px}
.name{font-weight:600;word-break:break-all}.meta{color:var(--quiet);font-size:13px;word-break:break-all}
.st{font-size:13px;white-space:nowrap}.done{color:var(--good)}.failed{color:var(--bad)}
.bar{height:6px;background:var(--line);border-radius:3px;margin-top:8px;overflow:hidden}.bar i{display:block;height:100%;background:var(--accent)}
.err{color:var(--bad);font-size:13px;margin-top:6px}.links a{color:var(--accent);font-size:13px;word-break:break-all;display:block}
.empty{color:var(--quiet);font-size:14px}
</style></head><body><main>
<h1>Fetcher</h1><p class="sub">Paste links and they're saved straight into your files folder.</p>
<form method="post" action="/add">
  <label>Links, one per line (Google Drive, Dropbox or direct download)
    <textarea name="urls" required placeholder="https://drive.google.com/file/d/…/view&#10;https://example.com/movies.zip"></textarea></label>
  <div class="row">
    <label>Save to folder (blank = top level)
      <input name="folder" list="folders" placeholder="movies/season1" autocomplete="off"></label>
    <label>File name (optional, single link only)
      <input name="name" placeholder="keep the original name"></label>
  </div>
  <datalist id="folders">{% for f in folders %}<option value="{{ f }}">{% endfor %}</datalist>
  <button type="submit">Download</button>
</form>
<div class="head"><h2>Downloads</h2><button class="link" onclick="clearDone()">Clear finished</button></div>
<div id="jobs"><p class="empty">Nothing yet.</p></div>
</main>
<script>
const fmt = b => { if (!b) return '0 B'; const u = ['B','KB','MB','GB','TB']; const i = Math.min(4, Math.floor(Math.log(b)/Math.log(1024))); return (b/1024**i).toFixed(i ? 1 : 0) + ' ' + u[i]; };
const esc = s => String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
async function refresh() {
  try {
    const jobs = await (await fetch('/jobs')).json();
    const box = document.getElementById('jobs');
    if (!jobs.length) { box.innerHTML = '<p class="empty">Nothing yet.</p>'; return; }
    box.innerHTML = jobs.map(j => {
      const pct = j.total ? Math.min(100, j.done / j.total * 100) : 0;
      const size = j.total ? fmt(j.done) + ' of ' + fmt(j.total) : (j.done ? fmt(j.done) : '');
      const state = j.status === 'downloading' ? (j.total ? pct.toFixed(0) + '%' : 'downloading') : j.status;
      return `<div class="job"><div class="top"><div><div class="name">${esc(j.file || j.url)}</div>
        <div class="meta">${esc(j.folder ? '/' + j.folder : '/')} · ${esc(j.url)}</div></div>
        <div class="st ${j.status}">${esc(state)}${j.status === 'downloading' ? ` · <button class="link" onclick="cancelJob('${j.id}')">cancel</button>` : ''}</div></div>
        ${j.status === 'downloading' ? `<div class="bar"><i style="width:${j.total ? pct : 100}%;${j.total ? '' : 'opacity:.35'}"></i></div><div class="meta">${size}</div>` : ''}
        ${j.status === 'done' && size ? `<div class="meta">${size}</div>` : ''}
        ${j.error ? `<div class="err">${esc(j.error)}</div>` : ''}
        ${j.links && j.links.length ? `<div class="links">${j.links.map(l => `<a href="${esc(l)}" target="_blank" rel="noopener">${esc(l)}</a>`).join('')}</div>` : ''}
      </div>`;
    }).join('');
  } catch (e) {}
}
async function cancelJob(id) { await fetch('/cancel/' + id, {method: 'POST'}); refresh(); }
async function clearDone() { await fetch('/clear', {method: 'POST'}); refresh(); }
refresh(); setInterval(refresh, 1500);
</script></body></html>"""


if __name__ == "__main__":
    app.run(host="0.0.0.0", port=8000, threaded=True)
```

## 9. Troubleshooting

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| Caddy can't get certificates | DNS not pointing here, port 80 blocked, or Cloudflare proxy in Flexible mode | Check `dig`, `ufw status`; set Cloudflare to Full (strict) or DNS-only |
| Authelia exits on start | Config syntax, a missing secret file, or the template filter not enabled | `docker compose logs authelia`; confirm `X_AUTHELIA_CONFIG_FILTERS=template` and every file in `authelia/secrets/` exists |
| `cdn.` returns the file without login | `forward_auth` missing in that site block | Confirm `import authelia` there, then `docker compose exec caddy caddy reload --config /etc/caddy/Caddyfile` |
| Redirect loop between `cdn.` and `auth.` | Session cookie domain doesn't cover the subdomain | `session.cookies[0].domain` must be `axiosiiitl.dev` |
| FileBrowser container restarts | Strict config validation | The log names the bad key; compare with the default config linked in §8 |
| FileBrowser OIDC "invalid client" or redirect error | Secret pair mismatch or wrong redirect URI | `filebrowser/config.yaml` holds the plain secret from `authelia/secrets/filebrowser_client_secret`; Authelia reads its digest. To regenerate both: delete those two secret files and `filebrowser/config.yaml`, rerun the script, restart both. Redirect URI must be exactly `https://index.axiosiiitl.dev/api/auth/oidc/callback` |
| Permission denied writing to `/srv/files` | Ownership | `sudo chown -R 1000:1000 /srv/files` |
| Fetcher: "opens a web page, not a file" | Link is a viewer page (MediaFire, Mega, private Drive) | Use a direct link; Drive files must be shared "Anyone with the link" |
| Fetcher Drive error mentioning quota | Google limits popular files | Retry after about 24 hours |
| Video seeks slowly inside a zip | Zip was compressed | Re-zip with `zip -0` |

## 10. Known limits (not bugs)

- Only `.zip` and `.sqfs` open as folders on `cdn.`; `.rar` and `.7z` download as whole files.
- `cdn.` links rely on the browser's login cookie, so VLC and TV apps can't open them. Use a FileBrowser share link for those.
- Fetcher keeps its job list in memory; a restart clears the list, not the files. Private Google Drive files, Mega and MediaFire aren't supported.
- Authelia's file notifier writes emails to `notification.txt` instead of sending them.

## 11. Report back to the user

End with a short report:

1. What already existed and was kept.
2. What you created or changed (file paths; backups made; commits pushed).
3. Results of every test in §7, pass or fail.
4. Anything blocked on the user (the `auth` DNS record, Cloudflare settings, firewall confirmation, GitHub access on the server).
5. Versions running: `docker compose images`.
