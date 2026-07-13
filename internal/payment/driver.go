package payment

import (
	"context"
	"fmt"
	"sync"

	"github.com/HenZenKuriRIP/k2pay/internal/model"
)

// Mode 收款模式
const (
	ModePersonal = "personal" // 个人收款码（默认）
	ModeOfficial = "official" // 官方商户接口
)

// CreateRequest 官方下单请求
type CreateRequest struct {
	TradeNo    string // 平台订单号
	OutTradeNo string // 商户订单号（冗余）
	Subject    string // 商品名
	AmountCNY  string // 金额，单位元，两位小数
	ClientIP   string
	NotifyURL  string // 异步通知完整 URL
	ReturnURL  string // 同步跳转（可选）
}

// CreateResult 官方下单结果
type CreateResult struct {
	// Channel 写入订单 channel 字段
	Channel string
	// QRContent 扫码内容（微信 code_url / 支付宝 qr_code）
	QRContent string
	// PayURL 可选跳转链接
	PayURL string
	// UpstreamID 上游预下单号
	UpstreamID string
}

// Driver 官方支付驱动
type Driver interface {
	Name() string
	Enabled() bool
	Create(ctx context.Context, req *CreateRequest) (*CreateResult, error)
}

var (
	mu      sync.RWMutex
	drivers = map[string]Driver{}
)

// Register 注册驱动
func Register(d Driver) {
	mu.Lock()
	defer mu.Unlock()
	drivers[d.Name()] = d
}

// Get 获取驱动
func Get(name string) (Driver, bool) {
	mu.RLock()
	defer mu.RUnlock()
	d, ok := drivers[name]
	return d, ok
}

// GetConfig 读取 system_configs 中的配置值
func GetConfig(key, def string) string {
	var cfg model.SystemConfig
	if err := model.GetDB().Where(`"key" = ?`, key).First(&cfg).Error; err != nil {
		return def
	}
	if cfg.Value == "" {
		return def
	}
	return cfg.Value
}

// SetConfig 写入配置
func SetConfig(key, value, desc string) error {
	var cfg model.SystemConfig
	err := model.GetDB().Where(`"key" = ?`, key).First(&cfg).Error
	if err != nil {
		return model.GetDB().Create(&model.SystemConfig{
			Key: key, Value: value, Description: desc,
		}).Error
	}
	return model.GetDB().Model(&cfg).Update("value", value).Error
}

// FiatMode 返回法币支付模式 personal|official
func FiatMode(chain string) string {
	switch chain {
	case "alipay":
		return GetConfig(CfgAlipayMode, ModePersonal)
	case "wechat":
		return GetConfig(CfgWechatMode, ModePersonal)
	default:
		return ModePersonal
	}
}

// OfficialDriverForChain 按链返回已启用的官方驱动
func OfficialDriverForChain(chain string) (Driver, error) {
	if FiatMode(chain) != ModeOfficial {
		return nil, fmt.Errorf("not official mode")
	}
	var name string
	switch chain {
	case "alipay":
		name = "alipay_official"
	case "wechat":
		name = "wechat_official"
	default:
		return nil, fmt.Errorf("no official driver for %s", chain)
	}
	d, ok := Get(name)
	if !ok || !d.Enabled() {
		return nil, fmt.Errorf("official driver %s not configured", name)
	}
	return d, nil
}

// 配置键
const (
	CfgAlipayMode       = "pay_alipay_mode"        // personal|official
	CfgAlipayAppID      = "pay_alipay_app_id"
	CfgAlipayPrivateKey = "pay_alipay_private_key" // 应用私钥 PKCS1/PKCS8 PEM 或纯 base64
	CfgAlipayPublicKey  = "pay_alipay_public_key"  // 支付宝公钥
	CfgAlipaySandbox    = "pay_alipay_sandbox"     // 1=沙箱

	CfgWechatMode       = "pay_wechat_mode" // personal|official
	CfgWechatAppID      = "pay_wechat_app_id"
	CfgWechatMchID      = "pay_wechat_mch_id"
	CfgWechatAPIv3Key   = "pay_wechat_api_v3_key"
	CfgWechatSerialNo   = "pay_wechat_serial_no"    // 商户证书序列号
	CfgWechatPrivateKey = "pay_wechat_private_key"  // 商户 API 证书私钥
	CfgSiteURL          = "site_url"               // 站点公网 URL，用于拼 notify
)

// EnsureDefaultConfigs 初始化默认支付配置键
func EnsureDefaultConfigs() {
	defaults := []model.SystemConfig{
		{Key: CfgAlipayMode, Value: ModePersonal, Description: "支付宝模式: personal个人码 official官方"},
		{Key: CfgAlipayAppID, Value: "", Description: "支付宝应用AppID"},
		{Key: CfgAlipayPrivateKey, Value: "", Description: "支付宝应用私钥"},
		{Key: CfgAlipayPublicKey, Value: "", Description: "支付宝公钥"},
		{Key: CfgAlipaySandbox, Value: "0", Description: "支付宝沙箱: 1启用"},
		{Key: CfgWechatMode, Value: ModePersonal, Description: "微信模式: personal个人码 official官方"},
		{Key: CfgWechatAppID, Value: "", Description: "微信AppID"},
		{Key: CfgWechatMchID, Value: "", Description: "微信商户号"},
		{Key: CfgWechatAPIv3Key, Value: "", Description: "微信APIv3密钥"},
		{Key: CfgWechatSerialNo, Value: "", Description: "微信商户API证书序列号"},
		{Key: CfgWechatPrivateKey, Value: "", Description: "微信商户API私钥"},
		{Key: CfgSiteURL, Value: "", Description: "站点公网URL(https://domain.com)"},
	}
	for _, d := range defaults {
		var count int64
		model.GetDB().Model(&model.SystemConfig{}).Where(`"key" = ?`, d.Key).Count(&count)
		if count == 0 {
			model.GetDB().Create(&d)
		}
	}
}
