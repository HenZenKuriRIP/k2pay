package util

import (
	"crypto/md5"
	"encoding/hex"
	"log"
	"net/url"
	"sort"
	"strings"
)

// RFC3986Encode 按 RFC 3986 规范进行 URL 编码
func RFC3986Encode(s string) string {
	encoded := url.QueryEscape(s)
	encoded = strings.ReplaceAll(encoded, "+", "%20")
	encoded = strings.ReplaceAll(encoded, "%7E", "~")
	return encoded
}

// GenerateSign 生成签名（彩虹易支付经典算法，默认出站/验签主路径）
// 算法: 参数 ASCII 排序 → k=v&k2=v2（值不编码）→ 末尾直接拼 key → MD5 小写
func GenerateSign(params map[string]string, key string) string {
	return generateSignWithEncoder(params, key, func(s string) string { return s })
}

// GenerateSignEncoded 生成签名（参数值 URL 编码，兼容部分商户）
func GenerateSignEncoded(params map[string]string, key string) string {
	return generateSignWithEncoder(params, key, RFC3986Encode)
}

// generateSignWithEncoder 使用指定编码函数生成签名
func generateSignWithEncoder(params map[string]string, key string, encoder func(string) string) string {
	filtered := make(map[string]string)
	for k, v := range params {
		if k == "sign" || k == "sign_type" || v == "" {
			continue
		}
		filtered[k] = v
	}

	keys := make([]string, 0, len(filtered))
	for k := range filtered {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var builder strings.Builder
	for i, k := range keys {
		if i > 0 {
			builder.WriteString("&")
		}
		builder.WriteString(k)
		builder.WriteString("=")
		builder.WriteString(encoder(filtered[k]))
	}
	// 经典易支付：直接拼接 key，中间无 &
	builder.WriteString(key)

	return MD5(builder.String())
}

// urlDecodeParams 尝试对参数值进行 URL 解码
func urlDecodeParams(params map[string]string) (map[string]string, bool) {
	decoded := make(map[string]string, len(params))
	changed := false
	for k, v := range params {
		d, err := url.QueryUnescape(v)
		if err == nil && d != v {
			decoded[k] = d
			changed = true
		} else {
			decoded[k] = v
		}
	}
	return decoded, changed
}

// VerifySign 验证签名（兼容经典无编码 + URL 编码 + 双重编码）
func VerifySign(params map[string]string, key string, sign string) bool {
	// 1. 经典彩虹易支付：值不编码
	if strings.EqualFold(GenerateSign(params, key), sign) {
		return true
	}

	// 2. RFC3986 编码
	if strings.EqualFold(GenerateSignEncoded(params, key), sign) {
		log.Printf("[VerifySign] 使用 RFC3986 编码验签成功")
		return true
	}

	// 3. PHP QueryEscape（空格为 +）
	if strings.EqualFold(generateSignWithEncoder(params, key, url.QueryEscape), sign) {
		log.Printf("[VerifySign] 使用 QueryEscape 验签成功")
		return true
	}

	// 4. 双重编码场景
	decodedParams, changed := urlDecodeParams(params)
	if changed {
		if strings.EqualFold(GenerateSign(decodedParams, key), sign) {
			return true
		}
		if strings.EqualFold(GenerateSignEncoded(decodedParams, key), sign) {
			return true
		}
	}

	return false
}

// MD5 计算MD5哈希值
func MD5(s string) string {
	hash := md5.Sum([]byte(s))
	return hex.EncodeToString(hash[:])
}

// BuildQueryString 构建查询字符串
func BuildQueryString(params map[string]string) string {
	values := url.Values{}
	for k, v := range params {
		values.Set(k, v)
	}
	return values.Encode()
}

// ParseQueryString 解析查询字符串
func ParseQueryString(query string) map[string]string {
	result := make(map[string]string)
	values, err := url.ParseQuery(query)
	if err != nil {
		return result
	}
	for k, v := range values {
		if len(v) > 0 {
			result[k] = v[0]
		}
	}
	return result
}

// BuildNotifyParams 构建异步/同步通知参数（经典易支付字段 + 经典 MD5）
func BuildNotifyParams(pid, tradeNo, outTradeNo, payType, name, money, tradeStatus, key string) map[string]string {
	// type 转为易支付对外名
	epayType := ToEpayType(payType)
	params := map[string]string{
		"pid":          pid,
		"trade_no":     tradeNo,
		"out_trade_no": outTradeNo,
		"type":         epayType,
		"name":         name,
		"money":        money,
		"trade_status": tradeStatus,
		"sign_type":    "MD5",
	}
	params["sign"] = GenerateSign(params, key)
	return params
}
