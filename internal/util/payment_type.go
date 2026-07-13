package util

import "strings"

// 支付类型元信息（易支付 type 名 + 内部 chain）
type PaymentTypeMeta struct {
	EpayType string // 对外 type（易支付兼容名）
	Chain    string // 内部链/通道
	Name     string // 展示名
	IsCrypto bool
	IsFiat   bool
}

// 标准支付类型定义
var paymentTypeRegistry = []PaymentTypeMeta{
	{EpayType: "alipay", Chain: "alipay", Name: "支付宝", IsFiat: true},
	{EpayType: "wxpay", Chain: "wechat", Name: "微信支付", IsFiat: true},
	// 加密货币扩展（非经典易支付，保留为扩展 type）
	{EpayType: "usdt_trc20", Chain: "trc20", Name: "USDT (TRC20)", IsCrypto: true},
	{EpayType: "usdt_erc20", Chain: "erc20", Name: "USDT (ERC20)", IsCrypto: true},
	{EpayType: "usdt_bep20", Chain: "bep20", Name: "USDT (BEP20)", IsCrypto: true},
	{EpayType: "usdt_polygon", Chain: "polygon", Name: "USDT (Polygon)", IsCrypto: true},
	{EpayType: "usdt_arbitrum", Chain: "arbitrum", Name: "USDT (Arbitrum)", IsCrypto: true},
	{EpayType: "usdt_optimism", Chain: "optimism", Name: "USDT (Optimism)", IsCrypto: true},
	{EpayType: "usdt_base", Chain: "base", Name: "USDT (Base)", IsCrypto: true},
	{EpayType: "usdt_avalanche", Chain: "avalanche", Name: "USDT (Avalanche)", IsCrypto: true},
	{EpayType: "trx", Chain: "trx", Name: "TRX", IsCrypto: true},
}

// 别名 → 标准 epay type
var paymentTypeAliases = map[string]string{
	// 支付宝
	"alipay": "alipay", "alipays": "alipay", "alipay2": "alipay", "2": "alipay",
	// 微信
	"wxpay": "wxpay", "wechat": "wxpay", "wx": "wxpay", "wxpayn": "wxpay",
	"wxpayng": "wxpay", "wxpaysl": "wxpay", "1": "wxpay",
	// 加密货币
	"usdt": "usdt_trc20", "trc20": "usdt_trc20", "usdt_trc20": "usdt_trc20", "usdt_tron": "usdt_trc20",
	"erc20": "usdt_erc20", "usdt_erc20": "usdt_erc20", "usdt_eth": "usdt_erc20",
	"bep20": "usdt_bep20", "usdt_bep20": "usdt_bep20", "usdt_bsc": "usdt_bep20",
	"polygon": "usdt_polygon", "usdt_polygon": "usdt_polygon",
	"arbitrum": "usdt_arbitrum", "usdt_arbitrum": "usdt_arbitrum", "arb": "usdt_arbitrum",
	"optimism": "usdt_optimism", "usdt_optimism": "usdt_optimism", "op": "usdt_optimism",
	"base": "usdt_base", "usdt_base": "usdt_base",
	"avalanche": "usdt_avalanche", "usdt_avalanche": "usdt_avalanche", "avax": "usdt_avalanche",
	"trx": "trx", "trx_native": "trx",
}

// ResolvePaymentType 将任意 type 别名解析为标准元信息；空 type 返回 nil,nil
func ResolvePaymentType(payType string) (*PaymentTypeMeta, error) {
	payType = strings.TrimSpace(strings.ToLower(payType))
	if payType == "" {
		return nil, nil
	}
	canonical, ok := paymentTypeAliases[payType]
	if !ok {
		// 尝试直接匹配 registry
		for i := range paymentTypeRegistry {
			if paymentTypeRegistry[i].EpayType == payType || paymentTypeRegistry[i].Chain == payType {
				m := paymentTypeRegistry[i]
				return &m, nil
			}
		}
		return nil, errUnsupportedPayType
	}
	for i := range paymentTypeRegistry {
		if paymentTypeRegistry[i].EpayType == canonical {
			m := paymentTypeRegistry[i]
			return &m, nil
		}
	}
	return nil, errUnsupportedPayType
}

var errUnsupportedPayType = &payTypeError{"不支持的支付类型"}

type payTypeError struct{ msg string }

func (e *payTypeError) Error() string { return e.msg }

// GetPaymentTypeChain 根据支付类型获取链名
func GetPaymentTypeChain(payType string) string {
	meta, err := ResolvePaymentType(payType)
	if err != nil || meta == nil {
		return strings.ToLower(strings.TrimSpace(payType))
	}
	return meta.Chain
}

// NormalizePaymentType 标准化为内部 type 字段（链上用 usdt_xxx，法币用 chain 名）
func NormalizePaymentType(payType string) string {
	meta, err := ResolvePaymentType(payType)
	if err != nil || meta == nil {
		return strings.ToLower(strings.TrimSpace(payType))
	}
	if meta.IsFiat || meta.Chain == "trx" {
		return meta.Chain
	}
	return "usdt_" + meta.Chain
}

// ToEpayType 将内部 type/chain 转为易支付对外 type 名（用于回调/查单）
func ToEpayType(internalTypeOrChain string) string {
	meta, err := ResolvePaymentType(internalTypeOrChain)
	if err != nil || meta == nil {
		// 已是内部 chain
		switch strings.ToLower(internalTypeOrChain) {
		case "wechat":
			return "wxpay"
		case "alipay":
			return "alipay"
		default:
			return internalTypeOrChain
		}
	}
	return meta.EpayType
}

// IsValidChain 检查链是否有效
func IsValidChain(chain string) bool {
	validChains := map[string]bool{
		"trx": true, "trc20": true, "erc20": true, "bep20": true,
		"polygon": true, "optimism": true, "arbitrum": true,
		"avalanche": true, "base": true, "wechat": true, "alipay": true,
	}
	return validChains[chain]
}

// IsFiatChain 检查是否为法币收款方式
func IsFiatChain(chain string) bool {
	return chain == "wechat" || chain == "alipay"
}

// AllPaymentTypes 返回全部标准支付类型
func AllPaymentTypes() []PaymentTypeMeta {
	out := make([]PaymentTypeMeta, len(paymentTypeRegistry))
	copy(out, paymentTypeRegistry)
	return out
}
