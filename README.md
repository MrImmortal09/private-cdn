# private-cdn

A private, self-hosted file CDN for `axiosiiitl.dev`. Every file gets a direct link, videos inside `.zip` files stream without extracting, and you can paste a Google Drive or download link to pull files onto the server. Nothing is reachable without your login.

| Address | Service | What it does |
| --- | --- | --- |
| `auth.axiosiiitl.dev` | [Authelia](https://www.authelia.com) | Login with password + 2FA; one sign-in covers every subdomain |
| `index.axiosiiitl.dev` | [FileBrowser Quantum](https://github.com/gtsteffaniak/filebrowser) | Upload, browse, delete; share links with expiry for guests |
| `cdn.axiosiiitl.dev` | [rclone](https://rclone.org/archive/) `serve http` | Direct links to every file, including files inside zips |
| `fetch.axiosiiitl.dev` | Fetcher (`fetcher/`) | Paste Google Drive, Dropbox or direct links; files download to a folder you pick |

```
Browser ──HTTPS──> Caddy :443  (only public ports: 80, 443)
                    ├── auth   ──> Authelia
                    ├── index  ──> FileBrowser Quantum   (signs in through Authelia)
                    ├── cdn    ──[Authelia check]──> rclone    ──reads──> /srv/files
                    └── fetch  ──[Authelia check]──> Fetcher   ──writes─> /srv/files
```

## Repository layout

```text
docker-compose.yml               all five services
caddy/Caddyfile                  domains, HTTPS, the Authelia gate
authelia/config/
  configuration.yml              login rules, session, OIDC client for FileBrowser
  users.yml.example              copy to users.yml (gitignored)
filebrowser/config.yaml.example  the setup script turns it into config.yaml (gitignored)
fetcher/                         the paste-a-link downloader (Flask + gdown)
scripts/setup-secrets.sh         generates all secrets; safe to re-run
AGENT_HANDOFF.md                 full runbook: audit, setup, tests, troubleshooting
```

Secrets, users, databases and certificates are gitignored and live only on the server.

## Setup

Needs a Linux server with Docker and Compose, and DNS A records for `auth`, `index`, `cdn` and `fetch` pointing to it.

```bash
sudo mkdir -p /opt/private-cdn /srv/files && sudo chown "$USER" /opt/private-cdn
git clone https://github.com/MrImmortal09/private-cdn /opt/private-cdn && cd /opt/private-cdn

./scripts/setup-secrets.sh

# Your login: type your password at the prompt, then put the Digest in users.yml
docker run --rm -it authelia/authelia:4.39 authelia crypto hash generate argon2
cp authelia/config/users.yml.example authelia/config/users.yml && nano authelia/config/users.yml
chmod 600 authelia/config/users.yml

sudo chown -R 1000:1000 /srv/files filebrowser
docker compose up -d --build
```

Then open `https://auth.axiosiiitl.dev`, sign in and register 2FA. The one-time code Authelia asks for is in `authelia/config/notification.txt`. Once "Sign in with Authelia" works on `index.axiosiiitl.dev`, set `password.enabled: false` in `filebrowser/config.yaml` and restart FileBrowser.

[AGENT_HANDOFF.md](AGENT_HANDOFF.md) has the full runbook, including acceptance tests and troubleshooting.

## Using it

- **Videos in zips:** `/srv/files/show.zip` containing `s1/ep1.mp4` plays at `https://cdn.axiosiiitl.dev/show.zip/s1/ep1.mp4`. Zip videos with `zip -0` (no compression) so skipping ahead is instant.
- **Paste a link:** on `fetch.axiosiiitl.dev`, paste one or more links, choose a folder, and press Download. Google Drive files must be shared as "Anyone with the link". Existing files are never overwritten.
- **Share with someone without an account:** make a share link in FileBrowser with an expiry and password.

## Limits

- Only `.zip` and `.sqfs` open as folders; `.rar` and `.7z` download whole.
- `cdn.` links need the browser login, so VLC and TV apps can't open them; use a FileBrowser share link.
- Fetcher doesn't support private Google Drive files, Mega or MediaFire. Its Google Drive path relies on gdown and hasn't been tested against Google yet.
