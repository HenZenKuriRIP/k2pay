#!/usr/bin/env bash
# K2Pay 一键安装 / 重装
# - 拉取二进制、MySQL、systemd
# - 清空冲突的 Nginx 站点配置后写入干净反代
# - 若已有 Let's Encrypt 证书则直接挂载；否则自动申请
# - 不使用 certbot --nginx（避免改写 default 导致 301 死循环）
#
# 用法:
#   curl -fsSL https://raw.githubusercontent.com/HenZenKuriRIP/k2pay/main/scripts/install.sh | \
#     sudo bash -s -- --domain pay.example.com --email you@example.com
#   sudo bash scripts/install.sh --domain pay.example.com --email you@example.com
#   sudo bash scripts/install.sh --domain pay.example.com --reset-nginx
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
WEBROOT="/var/www/k2pay-acme"

DOMAIN=""
EMAIL=""
SKIP_CERT=0
SKIP_NGINX=0
RESET_NGINX=1   # 默认重置 Nginx 站点配置（真正一键、避免 default 冲突）
VERSION="latest"
REINSTALL=0

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
    --reset-nginx) RESET_NGINX=1; shift ;;
    --keep-nginx) RESET_NGINX=0; shift ;;
    --reinstall) REINSTALL=1; shift ;;
    -h|--help)
      cat <<EOF
Usage: $0 [options]
  --domain FQDN       站点域名（强烈建议提供）
  --email EMAIL       Let's Encrypt 邮箱
  --version TAG       Release 版本，默认 latest
  --skip-cert         不申请/不挂载 HTTPS
  --skip-nginx        不配置 Nginx
  --reset-nginx       清空冲突站点并重写 Nginx（默认开启）
  --keep-nginx        不清理其它站点，仅写入 k2pay 配置
  --reinstall         重装：停旧服务、覆盖二进制、重写 Nginx
EOF
      exit 0 ;;
    *) die "未知参数: $1" ;;
  esac
done

[[ "$(id -u)" -eq 0 ]] || die "请使用 root: sudo bash $0"

if [[ -z "$DOMAIN" && "$SKIP_NGINX" -eq 0 ]]; then
  if [[ -t 0 ]]; then
    read -rp "域名 (必填才能自动配置 HTTPS，留空则仅本机 6088): " DOMAIN || true
  fi
  if [[ -z "${DOMAIN:-}" ]]; then
    SKIP_NGINX=1
    SKIP_CERT=1
    warn "未提供域名，跳过 Nginx/证书"
  fi
fi
if [[ -n "${DOMAIN:-}" && "$SKIP_CERT" -eq 0 && -z "${EMAIL:-}" ]]; then
  if [[ -t 0 ]]; then
    read -rp "Let's Encrypt 邮箱 (证书不存在时需要；已有证书可回车跳过): " EMAIL || true
  fi
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

# ---------- 重装：先停服务 ----------
if [[ "$REINSTALL" -eq 1 ]] || systemctl is-active --quiet k2pay 2>/dev/null; then
  info "停止已有 k2pay 服务..."
  systemctl stop k2pay 2>/dev/null || true
fi

# ---------- 依赖 ----------
export DEBIAN_FRONTEND=noninteractive
info "安装系统依赖..."
if [[ "$PKG" == "apt" ]]; then
  apt-get update -y
  apt-get install -y curl ca-certificates tar gzip openssl python3 file
  if ! command -v mysql >/dev/null 2>&1; then
    apt-get install -y mariadb-server mariadb-client || apt-get install -y mysql-server default-mysql-client
  fi
  systemctl enable --now mariadb 2>/dev/null || systemctl enable --now mysql 2>/dev/null || true
  if [[ "$SKIP_NGINX" -eq 0 ]]; then
    apt-get install -y nginx
    systemctl enable --now nginx
    apt-get install -y certbot 2>/dev/null || true
  fi
else
  $PKG install -y curl tar gzip openssl python3 file
  $PKG install -y mariadb-server 2>/dev/null || $PKG install -y mysql-server 2>/dev/null || true
  systemctl enable --now mariadb 2>/dev/null || systemctl enable --now mysqld 2>/dev/null || true
  if [[ "$SKIP_NGINX" -eq 0 ]]; then
    $PKG install -y nginx
    systemctl enable --now nginx
    $PKG install -y certbot 2>/dev/null || true
  fi
fi

# ---------- 数据库 ----------
info "配置 MySQL/MariaDB..."
DB_PASS=""
if [[ -f "$CONF_DIR/db.credentials" ]]; then
  # 重装保留原数据库密码
  # shellcheck disable=SC1090
  source "$CONF_DIR/db.credentials" 2>/dev/null || true
  DB_PASS="${DB_PASS:-}"
fi
if [[ -z "$DB_PASS" ]]; then
  DB_PASS="$(openssl rand -base64 24 | tr -dc 'a-zA-Z0-9' | head -c 20)"
fi

mysql_root() { mysql -uroot "$@"; }
mysql_root -e "CREATE DATABASE IF NOT EXISTS \`${DB_NAME}\` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;" \
  || die "无法连接 MySQL root，请先配置 root 本地登录"
mysql_root -e "CREATE USER '${DB_USER}'@'localhost' IDENTIFIED BY '${DB_PASS}';" 2>/dev/null \
  || mysql_root -e "ALTER USER '${DB_USER}'@'localhost' IDENTIFIED BY '${DB_PASS}';" 2>/dev/null \
  || mysql_root -e "SET PASSWORD FOR '${DB_USER}'@'localhost' = PASSWORD('${DB_PASS}');" 2>/dev/null \
  || true
mysql_root -e "GRANT ALL PRIVILEGES ON \`${DB_NAME}\`.* TO '${DB_USER}'@'localhost'; FLUSH PRIVILEGES;"
if ! mysql -u"${DB_USER}" -p"${DB_PASS}" -e "USE ${DB_NAME};" 2>/dev/null; then
  mysql_root -e "DROP USER IF EXISTS '${DB_USER}'@'localhost';" 2>/dev/null || true
  mysql_root -e "CREATE USER '${DB_USER}'@'localhost' IDENTIFIED BY '${DB_PASS}'; GRANT ALL ON \`${DB_NAME}\`.* TO '${DB_USER}'@'localhost'; FLUSH PRIVILEGES;"
  mysql -u"${DB_USER}" -p"${DB_PASS}" -e "USE ${DB_NAME};" || die "数据库用户创建失败"
fi

# ---------- 二进制 ----------
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
      if file "$TMPDIR/dl" 2>/dev/null | grep -qiE 'gzip|tar archive'; then
        tar -xzf "$TMPDIR/dl" -C "$TMPDIR"
        local f
        f="$(find "$TMPDIR" -type f -name 'k2pay*' ! -name '*.tar.gz' ! -name '*.tgz' | head -1)"
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
  TAG="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('tag_name',''))" 2>/dev/null || true)"
  if [[ -n "$TAG" ]] && fetch_binary "$TAG"; then GOT=1; info "已下载 Release ${TAG}"; fi
else
  fetch_binary "$VERSION" && GOT=1 && info "已下载 Release ${VERSION}"
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
  ) || die "源码编译失败。请发布 Release: k2pay-linux-${GOARCH}.tar.gz"
fi

[[ -f "$TMPDIR/k2pay" ]] || die "未找到二进制文件"
chmod +x "$TMPDIR/k2pay"
mkdir -p "$INSTALL_DIR" "$DATA_DIR/qrcode" "$DATA_DIR/apk" "$CONF_DIR"
install -m 755 "$TMPDIR/k2pay" "$BIN_PATH"
id k2pay &>/dev/null || useradd --system --home "$DATA_DIR" --shell /usr/sbin/nologin k2pay
chown -R k2pay:k2pay "$DATA_DIR" "$INSTALL_DIR"

# ---------- 应用配置 ----------
JWT_SECRET="$(openssl rand -hex 32)"
if [[ ! -f "$CONF_DIR/config.yaml" ]] || [[ "$REINSTALL" -eq 1 && ! -f "$CONF_DIR/config.yaml" ]]; then
  :
fi
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
else
  info "保留已有 ${CONF_DIR}/config.yaml"
fi

cat > "$CONF_DIR/db.credentials" <<EOF
DB_NAME=${DB_NAME}
DB_USER=${DB_USER}
DB_PASS=${DB_PASS}
EOF
chmod 600 "$CONF_DIR/config.yaml" "$CONF_DIR/db.credentials" 2>/dev/null || true
chown -R k2pay:k2pay "$CONF_DIR"

if [[ -n "${DOMAIN:-}" ]]; then
  if [[ "$SKIP_CERT" -eq 0 ]]; then
    echo "SITE_URL=https://${DOMAIN}" > "$CONF_DIR/site.env"
  else
    echo "SITE_URL=http://${DOMAIN}" > "$CONF_DIR/site.env"
  fi
fi

# ---------- systemd ----------
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
if systemctl is-active --quiet k2pay; then info "k2pay 服务已启动"; else
  warn "服务可能未就绪: journalctl -u k2pay -n 40 --no-pager"
fi

# ---------- Nginx 工具函数 ----------
nginx_backup_dir="/root/k2pay-nginx-backup-$(date +%Y%m%d%H%M%S)"

detect_nginx_layout() {
  if [[ -d /etc/nginx/sites-available ]]; then
    NGINX_LAYOUT="debian"
    NGINX_CONF="/etc/nginx/sites-available/k2pay"
    NGINX_ENABLED="/etc/nginx/sites-enabled/k2pay"
  else
    NGINX_LAYOUT="rhel"
    NGINX_CONF="/etc/nginx/conf.d/k2pay.conf"
    NGINX_ENABLED=""
  fi
}

# 清理会与本域名冲突的站点（保留证书目录 /etc/letsencrypt）
reset_nginx_sites() {
  local domain="$1"
  info "清理冲突的 Nginx 站点配置（保留 Let's Encrypt 证书）..."
  mkdir -p "$nginx_backup_dir"

  # 备份并移除 k2pay 旧配置
  for f in \
    /etc/nginx/sites-available/k2pay \
    /etc/nginx/sites-enabled/k2pay \
    /etc/nginx/sites-enabled/k2payy \
    /etc/nginx/conf.d/k2pay.conf \
    /etc/nginx/conf.d/k2pay.conf.bak
  do
    if [[ -e "$f" || -L "$f" ]]; then
      cp -a "$f" "$nginx_backup_dir/" 2>/dev/null || true
      rm -f "$f"
    fi
  done

  # 禁用 default（Certbot 常把证书错装到 default 导致 301 死循环）
  for f in /etc/nginx/sites-enabled/default /etc/nginx/sites-enabled/default.conf; do
    if [[ -e "$f" || -L "$f" ]]; then
      cp -a "$f" "$nginx_backup_dir/" 2>/dev/null || true
      rm -f "$f"
      info "已禁用: $f （备份在 $nginx_backup_dir）"
    fi
  done
  # sites-available/default 保留文件但取消启用即可
  if [[ -f /etc/nginx/sites-available/default ]]; then
    cp -a /etc/nginx/sites-available/default "$nginx_backup_dir/" 2>/dev/null || true
  fi

  # 删除 conf.d 里包含本域名的其它自定义文件（不碰 nginx.conf）
  if [[ -d /etc/nginx/conf.d ]]; then
    local f
    for f in /etc/nginx/conf.d/*.conf; do
      [[ -f "$f" ]] || continue
      if grep -q "server_name.*${domain}" "$f" 2>/dev/null; then
        info "移除含本域名的配置: $f"
        cp -a "$f" "$nginx_backup_dir/" 2>/dev/null || true
        rm -f "$f"
      fi
    done
  fi

  # sites-enabled 里其它含本域名的配置
  if [[ -d /etc/nginx/sites-enabled ]]; then
    local f
    for f in /etc/nginx/sites-enabled/*; do
      [[ -e "$f" || -L "$f" ]] || continue
      local real="$f"
      [[ -L "$f" ]] && real="$(readlink -f "$f" 2>/dev/null || echo "$f")"
      if grep -q "server_name.*${domain}" "$real" 2>/dev/null || grep -q "server_name.*${domain}" "$f" 2>/dev/null; then
        info "移除含本域名的站点: $f"
        cp -a "$f" "$nginx_backup_dir/" 2>/dev/null || true
        rm -f "$f"
      fi
    done
  fi
}

cert_exists() {
  local domain="$1"
  [[ -f "/etc/letsencrypt/live/${domain}/fullchain.pem" && -f "/etc/letsencrypt/live/${domain}/privkey.pem" ]]
}

# 写入 HTTP-only（用于申请证书或 --skip-cert）
write_nginx_http_only() {
  local domain="$1"
  local conf="$2"
  cat > "$conf" <<EOF
# K2Pay — managed by install.sh (HTTP)
server {
    listen 80;
    listen [::]:80;
    server_name ${domain};

    client_max_body_size 50m;

    location ^~ /.well-known/acme-challenge/ {
        root ${WEBROOT};
        default_type "text/plain";
        allow all;
    }

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
}

# 写入完整 HTTP→HTTPS + 反代（证书已存在时使用）
write_nginx_https() {
  local domain="$1"
  local conf="$2"
  local ssl_opts=""
  local ssl_dh=""
  [[ -f /etc/letsencrypt/options-ssl-nginx.conf ]] && ssl_opts="include /etc/letsencrypt/options-ssl-nginx.conf;"
  [[ -f /etc/letsencrypt/ssl-dhparams.pem ]] && ssl_dh="ssl_dhparam /etc/letsencrypt/ssl-dhparams.pem;"

  cat > "$conf" <<EOF
# K2Pay — managed by install.sh (HTTPS)
# 证书: /etc/letsencrypt/live/${domain}/
# 反代: 127.0.0.1:${HTTP_PORT}

server {
    listen 80;
    listen [::]:80;
    server_name ${domain};

    location ^~ /.well-known/acme-challenge/ {
        root ${WEBROOT};
        default_type "text/plain";
        allow all;
    }

    location / {
        return 301 https://\$host\$request_uri;
    }
}

server {
    listen 443 ssl http2;
    listen [::]:443 ssl http2;
    server_name ${domain};

    ssl_certificate     /etc/letsencrypt/live/${domain}/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/${domain}/privkey.pem;
    ${ssl_opts}
    ${ssl_dh}

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
}

enable_nginx_conf() {
  local conf="$1"
  if [[ "$NGINX_LAYOUT" == "debian" ]]; then
    mkdir -p /etc/nginx/sites-enabled /etc/nginx/sites-available
    ln -sfn "$conf" /etc/nginx/sites-enabled/k2pay
  fi
}

# ---------- Nginx + 证书 ----------
HAS_HTTPS=0
if [[ "$SKIP_NGINX" -eq 0 && -n "${DOMAIN:-}" ]]; then
  detect_nginx_layout
  mkdir -p "$WEBROOT"

  if [[ "$RESET_NGINX" -eq 1 ]]; then
    reset_nginx_sites "$DOMAIN"
  else
    rm -f /etc/nginx/sites-enabled/k2pay /etc/nginx/sites-available/k2pay /etc/nginx/conf.d/k2pay.conf
  fi

  # 先写 HTTP，保证 ACME 与 nginx -t 能过
  write_nginx_http_only "$DOMAIN" "$NGINX_CONF"
  enable_nginx_conf "$NGINX_CONF"
  nginx -t || die "Nginx 配置检测失败"
  systemctl reload nginx || systemctl restart nginx

  if [[ "$SKIP_CERT" -eq 0 ]]; then
    if cert_exists "$DOMAIN"; then
      info "检测到已有证书 /etc/letsencrypt/live/${DOMAIN}/ ，直接写入 HTTPS 反代配置"
      write_nginx_https "$DOMAIN" "$NGINX_CONF"
      enable_nginx_conf "$NGINX_CONF"
      nginx -t && systemctl reload nginx
      HAS_HTTPS=1
    else
      if [[ -z "${EMAIL:-}" ]]; then
        warn "无证书且未提供 --email，保持 HTTP。稍后可加邮箱重装申请证书。"
      elif ! command -v certbot >/dev/null 2>&1; then
        warn "未安装 certbot，保持 HTTP"
      else
        info "申请 Let's Encrypt 证书 (certonly --webroot，不改写其它站点)..."
        if certbot certonly --webroot \
            -w "$WEBROOT" \
            -d "$DOMAIN" \
            --non-interactive --agree-tos \
            -m "$EMAIL" \
            --preferred-challenges http; then
          write_nginx_https "$DOMAIN" "$NGINX_CONF"
          enable_nginx_conf "$NGINX_CONF"
          nginx -t && systemctl reload nginx
          HAS_HTTPS=1
          info "证书申请成功并已写入 Nginx"
        else
          warn "证书申请失败，保持 HTTP 反代。检查 80 端口与域名解析后重跑安装脚本。"
        fi
      fi
    fi
  else
    info "已 --skip-cert，仅 HTTP 反代"
  fi

  # 最终校验
  if nginx -t 2>/dev/null; then
    systemctl reload nginx 2>/dev/null || true
    info "Nginx 配置完成: $NGINX_CONF"
  else
    die "Nginx 最终检测失败，备份在: $nginx_backup_dir"
  fi
fi

# ---------- 完成 ----------
echo
info "========== 安装完成 =========="
echo "  二进制:   ${BIN_PATH}"
echo "  配置:     ${CONF_DIR}/config.yaml"
echo "  数据:     ${DATA_DIR}"
echo "  数据库:   ${DB_NAME} / ${DB_USER}  (密码见 ${CONF_DIR}/db.credentials)"
echo "  服务:     systemctl status k2pay"
if [[ -n "${DOMAIN:-}" && "$SKIP_NGINX" -eq 0 ]]; then
  if [[ "$HAS_HTTPS" -eq 1 ]]; then
    echo "  HTTPS:    已启用 (证书 /etc/letsencrypt/live/${DOMAIN}/)"
    echo "  管理后台: https://${DOMAIN}/admin"
    echo "  商户后台: https://${DOMAIN}/merchant"
    echo "  支付 API: https://${DOMAIN}/api/pay/"
  else
    echo "  HTTP:     http://${DOMAIN}/admin"
    echo "  提示:     证书未就绪时可用: sudo bash install.sh --domain ${DOMAIN} --email you@xx.com --reinstall"
  fi
  if [[ -d "$nginx_backup_dir" ]]; then
    echo "  Nginx备份: ${nginx_backup_dir}"
  fi
else
  echo "  本机:     http://127.0.0.1:${HTTP_PORT}/admin"
fi
echo "  默认账号: admin / admin123  (请立即修改)"
echo "  卸载:     curl -fsSL https://raw.githubusercontent.com/${REPO}/main/scripts/uninstall.sh | sudo bash"
echo "================================"
