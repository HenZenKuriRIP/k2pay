# K2Pay

多商户聚合收款网关。兼容**易支付协议语义**（签名、回调、查单），使用 **REST 风格 API**；支持加密货币（USDT/TRX 多链）、微信/支付宝个人码与官方扫码支付。

仓库: https://github.com/HenZenKuriRIP/k2pay

## 功能概览

- 商户体系：管理员开户、`pid` / `key`、商户后台、余额与提现
- 支付：`/api/pay/*` 下单 / 查单 / 退款（余额冲正）
- 通道：链上 USDT/TRX、个人收款码、支付宝/微信官方 Native 扫码
- 回调：异步 `notify_url`（MD5 签名 + 重试）、同步 `return_url`
- 运维：Docker、systemd、一键安装（Nginx + 证书 + MySQL）

## 快速安装（Linux 服务器）

```bash
# 一键安装（自动依赖、数据库、二进制、systemd；可选 Nginx + Let's Encrypt）
curl -fsSL https://raw.githubusercontent.com/HenZenKuriRIP/k2pay/main/scripts/install.sh | \
  sudo bash -s -- --domain pay.example.com --email admin@example.com
```

| 参数 | 说明 |
|------|------|
| `--domain` | 域名（配置 Nginx） |
| `--email` | 申请证书邮箱 |
| `--skip-cert` | 不申请 HTTPS |
| `--skip-nginx` | 仅装服务，不装 Nginx |
| `--version v1.0.0` | 指定 Release 版本 |

卸载（**保留数据库与域名证书**）：

```bash
curl -fsSL https://raw.githubusercontent.com/HenZenKuriRIP/k2pay/main/scripts/uninstall.sh | sudo bash
```

| 入口 | 地址 |
|------|------|
| 管理后台 | `https://你的域名/admin` |
| 商户后台 | `https://你的域名/merchant` |
| 默认管理员 | `admin` / `admin123`（请立刻修改） |

数据库密码见 `/etc/k2pay/db.credentials`。

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
# 要求: Go 1.21+、MySQL 8+
go run .
# 管理后台 http://127.0.0.1:6088/admin
```

```bash
CGO_ENABLED=0 go build -o k2pay .
```

模块路径：`github.com/HenZenKuriRIP/k2pay`

## 目录结构

```
k2pay/
├── main.go
├── config/
├── internal/
├── web/
├── scripts/
│   ├── install.sh
│   └── uninstall.sh
├── db/
└── config.yaml
```

## 配置要点

- 配置：`/etc/k2pay/config.yaml` 或工作目录 `config.yaml`
- 数据：`/var/lib/k2pay`
- 官方支付：管理后台填写 `site_url` 与支付宝/微信密钥
- 法币默认个人码；改为「官方」后走开放平台扫码

## Docker

```bash
docker compose up -d
```

## License

MIT
