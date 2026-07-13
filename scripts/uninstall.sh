#!/usr/bin/env bash
# =============================================================================
# K2Pay 卸载脚本
# =============================================================================
# 默认保留: PostgreSQL 数据、Let's Encrypt 证书、上传目录
# 用法:
#   curl -fsSL https://raw.githubusercontent.com/HenZenKuriRIP/k2pay/main/scripts/uninstall.sh | sudo bash
#   sudo bash uninstall.sh -y
#   sudo bash uninstall.sh --purge-all -y   # 连数据、配置、数据库一并删除
# =============================================================================
set -euo pipefail

FORCE=0
PURGE_ALL=0

C0='\033[0m'; C1='\033[1;36m'; C2='\033[1;32m'; C3='\033[1;33m'; C4='\033[1;31m'
ok()   { echo -e "  ${C2}✓${C0} $*"; }
warn() { echo -e "  ${C3}!${C0} $*"; }
die()  { echo -e "  ${C4}✗ $*${C0}" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    -y|--force) FORCE=1; shift ;;
    --purge-all) PURGE_ALL=1; shift ;;
    -h|--help)
      cat <<EOF
K2Pay 卸载

  sudo bash uninstall.sh [选项]

  -y, --force     跳过确认
  --purge-all     删除配置、上传数据、PostgreSQL 库/用户（证书仍保留）
EOF
      exit 0 ;;
    *) die "未知参数: $1" ;;
  esac
done

[[ "$(id -u)" -eq 0 ]] || die "请使用 root 运行"

echo -e "${C1}"
echo "╔══════════════════════════════════════╗"
echo "║          K2Pay Uninstaller           ║"
echo "╚══════════════════════════════════════╝"
echo -e "${C0}"
echo "  将移除: 程序、systemd、Nginx 站点配置"
if [[ "$PURGE_ALL" -eq 1 ]]; then
  echo "  将清除: /etc/k2pay、/var/lib/k2pay、PostgreSQL 库 k2pay"
else
  echo "  将保留: 数据库、配置、上传数据、Let's Encrypt 证书"
fi
echo

if [[ "$FORCE" -ne 1 ]]; then
  read -rp "确认卸载? [y/N] " ans
  [[ "${ans:-}" =~ ^[yY]$ ]] || { echo "已取消"; exit 0; }
fi

echo
echo -e "${C1}▸ 停止服务${C0}"
systemctl stop k2pay 2>/dev/null || true
systemctl disable k2pay 2>/dev/null || true
rm -f /etc/systemd/system/k2pay.service
systemctl daemon-reload 2>/dev/null || true
ok "systemd"

echo -e "${C1}▸ 删除程序${C0}"
rm -f /usr/local/bin/k2pay /usr/bin/k2pay
rm -rf /opt/k2pay
ok "二进制已删除"

echo -e "${C1}▸ 清理 Nginx${C0}"
rm -f /etc/nginx/sites-enabled/k2pay /etc/nginx/sites-enabled/k2payy
rm -f /etc/nginx/sites-available/k2pay
rm -f /etc/nginx/conf.d/k2pay.conf
rm -rf /var/www/k2pay-acme
if command -v nginx >/dev/null 2>&1 && nginx -t >/dev/null 2>&1; then
  systemctl reload nginx 2>/dev/null || true
fi
ok "Nginx 站点已清理"

if [[ "$PURGE_ALL" -eq 1 ]]; then
  echo -e "${C1}▸ 清除数据与数据库${C0}"
  rm -rf /etc/k2pay /var/lib/k2pay
  if id postgres >/dev/null 2>&1; then
    sudo -u postgres psql -c "DROP DATABASE IF EXISTS k2pay;" >/dev/null 2>&1 || true
    sudo -u postgres psql -c "DROP USER IF EXISTS k2pay;" >/dev/null 2>&1 || true
  fi
  ok "配置、上传数据与 PostgreSQL 库已删除"
else
  warn "已保留 /etc/k2pay、/var/lib/k2pay 与 PostgreSQL 数据（可用 --purge-all 全清）"
fi

echo
echo -e "${C2}卸载完成${C0}"
echo "  证书目录 /etc/letsencrypt 未改动"
echo
