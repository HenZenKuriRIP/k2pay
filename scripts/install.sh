#!/usr/bin/env bash
# K2Pay 一键安装
# 用法:
#   curl -fsSL https://raw.githubusercontent.com/HenZenKuriRIP/k2pay/main/scripts/install.sh | sudo bash -s -- --domain pay.example.com --email you@example.com
#   sudo bash scripts/install.sh --domain pay.example.com --email you@example.com
set -euo pipefail

REPO="HenZenKuriRIP/k2pay"
INSTALL_DIR="/opt/k2pay"
DATA_DIR="/var/lib/k2pay"
CONF_DIR="/etc/k2pay"
BIN_PATH="/usr/local/bin/k2pay"
SERVICE_FILE="/etc/systemd/system/k2pay.service"
DB_NAME="k2pay"
DB_USER="k2pay"
HTTP_PORT="6088"

DOMAIN=""
EMAIL=""
SKIP_CERT=0
SKIP_NGINX=0
VERSION="latest"

GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RED='\033[0;31m'; NC='\033[0m'
info() { echo -e "${GREEN}[INFO]${NC} $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
die()  { echo -e "${RED}[ERR]${NC} $*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --domain) DOMAIN="${2:-}"; shift 2 ;;
    --email) EMAIL="${2:-}"; shift 2 ;;
    --version) VERSION="${2:-}"; shift 2 ;;
    --skip-cert) SKIP_CERT=1; shift ;;
    --skip-nginx) SKIP_NGINX=1; shift ;;
    -h|--help)
      echo "Usage: $0 [--domain FQDN] [--email EMAIL] [--version TAG] [--skip-cert] [--skip-nginx]"
      exit 0 ;;
    *) die "未知参数: $1" ;;
  esac
done

[[ "$(id -u)" -eq 0 ]] || die "请使用 root: sudo bash $0"

if [[ -z "$DOMAIN" && "$SKIP_NGINX" -eq 0 ]]; then
  if [[ -t 0 ]]; then
    read -rp "域名 (留空跳过 Nginx/证书): " DOMAIN || true
  fi
  [[ -z "${DOMAIN:-}" ]] && SKIP_NGINX=1 && SKIP_CERT=1
fi
if [[ -n "${DOMAIN:-}" && "$SKIP_CERT" -eq 0 && -z "${EMAIL:-}" ]]; then
  if [[ -t 0 ]]; then
    read -rp "Let's Encrypt 邮箱 (留空跳过证书): " EMAIL || true
  fi
  [[ -z "${EMAIL:-}" ]] && SKIP_CERT=1
fi

ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) GOARCH=amd64 ;;
  aarch64|arm64) GOARCH=arm64 ;;
  *) die "不支持架构: $ARCH" ;;
esac
[[ "$(uname -s)" == "Linux" ]] || die "仅支持 Linux"

if command -v apt-get >/dev/null 2>&1; then PKG=apt
elif command -v dnf >/dev/null 2>&1; then PKG=dnf
elif command -v yum >/dev/null 2>&1; then PKG=yum
else die "需要 apt/dnf/yum"; fi

export DEBIAN_FRONTEND=noninteractive
info "安装依赖..."
if [[ "$PKG" == "apt" ]]; then
  apt-get update -y
  apt-get install -y curl ca-certificates tar gzip openssl python3
  if ! command -v mysql >/dev/null 2>&1; then
    apt-get install -y mariadb-server mariadb-client || apt-get install -y mysql-server default-mysql-client
  fi
  systemctl enable --now mariadb 2>/dev/null || systemctl enable --now mysql 2>/dev/null || true
  if [[ "$SKIP_NGINX" -eq 0 ]]; then
    apt-get install -y nginx
    systemctl enable --now nginx
    [[ "$SKIP_CERT" -eq 0 ]] && apt-get install -y certbot python3-certbot-nginx
  fi
else
  $PKG install -y curl tar gzip openssl python3
  $PKG install -y mariadb-server 2>/dev/null || $PKG install -y mysql-server 2>/dev/null || true
  systemctl enable --now mariadb 2>/dev/null || systemctl enable --now mysqld 2>/dev/null || true
  if [[ "$SKIP_NGINX" -eq 0 ]]; then
    $PKG install -y nginx
    systemctl enable --now nginx
    [[ "$SKIP_CERT" -eq 0 ]] && $PKG install -y certbot python3-certbot-nginx 2>/dev/null || true
  fi
fi

info "配置 MySQL/MariaDB..."
DB_PASS="$(openssl rand -base64 24 | tr -dc 'a-zA-Z0-9' | head -c 20)"
mysql_root() { mysql -uroot "$@"; }

mysql_root -e "CREATE DATABASE IF NOT EXISTS \`${DB_NAME}\` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;" || die "无法连接 MySQL root，请先配置 root 登录"
# 兼容新旧 MySQL/MariaDB 创建用户
mysql_root -e "CREATE USER '${DB_USER}'@'localhost' IDENTIFIED BY '${DB_PASS}';" 2>/dev/null \
  || mysql_root -e "ALTER USER '${DB_USER}'@'localhost' IDENTIFIED BY '${DB_PASS}';" 2>/dev/null \
  || mysql_root -e "SET PASSWORD FOR '${DB_USER}'@'localhost' = PASSWORD('${DB_PASS}');" 2>/dev/null \
  || true
mysql_root -e "GRANT ALL PRIVILEGES ON \`${DB_NAME}\`.* TO '${DB_USER}'@'localhost'; FLUSH PRIVILEGES;"
mysql -u"${DB_USER}" -p"${DB_PASS}" -e "USE ${DB_NAME};" || {
  # 再试一次重建用户
  mysql_root -e "DROP USER IF EXISTS '${DB_USER}'@'localhost';" 2>/dev/null || true
  mysql_root -e "CREATE USER '${DB_USER}'@'localhost' IDENTIFIED BY '${DB_PASS}'; GRANT ALL ON \`${DB_NAME}\`.* TO '${DB_USER}'@'localhost'; FLUSH PRIVILEGES;"
  mysql -u"${DB_USER}" -p"${DB_PASS}" -e "USE ${DB_NAME};" || die "数据库用户创建失败"
}

info "获取 K2Pay 二进制 (linux-${GOARCH})..."
TMPDIR="$(mktemp -d)"
cleanup() { rm -rf "$TMPDIR"; }
trap cleanup EXIT

fetch_binary() {
  local tag="$1"
  local urls=(
    "https://github.com/${REPO}/releases/download/${tag}/k2pay-linux-${GOARCH}.tar.gz"
    "https://github.com/${REPO}/releases/download/${tag}/k2pay-linux-${GOARCH}"
  )
  local u
  for u in "${urls[@]}"; do
    if curl -fsSL "$u" -o "$TMPDIR/dl" 2>/dev/null; then
      if file "$TMPDIR/dl" 2>/dev/null | grep -qi 'gzip\|tar'; then
        tar -xzf "$TMPDIR/dl" -C "$TMPDIR"
        local f
        f="$(find "$TMPDIR" -type f -name 'k2pay*' ! -name '*.tar.gz' | head -1)"
        [[ -n "$f" ]] && cp "$f" "$TMPDIR/k2pay" && return 0
      else
        cp "$TMPDIR/dl" "$TMPDIR/k2pay" && return 0
      fi
    fi
  done
  return 1
}

GOT=0
if [[ "$VERSION" == "latest" ]]; then
  TAG="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('tag_name',''))" 2>/dev/null || true)"
  if [[ -n "$TAG" ]] && fetch_binary "$TAG"; then GOT=1; fi
else
  fetch_binary "$VERSION" && GOT=1
fi

if [[ "$GOT" -ne 1 ]]; then
  warn "Release 无可用二进制，从源码编译..."
  if ! command -v go >/dev/null 2>&1; then
    if [[ "$PKG" == "apt" ]]; then apt-get install -y golang-go git
    else $PKG install -y golang git; fi
  fi
  command -v git >/dev/null 2>&1 || { [[ "$PKG" == "apt" ]] && apt-get install -y git || $PKG install -y git; }
  git clone --depth 1 "https://github.com/${REPO}.git" "$TMPDIR/src"
  (
    cd "$TMPDIR/src"
    CGO_ENABLED=0 go build -ldflags="-s -w -X main.Version=$(git describe --tags --always 2>/dev/null || echo dev)" -o "$TMPDIR/k2pay" .
  ) || die "源码编译失败。请先发布 Release 二进制: k2pay-linux-${GOARCH}.tar.gz"
fi

[[ -f "$TMPDIR/k2pay" ]] || die "未找到二进制文件"
chmod +x "$TMPDIR/k2pay"

mkdir -p "$INSTALL_DIR" "$DATA_DIR/qrcode" "$DATA_DIR/apk" "$CONF_DIR"
install -m 755 "$TMPDIR/k2pay" "$BIN_PATH"
id k2pay &>/dev/null || useradd --system --home "$DATA_DIR" --shell /usr/sbin/nologin k2pay
chown -R k2pay:k2pay "$DATA_DIR" "$INSTALL_DIR"

JWT_SECRET="$(openssl rand -hex 32)"
if [[ ! -f "$CONF_DIR/config.yaml" ]]; then
  info "写入 ${CONF_DIR}/config.yaml"
  cat > "$CONF_DIR/config.yaml" <<EOF
# K2Pay — generated by install.sh
server:
  host: "127.0.0.1"
  port: ${HTTP_PORT}

database:
  host: "127.0.0.1"
  port: 3306
  user: "${DB_USER}"
  password: "${DB_PASS}"
  dbname: "${DB_NAME}"
  max_open_conns: 50
  max_idle_conns: 10
  conn_max_lifetime: 60

jwt:
  secret: "${JWT_SECRET}"
  expire_hour: 24

storage:
  data_dir: "${DATA_DIR}"

security:
  rate_limit_api: 20
  rate_limit_api_burst: 50
  rate_limit_login: 2
  rate_limit_login_burst: 5
  cors_allow_origins: []
  ip_blacklist_cache_ttl: 30
  http_timeout: 15

order:
  expire_minutes: 30
  cleanup_hours: 24
  wallet_cache_ttl: 60

rate:
  auto_update_enabled: true
  update_interval: 60
  source: "binance"
  fallback_source: "okx"
  cache_seconds: 300

notify:
  retry_count: 5
  retry_interval: 10
  timeout: 30

log:
  level: "info"
  db_log_level: "warn"
  api_log_days: 30
EOF
  cat > "$CONF_DIR/db.credentials" <<EOF
DB_NAME=${DB_NAME}
DB_USER=${DB_USER}
DB_PASS=${DB_PASS}
EOF
  chmod 600 "$CONF_DIR/config.yaml" "$CONF_DIR/db.credentials"
else
  info "保留已有配置 ${CONF_DIR}/config.yaml"
fi
chown -R k2pay:k2pay "$CONF_DIR"
[[ -n "${DOMAIN:-}" ]] && echo "SITE_URL=https://${DOMAIN}" > "$CONF_DIR/site.env"

# config loader looks at /etc/k2pay — ensure viper finds it
# config.go has /etc/k2pay path after rename

info "配置 systemd..."
cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=K2Pay Payment Gateway
After=network-online.target mysql.service mariadb.service mysqld.service
Wants=network-online.target

[Service]
Type=simple
User=k2pay
Group=k2pay
WorkingDirectory=${CONF_DIR}
ExecStart=${BIN_PATH}
Restart=on-failure
RestartSec=5
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable k2pay
systemctl restart k2pay || true
sleep 2
if systemctl is-active --quiet k2pay; then info "服务已启动"; else
  warn "服务可能未就绪，日志: journalctl -u k2pay -n 40 --no-pager"
fi

if [[ "$SKIP_NGINX" -eq 0 && -n "${DOMAIN:-}" ]]; then
  info "配置 Nginx 反代 ${DOMAIN}..."
  NGINX_CONF=""
  if [[ -d /etc/nginx/sites-available ]]; then
    NGINX_CONF="/etc/nginx/sites-available/k2pay"
    cat > "$NGINX_CONF" <<EOF
server {
    listen 80;
    server_name ${DOMAIN};
    client_max_body_size 50m;
    location / {
        proxy_pass http://127.0.0.1:${HTTP_PORT};
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;
        proxy_http_version 1.1;
        proxy_read_timeout 120s;
    }
}
EOF
    ln -sfn "$NGINX_CONF" /etc/nginx/sites-enabled/k2pay
  else
    NGINX_CONF="/etc/nginx/conf.d/k2pay.conf"
    cat > "$NGINX_CONF" <<EOF
server {
    listen 80;
    server_name ${DOMAIN};
    client_max_body_size 50m;
    location / {
        proxy_pass http://127.0.0.1:${HTTP_PORT};
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;
        proxy_http_version 1.1;
        proxy_read_timeout 120s;
    }
}
EOF
  fi
  nginx -t && systemctl reload nginx

  if [[ "$SKIP_CERT" -eq 0 && -n "${EMAIL:-}" ]] && command -v certbot >/dev/null 2>&1; then
    info "申请 HTTPS 证书..."
    certbot --nginx -d "$DOMAIN" --non-interactive --agree-tos -m "$EMAIL" --redirect \
      || warn "证书申请失败，可稍后执行: certbot --nginx -d ${DOMAIN}"
  fi
fi

echo
info "========== 安装完成 =========="
echo "  二进制:   ${BIN_PATH}"
echo "  配置:     ${CONF_DIR}/config.yaml"
echo "  数据:     ${DATA_DIR}"
echo "  数据库:   ${DB_NAME} / ${DB_USER}  (密码见 ${CONF_DIR}/db.credentials)"
echo "  服务:     systemctl status k2pay"
if [[ -n "${DOMAIN:-}" ]]; then
  PROTO="http"; [[ "$SKIP_CERT" -eq 0 ]] && PROTO="https"
  echo "  管理后台: ${PROTO}://${DOMAIN}/admin"
  echo "  商户后台: ${PROTO}://${DOMAIN}/merchant"
  echo "  支付 API: ${PROTO}://${DOMAIN}/api/pay/"
else
  echo "  本机:     http://127.0.0.1:${HTTP_PORT}/admin"
fi
echo "  默认账号: admin / admin123  (请立即修改)"
echo "  卸载:     sudo bash uninstall.sh   # 保留数据库与证书"
echo "================================"
