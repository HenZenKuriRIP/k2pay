# K2Pay

多商户聚合收款网关。兼容**易支付协议语义**（签名、回调、查单），使用 **REST 风格 API**；支持加密货币（USDT/TRX 多链）、微信/支付宝个人码与官方扫码支付。

仓库: https://github.com/HenZenKuriRIP/k2pay

## 功能概览

- **商户体系**：管理员开户、`pid` / `key`、商户后台、余额与提现
- **支付**：`/api/pay/*` 下单 / 查单 / 退款（余额冲正）
- **通道**：链上 USDT/TRX、个人收款码、支付宝/微信官方 Native 扫码
- **链监控**：管理端可添加/编辑/禁用链，RPC 热加载，健康度 **0–100 评分**
- **官方支付**：支付宝当面付、微信 APIv3 Native；失败自动回退个人码
- **运维**：仪表盘服务器指标（CPU/内存/磁盘/负载/DB 连接池）、systemd、一键安装
- **技术栈**：**PostgreSQL**、Go 1.21+、嵌入式 Web 管理端/商户端

## 快速安装（Linux 服务器）

```bash
# 安装 / 重装（已是 root 时直接用，不要再套一层 sudo）
bash <(curl -fsSL https://raw.githubusercontent.com/HenZenKuriRIP/k2pay/main/scripts/install.sh)

# 指定域名
bash <(curl -fsSL https://raw.githubusercontent.com/HenZenKuriRIP/k2pay/main/scripts/install.sh) --domain pay.example.com

# 卸载
bash <(curl -fsSL https://raw.githubusercontent.com/HenZenKuriRIP/k2pay/main/scripts/uninstall.sh) -y
```

普通用户可先下载再 sudo：

```bash
curl -fsSL https://raw.githubusercontent.com/HenZenKuriRIP/k2pay/main/scripts/install.sh -o /tmp/k2pay-i.sh
sudo bash /tmp/k2pay-i.sh
```

> 不要用 `curl | bash`（占 stdin）；已是 `root@` 时不要再写 `sudo bash <(...)`（部分环境 process substitution 会失败）。

可选：`--domain` / `--email`（默认 `admin@k2pay.com`）/ `--version` / `--skip-https` / `--no-nginx` / **`--cloudflare`**（经 Cloudflare 代理时生成 real_ip 并开启 `trust_cloudflare`）；卸载 `--purge-all` 清库。

| 入口 | 地址 |
|------|------|
| 管理后台 | `https://你的域名/admin` |
| 商户后台 | `https://你的域名/merchant` |
| 默认管理员 | `admin` / `admin123`（请立刻修改） |

数据库密码见 `/etc/k2pay/db.credentials`。

---

## Cloudflare 接入指南（推荐公网部署）

将支付域名挂在 **Cloudflare 橙云代理** 后，可获得 DDoS 防护、WAF、边缘限速，并隐藏源站 IP。  
K2Pay **无需改对接协议**（签名 / pid / key / 路径不变）；必须正确还原 **真实客户端 IP**，否则 **商户 IP 白名单、全局 IP 黑名单、API 限流** 会失真。

### 架构

```
商户服务器 / 用户浏览器
        │
        ▼
  Cloudflare（橙云 Proxied）
    · WAF / Bot / Rate Limit / TLS
        │
        ▼
  源站 Nginx :80/:443
    · real_ip（CF-Connecting-IP → 真实 IP）
    · proxy_pass → 127.0.0.1:6088
        │
        ▼
  K2Pay（security.trust_cloudflare + trusted_proxies）
    · 白名单 / 黑名单 / 限流使用真实 IP
```

> 白名单校验在 **主程序** 内完成，**不会**动态改 Nginx 配置。Cloudflare 只是边缘防护 + 正确传 IP。

### 方式 A：安装时一键开启（推荐）

域名已在 Cloudflare 添加 DNS（A/AAAA 指向源站，代理状态为 **已代理 / 橙云**）：

```bash
# 安装并启用 Cloudflare 适配
bash <(curl -fsSL https://raw.githubusercontent.com/HenZenKuriRIP/k2pay/main/scripts/install.sh) \
  --domain pay.example.com \
  --cloudflare
```

脚本会：

1. 写入 `/etc/nginx/snippets/cloudflare-realip.conf`（从 Cloudflare 官方拉取 IP 段）  
2. Nginx `include` 该片段，并透传 `CF-Connecting-IP`  
3. `/etc/k2pay/config.yaml` 中设置：

```yaml
security:
  trusted_proxies:
    - "127.0.0.1"
    - "::1"
  trust_cloudflare: true
```

4. 应用仅监听 `127.0.0.1:6088`，由 Nginx 对外

### 方式 B：已有部署后手工接入

#### 1）Cloudflare 控制台

| 项 | 建议值 |
|----|--------|
| DNS | 支付域名 A/AAAA → 源站公网 IP，**Proxied（橙云）** |
| SSL/TLS | **Full (strict)** |
| 源站证书 | Let’s Encrypt，或 [Origin Certificate](https://developers.cloudflare.com/ssl/origin-configuration/origin-ca/) |
| 缓存 | `/api/*`、`/admin`、`/merchant`、`/cashier` **不缓存**（默认动态一般不缓存即可） |
| WAF / Rate limiting | 建议对 `/api/pay/*` 加边缘限速，减轻撞库扫 pid |

可选加强：

- **Firewall Rules / WAF**：拦明显扫描  
- **源站防火墙 / 安全组**：仅放行 [Cloudflare IP 段](https://www.cloudflare.com/ips/) 访问 80/443，禁止直连源站绕过 CF  
- **不要**把应用端口 `6088` 暴露到公网  

#### 2）源站 Nginx 还原真实 IP

```bash
# 生成 / 更新 Cloudflare IP 段（建议加入 cron 每月执行）
sudo bash scripts/update-cloudflare-realip.sh
# 默认输出: /etc/nginx/snippets/cloudflare-realip.conf
```

在支付站点的 `server { }` 中：

```nginx
include /etc/nginx/snippets/cloudflare-realip.conf;

location / {
    proxy_pass http://127.0.0.1:6088;
    proxy_set_header Host              $host;
    # real_ip 生效后 $remote_addr 已是访客/商户真实 IP
    proxy_set_header X-Real-IP         $remote_addr;
    proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header CF-Connecting-IP  $http_cf_connecting_ip;
    proxy_http_version 1.1;
    proxy_read_timeout 120s;
}
```

完整示例见仓库：`deploy/nginx/k2pay-cloudflare.example.conf`。

```bash
sudo nginx -t && sudo systemctl reload nginx
```

#### 3）K2Pay 配置

编辑 `/etc/k2pay/config.yaml`：

```yaml
security:
  # 信任本机 Nginx；ClientIP 只从可信反代的头里取
  trusted_proxies:
    - "127.0.0.1"
    - "::1"
  # 启用后优先读取 CF-Connecting-IP（需 Nginx 透传）
  trust_cloudflare: true
```

```bash
sudo systemctl restart k2pay
# 日志中应出现类似: Trusted proxies: [127.0.0.1 ::1] (Cloudflare CF-Connecting-IP enabled)
```

#### 4）验证真实 IP 是否正确

1. 用手机流量或另一台机器访问管理后台，或让商户机调一次 `/api/pay/create`  
2. 打开 **管理后台 → API 日志**，查看 **客户端 IP**  
3. 应等于发起方 **公网出口 IP**，而 **不是** Cloudflare 任播节点 IP  

若日志里全是 `104.x` / `172.64.x` 等 CF 段，说明 real_ip 或 `trust_cloudflare` 未生效，**先修好再开商户 IP 白名单**。

### 对商户对接的影响

| 项目 | 是否变化 |
|------|----------|
| 网关 URL | 仍为 `https://pay.example.com/api/pay/...`（你的域名） |
| 签名算法 MD5、pid、key | **不变** |
| 下单 / 查单 / 退款 / 回调字段 | **不变** |
| K2Pay → 商户 `notify_url` | **不受影响**（出站回调，不经商户白名单） |
| 商户 IP 白名单 | **仍填商户服务器真实出口 IP**（见下） |

#### 商户侧也使用了 Cloudflare 时

- 商户 **后端出站** 调 K2Pay 时，源 IP 一般是 **源站出口 IP**，不是 CF 节点 IP  
- 白名单请在商户服务器执行 `curl -4 ifconfig.me` 取得出口 IP 后填入  
- **不要**把「解析商户域名得到的 CF IP」当作白名单唯一来源  
- 管理后台「域名解析加 IP」对橙云域名 **不可靠**，优先用固定出口 IP  

### 定期维护

```bash
# 每月更新一次 Cloudflare IP 段（官方列表会变）
sudo bash /path/to/k2pay/scripts/update-cloudflare-realip.sh
```

或 crontab：

```cron
0 3 1 * * root bash /usr/local/share/k2pay/scripts/update-cloudflare-realip.sh >/var/log/k2pay-cf-realip.log 2>&1
```

### 常见问题

| 现象 | 原因与处理 |
|------|------------|
| 白名单已加服务器 IP 仍提示不在名单 | API 日志里 client IP 仍是 CF 节点 → 检查 Nginx `include cloudflare-realip`、`trust_cloudflare: true`、重启 k2pay |
| 证书错误 / 526 | Cloudflare SSL 用 Full (strict)，源站证书无效 → 用有效 LE 或 Origin CA |
| 能直连源站 IP 绕过 CF | 安全组未限制仅 CF IP 访问 80/443 |
| 管理后台也被全世界扫 | 可用 CF Access、国家限制，或 Nginx 对 `/admin` 做 IP allow |
| 回调（支付宝/微信）失败 | 回调 URL 必须用 **域名** 而非源站 IP；CF 代理下同样走域名即可 |

### 安全配置项速查

```yaml
security:
  trusted_proxies:     # 可信反代 CIDR/IP，空 = 不信任任何转发头
    - "127.0.0.1"
    - "::1"
  trust_cloudflare: true   # 是否在可信反代场景下读取 CF-Connecting-IP
```

相关文件：

| 文件 | 说明 |
|------|------|
| `scripts/update-cloudflare-realip.sh` | 拉取 CF IP 段生成 Nginx snippet |
| `deploy/nginx/k2pay-cloudflare.example.conf` | 完整 Nginx 示例 |
| `deploy/nginx/cloudflare-realip.conf` | snippet 说明占位 |

---

## 配置要点

配置文件：`/etc/k2pay/config.yaml` 或工作目录 `config.yaml`。

### 数据库（PostgreSQL）

```yaml
database:
  host: "127.0.0.1"
  port: 5432
  user: "k2pay"
  password: "your-password"
  dbname: "k2pay"
  sslmode: "disable"   # disable / require / verify-full
  max_open_conns: 100
  max_idle_conns: 10
  conn_max_lifetime: 60
```

- 首次启动自动 **AutoMigrate** 建表并写入默认数据  
- **不支持直连 MySQL**；从旧版 MySQL 升级需自行迁移数据  
- 数据目录：`/var/lib/k2pay`（上传文件、APK 等）

### 官方支付

管理后台 **系统设置 → 官方支付**：

1. 填写 `site_url`（公网 HTTPS，如 `https://pay.example.com`）  
2. 支付宝：模式选「官方」、AppID / 应用私钥 / 支付宝公钥  
3. 微信：模式选「官方」、AppID / 商户号 / 证书序列号 / API 私钥 / APIv3 密钥  
4. 保存后可用「测试连通」校验  
5. 开放平台/商户平台回调地址：  
   - `https://你的域名/api/channel/notify/alipay`  
   - `https://你的域名/api/channel/notify/wechat`  

法币默认个人收款码；改为「官方」后走开放平台扫码，失败时自动回退个人码。

### 链监控

管理后台 **链监控**：启用/禁用、编辑 RPC/扫描参数、添加 EVM 兼容链、健康度百分制检测。

---

## 下游商户对接说明

> 业务网站（发卡站、商城、SaaS 等）作为**下游商户**，通过 HTTP API 接入 K2Pay 收款。  
> 字段与签名规则对齐经典易支付，便于从旧易支付迁移（仅需改网关 URL，路径为 REST 风格）。

### 1. 获取对接凭证

1. 平台管理员在 **管理后台 → 商户管理** 创建商户  
2. 得到：
   - **pid**：商户号  
   - **key**：通讯密钥（商户后台「API 密钥」可查看/重置）  
3. 商户登录 `/merchant`，配置默认 `notify_url` / `return_url`（也可在每次下单时传入）  
4. 确保已开通支付能力（系统/商户钱包、个人码，或平台已配置官方支付宝/微信）

### 2. 接口一览

设网关根地址为 `https://pay.example.com`：

| 用途 | 方法 | 路径 |
|------|------|------|
| 表单跳转下单 | GET/POST | `/api/pay/submit` |
| API JSON 下单 | GET/POST | `/api/pay/create` |
| 商户信息/余额 | GET | `/api/pay/merchant` |
| 查询单笔订单 | GET | `/api/pay/order` |
| 订单列表 | GET | `/api/pay/orders` |
| 结算/提现记录 | GET | `/api/pay/settle` |
| 退款（余额冲正） | POST | `/api/pay/refund` |
| 可用支付方式 | GET | `/api/pay/types?pid=` |
| 用户收银台 | GET | `/cashier/{trade_no}` |

### 3. 签名算法（下单必做）

参与签名的参数：除 `sign`、`sign_type` 外，所有**非空**业务参数。

1. 按参数名 **ASCII 升序** 排序  
2. 拼成 `key1=value1&key2=value2`（**value 不做 URL 编码**）  
3. **末尾直接拼接**商户密钥 `key`（中间无 `&`）  
4. 对整串做 **MD5**，结果为 **32 位小写十六进制**，作为 `sign`

示例（伪代码）：

```text
# 参数: money=10.00 name=VIP pid=1001 type=alipay out_trade_no=A001
# notify_url=https://shop.com/notify return_url=https://shop.com/return
# 排序后拼接 + 密钥 secret_key：
money=10.00&name=VIP&notify_url=https://shop.com/notify&out_trade_no=A001&pid=1001&return_url=https://shop.com/return&type=alipaysecret_key
→ sign = md5(上述字符串)
```

PHP 示例：

```php
function k2pay_sign(array $params, string $key): string {
    ksort($params);
    $buf = [];
    foreach ($params as $k => $v) {
        if ($v === '' || $v === null) continue;
        if ($k === 'sign' || $k === 'sign_type') continue;
        $buf[] = $k . '=' . $v;
    }
    return md5(implode('&', $buf) . $key);
}
```

### 4. 发起支付

#### 4.1 页面跳转（推荐浏览器场景）

`GET/POST https://pay.example.com/api/pay/submit`

| 参数 | 必填 | 说明 |
|------|------|------|
| pid | 是 | 商户号 |
| type | 否 | 支付方式，见下表；不传则收银台内选择 |
| out_trade_no | 是 | 商户订单号（商户侧唯一，`A-Za-z0-9._-\|`） |
| notify_url | 建议 | 异步通知地址（公网可访问） |
| return_url | 建议 | 支付完成跳转 |
| name | 是 | 商品名 |
| money | 是 | 金额，单位元，如 `10.00` |
| currency | 否 | 默认 `CNY` |
| param | 否 | 透传参数，回调原样带回 |
| sign | 是 | 签名 |
| sign_type | 否 | 固定 `MD5` |

成功后 **302** 跳转到 `/cashier/{平台订单号}`。

#### 4.2 API 下单（推荐服务端）

`POST/GET https://pay.example.com/api/pay/create`  
参数同上；可选 `clientip`（用户 IP）。

成功响应示例：

```json
{
  "code": 1,
  "msg": "success",
  "trade_no": "20260713120000abcdef",
  "out_trade_no": "A001",
  "type": "alipay",
  "money": "10.00",
  "payurl": "https://pay.example.com/cashier/20260713120000abcdef",
  "pay_url": "https://pay.example.com/cashier/20260713120000abcdef",
  "qrcode": "",
  "expired_at": "2026-07-13 12:30:00"
}
```

将用户重定向到 `payurl` 即可支付。

#### 4.3 支付方式 type

| type | 说明 |
|------|------|
| `alipay` | 支付宝（个人码或官方扫码，由平台配置） |
| `wxpay` | 微信（同上） |
| `usdt_trc20` / `trc20` | USDT-TRC20 |
| `usdt_erc20` / `erc20` | USDT-ERC20 |
| `usdt_bep20` / `bep20` | USDT-BEP20 |
| `trx` | TRX |
| 其他多链 | `usdt_polygon`、`usdt_arbitrum`、`usdt_base` 等 |

### 5. 异步通知（notify_url）

支付成功后，K2Pay 以 **GET** 请求商户 `notify_url`，并附带 query 参数：

| 参数 | 说明 |
|------|------|
| pid | 商户号 |
| trade_no | 平台订单号 |
| out_trade_no | 商户订单号 |
| type | 支付方式（如 `alipay` / `wxpay`） |
| name | 商品名 |
| money | 下单金额 |
| trade_status | 成功时为 `TRADE_SUCCESS` |
| param | 下单透传（若有） |
| sign_type | `MD5` |
| sign | 签名（算法与下单相同，用同样 key 验签） |

**商户必须：**

1. 验签通过  
2. 校验 `out_trade_no` / `money` / `trade_status`  
3. 业务侧幂等发货（同一订单可能重试通知）  
4. 响应 body 为纯文本 **`success`**（大小写不敏感），否则平台会重试  

PHP 验签与响应示例：

```php
// 收到通知
$params = $_GET;
$sign = $params['sign'] ?? '';
unset($params['sign'], $params['sign_type']);
if (k2pay_sign($params, $merchantKey) !== strtolower($sign)) {
    exit('fail');
}
if (($params['trade_status'] ?? '') !== 'TRADE_SUCCESS') {
    exit('fail');
}
// TODO: 根据 out_trade_no 发货（注意幂等）
echo 'success';
```

### 6. 同步跳转（return_url）

用户支付成功后，收银台会跳转 `return_url`，并带上与异步通知**相同结构**的签名参数。  
**发货请以异步 notify 为准**，return 仅作页面展示。

### 7. 查单

```http
GET /api/pay/order?pid=1001&key=商户密钥&out_trade_no=A001
# 或 trade_no=平台订单号
```

成功时 `code=1`，`status`：`0` 未支付，`1` 已支付。

### 8. 商户信息

```http
GET /api/pay/merchant?pid=1001&key=商户密钥
```

返回余额 `money`、今日/昨日订单数等。

### 9. 退款

```http
POST /api/pay/refund
pid=...&key=...&money=1.00&out_trade_no=A001
# 或 trade_no=平台订单号
```

说明：当前为**商户余额冲正**，非微信/支付宝原路退回。

### 10. 对接检查清单

- [ ] 使用 HTTPS 网关域名  
- [ ] `pid` / `key` 正确，签名算法与文档一致  
- [ ] `notify_url` 为公网地址且能返回 `success`  
- [ ] 业务侧按 `out_trade_no` 幂等处理  
- [ ] 用查单接口做对账兜底  
- [ ] 测试环境先用小额/测试单走通全流程  
- [ ] 若网关启用了 **商户 IP 白名单**：已填 **调 API 的服务器出口 IP**（上 Cloudflare 时见上文，勿填 CF 节点 IP）  
- [ ] 若支付域名走 **Cloudflare**：API 日志中 client IP 已为真实出口 IP 后再开白名单  

### 11. 从旧易支付迁移

| 旧路径 | 新路径 |
|--------|--------|
| `/submit.php` | `/api/pay/submit` |
| `/mapi.php` | `/api/pay/create` |
| `/api.php?act=query` | `/api/pay/merchant` |
| `/api.php?act=order` | `/api/pay/order` |
| `/api.php?act=orders` | `/api/pay/orders` |
| `/api.php?act=settle` | `/api/pay/settle` |
| `/api.php?act=refund` | `/api/pay/refund` |

签名与回调字段保持易支付习惯，通常只需改 **API 根路径**。

---

## 支付 API 速查

| 说明 | 方法 | 路径 |
|------|------|------|
| 表单跳转下单 | GET/POST | `/api/pay/submit` |
| JSON 下单 | GET/POST | `/api/pay/create` |
| 商户信息 | GET | `/api/pay/merchant` |
| 查单 | GET | `/api/pay/order` |
| 订单列表 | GET | `/api/pay/orders` |
| 结算记录 | GET | `/api/pay/settle` |
| 退款 | POST | `/api/pay/refund` |
| 支付方式 | GET | `/api/pay/types?pid=` |
| 收银台 | GET | `/cashier/{trade_no}` |

官方通道回调（平台配置到支付宝/微信开放平台）：

- `POST /api/channel/notify/alipay`
- `POST /api/channel/notify/wechat`

## 本地开发

```bash
# 要求: Go 1.21+、PostgreSQL 14+
# 准备数据库后修改 config.yaml 中 database 段
go run .
# 管理后台 http://127.0.0.1:6088/admin
# 商户后台 http://127.0.0.1:6088/merchant
```

```bash
# 编译当前平台
CGO_ENABLED=0 go build -o k2pay .

# 跨平台 Release 二进制（Linux）
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o k2pay-linux-amd64 .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o k2pay-linux-arm64 .
```

模块路径：`github.com/HenZenKuriRIP/k2pay`

## 目录结构

```
k2pay/
├── main.go                 # 入口与路由
├── config/                 # 配置加载（含 PostgreSQL DSN）
├── config.yaml             # 配置示例
├── internal/
│   ├── handler/            # HTTP 处理器
│   ├── service/            # 业务（订单、链监控、指标等）
│   ├── model/              # 数据模型与 AutoMigrate
│   ├── payment/            # 官方支付宝/微信驱动
│   └── middleware/         # 鉴权与限流
├── web/                    # 管理端 / 商户端 / 收银台模板与静态资源
├── scripts/
│   ├── install.sh          # 一键安装（PostgreSQL + Nginx）
│   └── uninstall.sh
├── db/                     # 参考 SQL / 迁移说明
└── release/                # 构建产物（本地）
```

## Release

最新版本见 [Releases](https://github.com/HenZenKuriRIP/k2pay/releases)。

| 资产 | 说明 |
|------|------|
| `k2pay-linux-amd64.tar.gz` | Linux x86_64 |
| `k2pay-linux-arm64.tar.gz` | Linux ARM64 |
| `CHECKSUMS.txt` | SHA-256 |

## License

MIT
