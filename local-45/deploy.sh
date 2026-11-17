#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
HOST="${WB_HOST:-tokyo112}"
AUTO_YES=0
COMPONENT=""
for arg in "$@"; do
 case "$arg" in
  -y|--yes) AUTO_YES=1 ;;
  gateway|router|all) COMPONENT="$arg" ;;
  *) printf 'Usage: bash local-45/deploy.sh [-y|--yes] gateway|router|all\n' >&2; exit 1 ;;
 esac
done
case "$COMPONENT" in gateway|router|all) ;; *) printf 'Usage: bash local-45/deploy.sh [-y|--yes] gateway|router|all\n' >&2; exit 1 ;; esac

cd "$ROOT/local-45/dist"
shasum -a 256 -c SHA256SUMS
if [[ "$AUTO_YES" -ne 1 ]]; then
 printf 'Deploy %s to %s; preserve config/auths/data. Type deploy to continue: ' "$COMPONENT" "$HOST"
 read -r answer
 [[ "$answer" == deploy ]] || exit 1
fi

# 独立暂存目录；不从下载目录执行代码，退出时自动清理暂存目录。
STAGE=$(ssh "$HOST" 'mktemp -d /opt/services/workbuddy2api-panel/.deploy.XXXXXXXX')
scp wb2api wb-router SHA256SUMS "$HOST:$STAGE/"
ssh "$HOST" bash -s -- "$STAGE" "$COMPONENT" <<'REMOTE'
set -euo pipefail
STAGE="$1"; COMPONENT="$2"
D=/opt/services/workbuddy2api-panel
cd "$STAGE"
sha256sum -c SHA256SUMS
B=$(mktemp -d /opt/backups/wb-local-45-XXXXXXXX)
cp "$D/wb2api" "$B/wb2api.before"
cp "$D/wb-router" "$B/wb-router.before"
systemctl cat workbuddy2api-panel wb-router > "$B/units.before"
printf 'Rollback backup: %s\n' "$B"
rollback() {
 printf 'Deployment failed; restoring selected binaries\n' >&2
 if [[ "$COMPONENT" != router ]]; then
  cp "$B/wb2api.before" "$D/.wb2api.rollback"; mv "$D/.wb2api.rollback" "$D/wb2api"
  systemctl restart workbuddy2api-panel
 fi
 if [[ "$COMPONENT" != gateway ]]; then
  cp "$B/wb-router.before" "$D/.wb-router.rollback"; mv "$D/.wb-router.rollback" "$D/wb-router"
  systemctl restart wb-router
 fi
 rm -rf "$STAGE"
 exit 1
}
trap rollback ERR
if [[ "$COMPONENT" != router ]]; then
 chmod 755 "$STAGE/wb2api"; mv "$STAGE/wb2api" "$D/wb2api"
 systemctl restart workbuddy2api-panel
fi
if [[ "$COMPONENT" != gateway ]]; then
 chmod 755 "$STAGE/wb-router"; mv "$STAGE/wb-router" "$D/wb-router"
 systemctl restart wb-router
fi
curl -fsS --retry 6 --retry-connrefused --retry-delay 2 --max-time 8 http://127.0.0.1:8789/healthz
echo ""
curl -fsS --retry 6 --retry-connrefused --retry-delay 2 --max-time 8 http://127.0.0.1:8788/healthz
echo ""
systemctl is-active workbuddy2api-panel wb-router
trap - ERR
# 清理本次及历史遗留的空/旧 .deploy.* 暂存目录，并只保留最近 10 份回滚备份。
rm -rf "$STAGE"
find "$D" -maxdepth 1 -type d -name '.deploy.*' -mmin +30 -exec rm -rf {} + 2>/dev/null || true
ls -1dt /opt/backups/wb-local-45-* 2>/dev/null | tail -n +11 | xargs -r rm -rf
printf 'Deployment complete. Backup retained: %s\n' "$B"
REMOTE
