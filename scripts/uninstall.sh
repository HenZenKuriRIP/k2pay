#!/usr/bin/env bash
# K2Pay 卸载
#   root: bash <(curl -fsSL https://raw.githubusercontent.com/HenZenKuriRIP/k2pay/main/scripts/uninstall.sh)
#   普通用户: curl ... -o /tmp/k2pay-u.sh && sudo bash /tmp/k2pay-u.sh -y
set -euo pipefail

FORCE=0
PURGE_ALL=0

C0='\033[0m'; C1='\033[1;36m'; C2='\033[1;32m'; C3='\033[1;33m'; C4='\033[1;31m'
ok()   { echo -e "  ${C2}✓${C0} $*"; }
warn() { echo -e "  ${C3}!${C0} $*"; }
die()  { echo -e "  ${C4}✗ $*${C0}" >&2; exit 1; }

# curl|bash 时 stdin 是管道，交互必须从 /dev/tty 读
ask() {
  local prompt="$1" out
  if [[ -r /dev/tty ]]; then
    read -rp "$prompt" out < /dev/tty || true
  elif [[ -t 0 ]]; then
    read -rp "$prompt" out || true
  else
    out=""
  fi
  REPLY="$out"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    -y|--force|yes) FORCE=1; shift ;;
    --purge-all|purge) PURGE_ALL=1; shift ;;
    -h|--help)
      cat <<EOF
卸载 K2Pay

  bash <(curl -fsSL https://raw.githubusercontent.com/HenZenKuriRIP/k2pay/main/scripts/uninstall.sh)
  bash <(curl -fsSL .../uninstall.sh) -y
  bash <(curl -fsSL .../uninstall.sh) -y --purge-all
EOF
      exit 0 ;;
    *) die "未知参数: $1" ;;
  esac
done

if [[ "$(id -u)" -ne 0 ]]; then
  die "请用 root 执行，或: curl -fsSL https://raw.githubusercontent.com/HenZenKuriRIP/k2pay/main/scripts/uninstall.sh -o /tmp/k2pay-u.sh && sudo bash /tmp/k2pay-u.sh"
fi

echo -e "${C1}K2Pay 卸载${C0}"
echo "  移除: 程序 / 服务 / Nginx 站点"
if [[ "$PURGE_ALL" -eq 1 ]]; then
  echo "  清除: 配置、上传数据、PostgreSQL 库"
else
  echo "  保留: 数据库、配置、证书"
fi
echo

if [[ "$FORCE" -ne 1 ]]; then
  ask "确认卸载? [y/N] "
  [[ "${REPLY:-}" =~ ^[yY]$ ]] || { echo "已取消"; exit 0; }
fi

systemctl stop k2pay 2>/dev/null || true
systemctl disable k2pay 2>/dev/null || true
rm -f /etc/systemd/system/k2pay.service
systemctl daemon-reload 2>/dev/null || true
ok "服务已停止"

rm -f /usr/local/bin/k2pay /usr/bin/k2pay
rm -rf /opt/k2pay
ok "程序已删除"

rm -f /etc/nginx/sites-enabled/k2pay /etc/nginx/sites-enabled/k2payy
rm -f /etc/nginx/sites-available/k2pay /etc/nginx/conf.d/k2pay.conf
rm -rf /var/www/k2pay-acme
if command -v nginx >/dev/null 2>&1 && nginx -t >/dev/null 2>&1; then
  systemctl reload nginx 2>/dev/null || true
fi
ok "Nginx 已清理"

if [[ "$PURGE_ALL" -eq 1 ]]; then
  rm -rf /etc/k2pay /var/lib/k2pay
  if id postgres >/dev/null 2>&1; then
    sudo -u postgres psql -c "DROP DATABASE IF EXISTS k2pay;" >/dev/null 2>&1 || true
    sudo -u postgres psql -c "DROP USER IF EXISTS k2pay;" >/dev/null 2>&1 || true
  fi
  ok "数据与数据库已清除"
else
  warn "已保留 /etc/k2pay、数据库（全清请加 --purge-all）"
fi

echo -e "\n${C2}卸载完成${C0}\n"
