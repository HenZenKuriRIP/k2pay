#!/usr/bin/env bash
# K2Pay 卸载脚本
# 默认: 停止服务、删除程序/Nginx 站点配置，保留数据库与 Let's Encrypt 证书
# 用法: sudo bash scripts/uninstall.sh
#       sudo bash scripts/uninstall.sh --purge-app-data   # 同时删除 /var/lib/k2pay
set -euo pipefail

PURGE_APP_DATA=0
FORCE=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --purge-app-data) PURGE_APP_DATA=1; shift ;;
    --force|-y) FORCE=1; shift ;;
    -h|--help)
      echo "Usage: $0 [--purge-app-data] [--force]"
      echo "  默认保留: MySQL 中 k2pay 库、/etc/letsencrypt 证书"
      echo "  --purge-app-data  删除 /var/lib/k2pay 上传数据"
      exit 0 ;;
    *) echo "未知参数: $1" >&2; exit 1 ;;
  esac
done

[[ "$(id -u)" -eq 0 ]] || { echo "请使用 root 运行"; exit 1; }

echo "将卸载 K2Pay 程序与站点配置。"
echo "保留: 数据库 (k2pay)、Let's Encrypt 证书 (/etc/letsencrypt)"
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

# 二进制与安装目录
rm -f /usr/local/bin/k2pay /usr/bin/k2pay
rm -rf /opt/k2pay

# Nginx 站点配置 (不删证书)
rm -f /etc/nginx/sites-enabled/k2pay
rm -f /etc/nginx/sites-available/k2pay
rm -f /etc/nginx/conf.d/k2pay.conf
if command -v nginx >/dev/null 2>&1; then
  nginx -t 2>/dev/null && systemctl reload nginx 2>/dev/null || true
fi

# 应用配置（可选保留连接信息）
# 默认保留 /etc/k2pay/db.credentials 方便对照数据库；删除运行配置中的密钥可选手动
# 这里删除 config.yaml 避免误启，但保留 db.credentials
if [[ -d /etc/k2pay ]]; then
  rm -f /etc/k2pay/config.yaml /etc/k2pay/site.env
  echo "保留: /etc/k2pay/db.credentials (若存在，内含数据库密码)"
fi

if [[ "$PURGE_APP_DATA" -eq 1 ]]; then
  rm -rf /var/lib/k2pay
  echo "已删除应用数据目录 /var/lib/k2pay"
else
  echo "保留: /var/lib/k2pay (二维码等上传文件)"
fi

# 不删除系统用户 k2pay，避免误伤；不删除 MySQL 库
echo
echo "========== 卸载完成 =========="
echo "  已删除: 二进制、systemd、Nginx 站点配置"
echo "  已保留: MySQL 数据库 k2pay、/etc/letsencrypt 证书"
echo "  如需删库: mysql -uroot -e \"DROP DATABASE k2pay; DROP USER IF EXISTS 'k2pay'@'localhost';\""
echo "  证书未动: /etc/letsencrypt/live/<域名>/"
echo "=============================="
