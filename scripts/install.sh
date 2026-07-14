#!/usr/bin/env bash
# K2Pay 安装 / 重装
#   root 用户:
#     bash <(curl -fsSL https://raw.githubusercontent.com/HenZenKuriRIP/k2pay/main/scripts/install.sh)
#   普通用户:
#     curl -fsSL https://raw.githubusercontent.com/HenZenKuriRIP/k2pay/main/scripts/install.sh -o /tmp/k2pay-i.sh
#     sudo bash /tmp/k2pay-i.sh
set -euo pipefail

REPO="HenZenKuriRIP/k2pay"
BIN_PATH="/usr/local/bin/k2pay"
CONF_DIR="/etc/k2pay"
DATA_DIR="/var/lib/k2pay"
SERVICE_FILE="/etc/systemd/system/k2pay.service"
WEBROOT="/var/www/k2pay-acme"
APP_PORT="6088"
DB_NAME="k2pay"
DB_USER="k2pay"

DOMAIN=""
EMAIL=""
VERSION="latest"
SKIP_HTTPS=0
NO_NGINX=0
CLOUDFLARE=0

C0='\033[0m'; C1='\033[1;36m'; C2='\033[1;32m'; C3='\033[1;33m'; C4='\033[1;31m'
step()  { echo -e "\n${C1}▸ $*${C0}"; }
ok()    { echo -e "  ${C2}✓${C0} $*"; }
warn()  { echo -e "  ${C3}!${C0} $*"; }
die()   { echo -e "  ${C4}✗ $*${C0}" >&2; exit 1; }
quiet() { "$@" >/dev/null 2>&1; }

# curl|bash / process substitution 时 stdin 非终端，交互从 /dev/tty 读
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

usage() {
  cat <<EOF
K2Pay 安装

  # root
  bash <(curl -fsSL https://raw.githubusercontent.com/HenZenKuriRIP/k2pay/main/scripts/install.sh)

  # 普通用户
  curl -fsSL https://raw.githubusercontent.com/HenZenKuriRIP/k2pay/main/scripts/install.sh -o /tmp/k2pay-i.sh
  sudo bash /tmp/k2pay-i.sh --domain pay.example.com

选项:
  --domain FQDN     域名（不传则询问）
  --email EMAIL     证书邮箱（默认 admin@k2pay.com）
  --version TAG     默认 latest
  --skip-https      不要 HTTPS
  --no-nginx        仅本机 :${APP_PORT}
  --cloudflare      域名经 Cloudflare 橙云代理：生成 real_ip 配置并开启 trust_cloudflare
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --domain) DOMAIN="${2:-}"; shift 2 ;;
    --email)  EMAIL="${2:-}"; shift 2 ;;
    --version) VERSION="${2:-}"; shift 2 ;;
    --skip-https) SKIP_HTTPS=1; shift ;;
    --no-nginx) NO_NGINX=1; SKIP_HTTPS=1; shift ;;
    --cloudflare) CLOUDFLARE=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "未知参数: $1 （见 --help）" ;;
  esac
done

if [[ "$(id -u)" -ne 0 ]]; then
  die "请用 root 执行，或: curl -fsSL https://raw.githubusercontent.com/${REPO}/main/scripts/install.sh -o /tmp/k2pay-i.sh && sudo bash /tmp/k2pay-i.sh"
fi
[[ "$(uname -s)" == "Linux" ]] || die "仅支持 Linux"

ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) GOARCH=amd64 ;;
  aarch64|arm64) GOARCH=arm64 ;;
  *) die "不支持的架构: $ARCH" ;;
esac

if command -v apt-get >/dev/null 2>&1; then PKG=apt
elif command -v dnf >/dev/null 2>&1; then PKG=dnf
elif command -v yum >/dev/null 2>&1; then PKG=yum
else die "需要 apt / dnf / yum"; fi

export DEBIAN_FRONTEND=noninteractive

echo -e "${C1}"
echo "╔══════════════════════════════════════╗"
echo "║           K2Pay Installer            ║"
echo "╚══════════════════════════════════════╝"
echo -e "${C0}"

# ---- 交互输入（兼容 curl|bash，从 /dev/tty 读取）----
if [[ "$NO_NGINX" -eq 0 && -z "${DOMAIN}" ]]; then
  ask "域名（回车 = 仅本机访问）: "
  DOMAIN="${REPLY:-}"
  if [[ -z "${DOMAIN}" ]]; then
    NO_NGINX=1
    SKIP_HTTPS=1
    warn "未填域名，仅监听 127.0.0.1:${APP_PORT}"
  fi
fi

if [[ "$SKIP_HTTPS" -eq 0 && -n "${DOMAIN:-}" && -z "${EMAIL}" ]]; then
  ask "证书邮箱 [admin@k2pay.com]: "
  EMAIL="${REPLY:-admin@k2pay.com}"
fi
EMAIL="${EMAIL:-admin@k2pay.com}"

# ---- 1. 依赖 ----
step "安装系统依赖"
pkg_install() {
  if [[ "$PKG" == "apt" ]]; then
    quiet apt-get update -y || true
    quiet apt-get install -y "$@"
  else
    quiet $PKG install -y "$@" || true
  fi
}

pkg_install curl ca-certificates tar gzip openssl python3 file
command -v curl >/dev/null || die "curl 安装失败"

if ! command -v psql >/dev/null 2>&1; then
  if [[ "$PKG" == "apt" ]]; then
    quiet apt-get install -y postgresql postgresql-contrib
  else
    quiet $PKG install -y postgresql postgresql-contrib || quiet $PKG install -y postgresql-server
  fi
fi
quiet systemctl enable --now postgresql || quiet systemctl enable --now postgresql@* || true
ok "PostgreSQL"

if [[ "$NO_NGINX" -eq 0 ]]; then
  command -v nginx >/dev/null 2>&1 || pkg_install nginx
  quiet systemctl enable --now nginx || true
  ok "Nginx"
  if [[ "$SKIP_HTTPS" -eq 0 ]]; then
    command -v certbot >/dev/null 2>&1 || pkg_install certbot
    ok "Certbot"
  fi
fi

# ---- 2. 防火墙（证书需要 80）----
if [[ "$NO_NGINX" -eq 0 ]]; then
  step "开放防火墙端口"
  if command -v ufw >/dev/null 2>&1; then
    quiet ufw allow 80/tcp || true
    quiet ufw allow 443/tcp || true
    # 若 ufw 未启用则不强行 enable，避免锁死 SSH
    if ufw status 2>/dev/null | grep -qi "Status: active"; then
      ok "ufw: 已放行 80/443"
    else
      warn "ufw 未启用；若云厂商有安全组，请手动放行 80/443"
    fi
  elif command -v firewall-cmd >/dev/null 2>&1; then
    quiet firewall-cmd --permanent --add-service=http || true
    quiet firewall-cmd --permanent --add-service=https || true
    quiet firewall-cmd --reload || true
    ok "firewalld: 已放行 http/https"
  else
    warn "未检测到 ufw/firewalld，请确认 80/443 可从公网访问"
  fi
fi

# ---- 3. 数据库 ----
step "配置 PostgreSQL"
DB_PASS=""
if [[ -f "$CONF_DIR/db.credentials" ]]; then
  # shellcheck disable=SC1090
  source "$CONF_DIR/db.credentials" 2>/dev/null || true
fi
if [[ -z "${DB_PASS:-}" ]]; then
  DB_PASS="$(openssl rand -base64 24 | tr -dc 'a-zA-Z0-9' | head -c 20)"
fi

pg() {
  if id postgres >/dev/null 2>&1; then
    sudo -u postgres "$@"
  else
    "$@"
  fi
}

if ! pg psql -tAc "SELECT 1 FROM pg_roles WHERE rolname='${DB_USER}'" 2>/dev/null | grep -q 1; then
  pg psql -v ON_ERROR_STOP=1 -c "CREATE USER ${DB_USER} WITH PASSWORD '${DB_PASS}';" \
    || die "创建数据库用户失败"
else
  pg psql -c "ALTER USER ${DB_USER} WITH PASSWORD '${DB_PASS}';" >/dev/null 2>&1 || true
fi

if ! pg psql -tAc "SELECT 1 FROM pg_database WHERE datname='${DB_NAME}'" 2>/dev/null | grep -q 1; then
  pg psql -v ON_ERROR_STOP=1 -c "CREATE DATABASE ${DB_NAME} OWNER ${DB_USER};" \
    || die "创建数据库失败"
fi
pg psql -d "${DB_NAME}" -c "GRANT ALL ON SCHEMA public TO ${DB_USER};" >/dev/null 2>&1 || true
pg psql -d "${DB_NAME}" -c "ALTER DATABASE ${DB_NAME} OWNER TO ${DB_USER};" >/dev/null 2>&1 || true

# 允许本机密码登录（若 pg_hba 仅 peer，TCP 会失败）
PG_HBA="$(pg psql -tAc 'SHOW hba_file' 2>/dev/null | tr -d '[:space:]' || true)"
if [[ -n "$PG_HBA" && -f "$PG_HBA" ]]; then
  if ! grep -qE "host\s+${DB_NAME}\s+${DB_USER}\s+127\.0\.0\.1/32" "$PG_HBA" 2>/dev/null; then
    echo "host ${DB_NAME} ${DB_USER} 127.0.0.1/32 scram-sha-256" >> "$PG_HBA"
    echo "host ${DB_NAME} ${DB_USER} ::1/128 scram-sha-256" >> "$PG_HBA"
    quiet systemctl reload postgresql || quiet systemctl reload postgresql@* || true
  fi
fi
ok "数据库 ${DB_NAME} / 用户 ${DB_USER}"

# ---- 4. 下载二进制 ----
step "获取 K2Pay 程序 (${GOARCH})"
systemctl stop k2pay 2>/dev/null || true

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

fetch_bin() {
  local tag="$1" u f
  for u in \
    "https://github.com/${REPO}/releases/download/${tag}/k2pay-linux-${GOARCH}.tar.gz" \
    "https://github.com/${REPO}/releases/download/${tag}/k2pay-linux-${GOARCH}"
  do
    if curl -fsSL "$u" -o "$TMPDIR/dl" 2>/dev/null; then
      if file "$TMPDIR/dl" 2>/dev/null | grep -qiE 'gzip|tar'; then
        # macOS 打的包可能带 xattr 警告，忽略即可
        tar -xzf "$TMPDIR/dl" -C "$TMPDIR" 2>/dev/null || tar -xzf "$TMPDIR/dl" -C "$TMPDIR"
        f="$(find "$TMPDIR" -type f -name 'k2pay*' ! -name '*.tar.gz' ! -name '*.tgz' | head -1)"
        [[ -n "$f" ]] && cp "$f" "$TMPDIR/k2pay" && return 0
      else
        cp "$TMPDIR/dl" "$TMPDIR/k2pay" && return 0
      fi
    fi
  done
  return 1
}

TAG="$VERSION"
if [[ "$VERSION" == "latest" ]]; then
  TAG="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('tag_name',''))" 2>/dev/null || true)"
fi

if [[ -n "${TAG:-}" ]] && fetch_bin "$TAG"; then
  ok "已下载 Release ${TAG}"
else
  warn "Release 不可用，尝试从源码编译…"
  command -v go >/dev/null 2>&1 || pkg_install golang-go git || pkg_install golang git
  command -v git >/dev/null 2>&1 || pkg_install git
  git clone --depth 1 "https://github.com/${REPO}.git" "$TMPDIR/src" >/dev/null 2>&1 \
    || die "克隆源码失败"
  (
    cd "$TMPDIR/src"
    CGO_ENABLED=0 go build -ldflags="-s -w -X main.Version=$(git describe --tags --always 2>/dev/null || echo dev)" \
      -o "$TMPDIR/k2pay" .
  ) || die "编译失败"
  ok "源码编译完成"
fi

[[ -f "$TMPDIR/k2pay" ]] || die "未找到二进制"
chmod +x "$TMPDIR/k2pay"
install -m 755 "$TMPDIR/k2pay" "$BIN_PATH"
id k2pay &>/dev/null || useradd --system --home "$DATA_DIR" --shell /usr/sbin/nologin k2pay
mkdir -p "$DATA_DIR/qrcode" "$DATA_DIR/apk" "$CONF_DIR"
chown -R k2pay:k2pay "$DATA_DIR"
ok "安装到 ${BIN_PATH}"

# ---- 5. 配置文件 ----
step "写入配置"
JWT_SECRET="$(openssl rand -hex 32)"
CF_YAML="false"
[[ "${CLOUDFLARE}" -eq 1 ]] && CF_YAML="true"
if [[ ! -f "$CONF_DIR/config.yaml" ]]; then
  cat > "$CONF_DIR/config.yaml" <<EOF
# K2Pay — generated by install.sh
server:
  host: "127.0.0.1"
  port: ${APP_PORT}

database:
  host: "127.0.0.1"
  port: 5432
  user: "${DB_USER}"
  password: "${DB_PASS}"
  dbname: "${DB_NAME}"
  sslmode: "disable"
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
  trusted_proxies:
    - "127.0.0.1"
    - "::1"
  trust_cloudflare: ${CF_YAML}

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
  ok "新建 ${CONF_DIR}/config.yaml"
else
  # 确保密码与凭证一致（若用户手动改过 config 则不覆盖）
  ok "保留已有 ${CONF_DIR}/config.yaml"
  if [[ "${CLOUDFLARE}" -eq 1 ]]; then
    if grep -qE '^\s*trust_cloudflare:' "$CONF_DIR/config.yaml" 2>/dev/null; then
      sed -i 's/^\(\s*trust_cloudflare:\s*\).*/\1true/' "$CONF_DIR/config.yaml" || true
    else
      # 在 security 段末尾附近追加（简单追加到文件 security 关键字后靠后的位置）
      if grep -qE '^\s*security:' "$CONF_DIR/config.yaml"; then
        sed -i '/^security:/a\  trust_cloudflare: true' "$CONF_DIR/config.yaml" 2>/dev/null || \
          echo "  trust_cloudflare: true" >> "$CONF_DIR/config.yaml"
      else
        printf '\nsecurity:\n  trust_cloudflare: true\n  trusted_proxies:\n    - "127.0.0.1"\n    - "::1"\n' >> "$CONF_DIR/config.yaml"
      fi
    fi
    if ! grep -qE '^\s*trusted_proxies:' "$CONF_DIR/config.yaml" 2>/dev/null; then
      sed -i '/^security:/a\  trusted_proxies:\n    - "127.0.0.1"\n    - "::1"' "$CONF_DIR/config.yaml" 2>/dev/null || true
    fi
    ok "已开启 security.trust_cloudflare: true"
  fi
fi

cat > "$CONF_DIR/db.credentials" <<EOF
DB_NAME=${DB_NAME}
DB_USER=${DB_USER}
DB_PASS=${DB_PASS}
EOF
chmod 600 "$CONF_DIR/config.yaml" "$CONF_DIR/db.credentials"
chown -R k2pay:k2pay "$CONF_DIR"

if [[ -n "${DOMAIN:-}" ]]; then
  if [[ "$SKIP_HTTPS" -eq 0 ]]; then
    echo "SITE_URL=https://${DOMAIN}" > "$CONF_DIR/site.env"
  else
    echo "SITE_URL=http://${DOMAIN}" > "$CONF_DIR/site.env"
  fi
fi

# ---- 6. systemd ----
step "配置 systemd 服务"
cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=K2Pay Payment Gateway
After=network-online.target postgresql.service
Wants=network-online.target

[Service]
Type=simple
User=k2pay
Group=k2pay
WorkingDirectory=${CONF_DIR}
ExecStart=${BIN_PATH}
Restart=on-failure
RestartSec=3
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable k2pay >/dev/null 2>&1
systemctl restart k2pay
sleep 2
if systemctl is-active --quiet k2pay; then
  ok "k2pay 服务运行中"
else
  warn "服务启动失败，查看: journalctl -u k2pay -n 30 --no-pager"
  journalctl -u k2pay -n 15 --no-pager 2>/dev/null || true
fi

# ---- 7. Nginx + HTTPS ----
HAS_HTTPS=0
if [[ "$NO_NGINX" -eq 0 && -n "${DOMAIN:-}" ]]; then
  step "配置 Nginx (${DOMAIN})"

  if [[ -d /etc/nginx/sites-available ]]; then
    NGINX_CONF="/etc/nginx/sites-available/k2pay"
    mkdir -p /etc/nginx/sites-enabled
  else
    NGINX_CONF="/etc/nginx/conf.d/k2pay.conf"
  fi

  # 清理旧 k2pay / default 冲突
  rm -f /etc/nginx/sites-enabled/k2pay /etc/nginx/sites-enabled/k2payy \
        /etc/nginx/sites-available/k2pay /etc/nginx/conf.d/k2pay.conf
  rm -f /etc/nginx/sites-enabled/default /etc/nginx/sites-enabled/default.conf 2>/dev/null || true
  mkdir -p "$WEBROOT"

  # Cloudflare real_ip 片段
  CF_REALIP_INC=""
  CF_EXTRA_HEADERS=""
  if [[ "${CLOUDFLARE}" -eq 1 ]]; then
    mkdir -p /etc/nginx/snippets
    CF_SNIPPET="/etc/nginx/snippets/cloudflare-realip.conf"
    step "生成 Cloudflare real_ip 配置"
    if curl -fsSL --max-time 20 \
        "https://raw.githubusercontent.com/${REPO}/main/scripts/update-cloudflare-realip.sh" \
        -o /tmp/k2pay-cf-realip.sh 2>/dev/null; then
      bash /tmp/k2pay-cf-realip.sh "$CF_SNIPPET" || warn "在线生成 CF real_ip 失败，将写最小配置"
    fi
    if [[ ! -f "$CF_SNIPPET" ]] || ! grep -q 'set_real_ip_from' "$CF_SNIPPET" 2>/dev/null; then
      # 离线最小占位（仍写 real_ip_header；建议稍后跑 update 脚本）
      cat > "$CF_SNIPPET" <<'CFE'
# 请运行: bash scripts/update-cloudflare-realip.sh
# 以写入完整 Cloudflare IP 段
real_ip_header CF-Connecting-IP;
real_ip_recursive on;
CFE
      warn "CF IP 段未拉取完整，请稍后执行 update-cloudflare-realip.sh"
    else
      ok "已写入 $CF_SNIPPET"
    fi
    CF_REALIP_INC="include ${CF_SNIPPET};"
    CF_EXTRA_HEADERS="proxy_set_header CF-Connecting-IP \$http_cf_connecting_ip;"
  fi

  write_http() {
    cat > "$NGINX_CONF" <<EOF
# K2Pay (HTTP)$([[ "${CLOUDFLARE}" -eq 1 ]] && echo " + Cloudflare real_ip")
server {
    listen 80;
    listen [::]:80;
    server_name ${DOMAIN};
    client_max_body_size 50m;
    ${CF_REALIP_INC}
    location ^~ /.well-known/acme-challenge/ {
        root ${WEBROOT};
        default_type text/plain;
        allow all;
    }
    location / {
        proxy_pass http://127.0.0.1:${APP_PORT};
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;
        ${CF_EXTRA_HEADERS}
        proxy_http_version 1.1;
        proxy_read_timeout 120s;
    }
}
EOF
  }

  write_https() {
    local ssl_opts="" ssl_dh=""
    [[ -f /etc/letsencrypt/options-ssl-nginx.conf ]] && ssl_opts="include /etc/letsencrypt/options-ssl-nginx.conf;"
    [[ -f /etc/letsencrypt/ssl-dhparams.pem ]] && ssl_dh="ssl_dhparam /etc/letsencrypt/ssl-dhparams.pem;"
    cat > "$NGINX_CONF" <<EOF
# K2Pay (HTTPS)$([[ "${CLOUDFLARE}" -eq 1 ]] && echo " + Cloudflare real_ip")
server {
    listen 80;
    listen [::]:80;
    server_name ${DOMAIN};
    location ^~ /.well-known/acme-challenge/ {
        root ${WEBROOT};
        default_type text/plain;
        allow all;
    }
    location / { return 301 https://\$host\$request_uri; }
}
server {
    listen 443 ssl http2;
    listen [::]:443 ssl http2;
    server_name ${DOMAIN};
    ssl_certificate     /etc/letsencrypt/live/${DOMAIN}/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/${DOMAIN}/privkey.pem;
    ${ssl_opts}
    ${ssl_dh}
    client_max_body_size 50m;
    ${CF_REALIP_INC}
    location / {
        proxy_pass http://127.0.0.1:${APP_PORT};
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;
        ${CF_EXTRA_HEADERS}
        proxy_http_version 1.1;
        proxy_read_timeout 120s;
    }
}
EOF
  }

  write_http
  if [[ -d /etc/nginx/sites-enabled ]]; then
    ln -sfn "$NGINX_CONF" /etc/nginx/sites-enabled/k2pay
  fi
  nginx -t >/dev/null 2>&1 || die "Nginx 配置检测失败"
  quiet systemctl reload nginx || quiet systemctl restart nginx
  ok "HTTP 反代就绪"

  if [[ "$SKIP_HTTPS" -eq 0 ]]; then
    if [[ -f "/etc/letsencrypt/live/${DOMAIN}/fullchain.pem" ]]; then
      write_https
      nginx -t >/dev/null 2>&1 && quiet systemctl reload nginx
      HAS_HTTPS=1
      ok "已挂载已有证书"
    elif command -v certbot >/dev/null 2>&1; then
      echo "  正在申请证书（邮箱: ${EMAIL}）…"
      if certbot certonly --webroot \
          -w "$WEBROOT" -d "$DOMAIN" \
          --non-interactive --agree-tos -m "$EMAIL" \
          --preferred-challenges http >/dev/null 2>&1; then
        write_https
        nginx -t >/dev/null 2>&1 && quiet systemctl reload nginx
        HAS_HTTPS=1
        ok "HTTPS 证书已签发"
      else
        warn "证书申请失败（检查域名解析与 80 端口后重新运行本脚本）"
      fi
    else
      warn "未安装 certbot，保持 HTTP"
    fi
  fi
fi

# ---- 完成 ----
echo
echo -e "${C2}╔══════════════════════════════════════╗"
echo -e "║           安装完成                   ║"
echo -e "╚══════════════════════════════════════╝${C0}"
echo
echo "  服务状态:  systemctl status k2pay"
echo "  配置文件:  ${CONF_DIR}/config.yaml"
echo "  数据库:    PostgreSQL ${DB_NAME}（密码见 ${CONF_DIR}/db.credentials）"
if [[ -n "${DOMAIN:-}" && "$NO_NGINX" -eq 0 ]]; then
  if [[ "$HAS_HTTPS" -eq 1 ]]; then
    echo "  管理后台:  https://${DOMAIN}/admin"
    echo "  商户后台:  https://${DOMAIN}/merchant"
  else
    echo "  管理后台:  http://${DOMAIN}/admin"
    echo "  商户后台:  http://${DOMAIN}/merchant"
  fi
else
  echo "  管理后台:  http://127.0.0.1:${APP_PORT}/admin"
  echo "  商户后台:  http://127.0.0.1:${APP_PORT}/merchant"
fi
echo "  默认账号:  admin / admin123  ← 请立即修改"
echo
echo "  卸载: bash <(curl -fsSL https://raw.githubusercontent.com/${REPO}/main/scripts/uninstall.sh)"
echo
