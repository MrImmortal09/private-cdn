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
