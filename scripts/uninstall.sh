#!/usr/bin/env bash
# K2Pay 卸载
# 默认: 停服务、删二进制、清理 K2Pay 的 Nginx 站点配置
# 保留: MySQL 数据库 k2pay、/etc/letsencrypt 证书、/var/lib/k2pay 上传数据
#
# 用法:
#   sudo bash scripts/uninstall.sh
#   sudo bash scripts/uninstall.sh --force
#   sudo bash scripts/uninstall.sh --purge-app-data
#   sudo bash scripts/uninstall.sh --restore-nginx-default  # 尝试恢复被禁用的 default
set -euo pipefail

PURGE_APP_DATA=0
FORCE=0
RESTORE_DEFAULT=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --purge-app-data) PURGE_APP_DATA=1; shift ;;
    --restore-nginx-default) RESTORE_DEFAULT=1; shift ;;
    --force|-y) FORCE=1; shift ;;
    -h|--help)
      cat <<EOF
Usage: $0 [options]
  默认保留: MySQL 库 k2pay、Let's Encrypt 证书、应用上传目录
  --purge-app-data         删除 /var/lib/k2pay
  --restore-nginx-default  若备份存在，恢复 sites-enabled/default
  --force / -y             跳过确认
EOF
      exit 0 ;;
    *) echo "未知参数: $1" >&2; exit 1 ;;
  esac
done

[[ "$(id -u)" -eq 0 ]] || { echo "请使用 root 运行"; exit 1; }

echo "将卸载 K2Pay 程序与其 Nginx 站点配置。"
echo "保留: 数据库 k2pay、/etc/letsencrypt 证书"
[[ "$PURGE_APP_DATA" -eq 1 ]] && echo "将删除: /var/lib/k2pay"
if [[ "$FORCE" -ne 1 ]]; then
  read -rp "确认卸载? [y/N] " ans
  [[ "${ans:-}" =~ ^[yY]$ ]] || { echo "已取消"; exit 0; }
fi

# 停止服务
systemctl stop k2pay 2>/dev/null || true
systemctl disable k2pay 2>/dev/null || true
rm -f /etc/systemd/system/k2pay.service
systemctl daemon-reload 2>/dev/null || true

# 二进制
rm -f /usr/local/bin/k2pay /usr/bin/k2pay
rm -rf /opt/k2pay

# Nginx：仅清理 K2Pay 相关（不删证书、不卸 nginx 软件包）
echo "清理 Nginx 中的 K2Pay 站点..."
rm -f /etc/nginx/sites-enabled/k2pay
rm -f /etc/nginx/sites-enabled/k2payy
rm -f /etc/nginx/sites-available/k2pay
rm -f /etc/nginx/conf.d/k2pay.conf
rm -rf /var/www/k2pay-acme

# 可选：恢复 default
if [[ "$RESTORE_DEFAULT" -eq 1 ]]; then
  LATEST_BAK="$(ls -dt /root/k2pay-nginx-backup-* 2>/dev/null | head -1 || true)"
  if [[ -n "$LATEST_BAK" && -f "$LATEST_BAK/default" ]]; then
    if [[ -d /etc/nginx/sites-available ]]; then
      cp -a "$LATEST_BAK/default" /etc/nginx/sites-available/default
      ln -sfn /etc/nginx/sites-available/default /etc/nginx/sites-enabled/default
      echo "已从备份恢复 default: $LATEST_BAK"
    fi
  else
    echo "未找到可恢复的 default 备份"
  fi
fi

if command -v nginx >/dev/null 2>&1; then
  if nginx -t 2>/dev/null; then
    systemctl reload nginx 2>/dev/null || true
  else
    echo "警告: nginx -t 失败，请手动检查 /etc/nginx"
  fi
fi

# 应用配置：删 config 防误启，保留 db.credentials
if [[ -d /etc/k2pay ]]; then
  rm -f /etc/k2pay/config.yaml /etc/k2pay/site.env
  echo "保留: /etc/k2pay/db.credentials（若存在）"
fi

if [[ "$PURGE_APP_DATA" -eq 1 ]]; then
  rm -rf /var/lib/k2pay
  echo "已删除 /var/lib/k2pay"
else
  echo "保留: /var/lib/k2pay"
fi

echo
echo "========== 卸载完成 =========="
echo "  已删除: 二进制、systemd、K2Pay Nginx 站点"
echo "  已保留: MySQL 数据库 k2pay、/etc/letsencrypt 证书"
echo "  重装:   curl -fsSL https://raw.githubusercontent.com/HenZenKuriRIP/k2pay/main/scripts/install.sh | \\"
echo "            sudo bash -s -- --domain 你的域名 --email 你的邮箱 --reinstall"
echo "  删库:   mysql -uroot -e \"DROP DATABASE k2pay;\""
echo "=============================="
