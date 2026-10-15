#!/usr/bin/env bash
# Deploys the latest main: pull, test, build, swap the binary, restart, and
# health-check, rolling back to the previous binary if the new one doesn't come
# up. Installed as /usr/local/bin/private-cdn-deploy and run by GitHub Actions
# over SSH as the forced command of a restricted deploy key.
set -euo pipefail

REPO_DIR="${REPO_DIR:-/home/ubuntu/private-cdn}"
REMOTE="${DEPLOY_REMOTE:-upstream}"   # https://github.com/MrImmortal09/private-cdn
BRANCH="${DEPLOY_BRANCH:-main}"
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:8090/healthz}"
BIN=/usr/local/bin/private-cdn
UNIT=/etc/systemd/system/private-cdn.service
GO=/usr/local/go/bin/go
export GOCACHE="$HOME/.cache/go-build" GOPATH="$HOME/go"

cd "$REPO_DIR"
git fetch --quiet "$REMOTE" "$BRANCH"
git checkout --quiet --force -B "$BRANCH" "$REMOTE/$BRANCH"
echo "==> deploying $(git log -1 --format='%h %s')"

echo "==> test"
"$GO" test -count=1 ./...

echo "==> build"
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
"$GO" build -trimpath -ldflags='-s -w' -o "$tmp" .

echo "==> install"
[ -x "$BIN" ] && sudo cp -p "$BIN" "$BIN.prev"
sudo install -m 755 "$tmp" "$BIN"
unit_changed=false
if ! cmp -s deploy/private-cdn.service "$UNIT"; then
  [ -f "$UNIT" ] && sudo cp -p "$UNIT" "$UNIT.prev"
  sudo install -m 644 deploy/private-cdn.service "$UNIT"
  sudo systemctl daemon-reload
  unit_changed=true
fi
sudo install -m 755 deploy/deploy.sh /usr/local/bin/private-cdn-deploy

echo "==> restart"
sudo systemctl restart private-cdn
for _ in $(seq 1 20); do
  if curl -fsS --max-time 2 "$HEALTH_URL" >/dev/null; then
    echo "==> healthy: $(git log -1 --format='%h')"
    exit 0
  fi
  sleep 1
done

echo "!! health check failed; rolling back" >&2
sudo journalctl -u private-cdn -n 30 --no-pager >&2 || true
if $unit_changed && [ -f "$UNIT.prev" ]; then
  sudo install -m 644 "$UNIT.prev" "$UNIT"
  sudo systemctl daemon-reload
fi
if [ -x "$BIN.prev" ]; then
  sudo install -m 755 "$BIN.prev" "$BIN"
fi
sudo systemctl restart private-cdn
exit 1
