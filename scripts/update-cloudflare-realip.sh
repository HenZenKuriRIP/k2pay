#!/usr/bin/env bash
# 从 Cloudflare 官方拉取 IP 段，生成 Nginx real_ip 配置
# 用法:
#   sudo bash scripts/update-cloudflare-realip.sh
#   sudo bash scripts/update-cloudflare-realip.sh /etc/nginx/snippets/cloudflare-realip.conf
set -euo pipefail

OUT="${1:-/etc/nginx/snippets/cloudflare-realip.conf}"
TMP="$(mktemp)"
trap 'rm -f "$TMP"' EXIT

echo "==> 拉取 Cloudflare IP 列表…"
{
  echo "# Cloudflare real_ip — generated $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "# Source: https://www.cloudflare.com/ips-v4 / ips-v6"
  echo "# Update: sudo bash scripts/update-cloudflare-realip.sh"
  echo "#"
  echo "# 在 server { } 中 include 本文件，并确保:"
  echo "#   proxy_set_header CF-Connecting-IP \$http_cf_connecting_ip;"
  echo "#   proxy_set_header X-Real-IP \$remote_addr;   # real_ip 生效后 \$remote_addr 为访客真 IP"
  echo "#   proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;"
  echo ""

  echo "# ---- IPv4 ----"
  if ! curl -fsSL --max-time 15 https://www.cloudflare.com/ips-v4; then
    echo "ERROR: 无法拉取 ips-v4" >&2
    exit 1
  fi | while read -r cidr; do
    [[ -z "$cidr" ]] && continue
    echo "set_real_ip_from $cidr;"
  done

  echo ""
  echo "# ---- IPv6 ----"
  if ! curl -fsSL --max-time 15 https://www.cloudflare.com/ips-v6; then
    echo "ERROR: 无法拉取 ips-v6" >&2
    exit 1
  fi | while read -r cidr; do
    [[ -z "$cidr" ]] && continue
    echo "set_real_ip_from $cidr;"
  done

  echo ""
  echo "real_ip_header CF-Connecting-IP;"
  echo "real_ip_recursive on;"
} > "$TMP"

# 至少应有若干 set_real_ip_from
count="$(grep -c 'set_real_ip_from' "$TMP" || true)"
if [[ "$count" -lt 5 ]]; then
  echo "ERROR: 生成的规则过少 ($count)，中止写入" >&2
  exit 1
fi

mkdir -p "$(dirname "$OUT")"
install -m 644 "$TMP" "$OUT"
echo "OK: 已写入 $OUT （${count} 条 set_real_ip_from）"

if command -v nginx >/dev/null 2>&1; then
  if nginx -t 2>/dev/null; then
    systemctl reload nginx 2>/dev/null || service nginx reload 2>/dev/null || true
    echo "OK: nginx 已 reload"
  else
    echo "WARN: nginx -t 失败，请手动检查后 reload" >&2
  fi
fi
