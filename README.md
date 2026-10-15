# private-cdn

A private, self-hosted file CDN for `cdn.axiosiiitl.dev`, in one Go binary with no dependencies outside the standard library.

- **Private by default.** Browsing, uploading, deleting and fetching all need your username and password.
- **Every file has a share link**, including every file inside a `.zip`: `https://cdn.axiosiiitl.dev/share/k3X9fQ2mZp7Lw0aB`. Anyone with the link can stream or download that one file with no login. The code is an HMAC of the path, so it is stable and reveals nothing about the path.
- **Videos inside zips stream** with seeking (HTTP Range), in the browser, VLC or a TV app.
- **Fetch** (`/fetch`): paste Google Drive, Dropbox or direct links and pick a folder; files download onto the server and show their share link when done.

```
Browser ──HTTPS──> nginx :443 ──> private-cdn 127.0.0.1:8090 ──> /srv/files
```

| Path | Login | What it is |
| --- | --- | --- |
| `/files/…` | yes | Browse folders and zips, upload, new folder, delete, **Copy link** |
| `/fetch` | yes | Paste-a-link downloader |
| `/share/<code>` | **no** | The shared file, streamed |
| `/login`, `/logout` | – | Session (30 days, HttpOnly cookie) |

## Setup

Needs Go 1.24+ to build, nginx with a certificate for `cdn.axiosiiitl.dev`, and a DNS record pointing to the server.

```bash
git clone https://github.com/MrImmortal09/private-cdn && cd private-cdn
go build -trimpath -ldflags='-s -w' -o private-cdn .
sudo install -m 755 private-cdn /usr/local/bin/private-cdn

# Your login: type the password at the prompt; put the printed hash in the env file
private-cdn hash-password
sudo install -m 600 deploy/private-cdn.env.example /etc/private-cdn.env && sudo nano /etc/private-cdn.env

sudo mkdir -p /srv/files && sudo chown ubuntu:ubuntu /srv/files
sudo cp deploy/private-cdn.service /etc/systemd/system/ && sudo systemctl daemon-reload
sudo systemctl enable --now private-cdn

sudo cp deploy/nginx-cdn.conf /etc/nginx/sites-available/private-cdn
sudo ln -s /etc/nginx/sites-available/private-cdn /etc/nginx/sites-enabled/
sudo nginx -t && sudo systemctl reload nginx
```

Adjust `User=` in the service file and the certificate paths in the nginx file to your server.

## Configuration (`/etc/private-cdn.env`)

| Variable | Default | |
| --- | --- | --- |
| `CDN_USER` | – | Username (required) |
| `CDN_PASSWORD_HASH` | – | From `private-cdn hash-password` (required) |
| `CDN_ROOT` | `/srv/files` | Folder that is served |
| `CDN_PUBLIC_URL` | `https://cdn.axiosiiitl.dev` | Used in share links and for the same-origin check |
| `CDN_ADDR` | `127.0.0.1:8090` | Listen address; keep it on loopback behind nginx |
| `CDN_STATE_DIR` | `/var/lib/private-cdn` | Server secret and the share-link index |

## Notes

- **Revoking links:** a link stops working when its file is moved, renamed or deleted. To revoke every link at once, stop the service, delete `/var/lib/private-cdn/secret`, and start it again (this also signs you out).
- **Zip videos:** create them with `zip -0` (no compression). Stored entries seek instantly; compressed entries work, but seeking forward decompresses everything before that point.
- **Security:** five wrong passwords from one address lock it out for 15 minutes. State-changing requests must come from the site itself (Origin check plus a SameSite cookie). Served files carry `Content-Security-Policy: sandbox`, so an uploaded `.html` file can't act as the site. Fetch refuses links that resolve to private or loopback addresses.
- Hidden files (names starting with `.`) are never listed or served. Uploads and downloads in progress are written as hidden `.part` files and renamed when complete, and existing files are never overwritten.
- Fetch doesn't support Google Drive folders, private Drive files, Mega or MediaFire.

## Development

```bash
go test -race ./...
```
