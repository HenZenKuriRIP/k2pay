package payment

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// AlipayOfficial 支付宝开放平台当面付扫码（alipay.trade.precreate）
type AlipayOfficial struct{}

func init() {
	Register(&AlipayOfficial{})
}

func (a *AlipayOfficial) Name() string { return "alipay_official" }

func (a *AlipayOfficial) Enabled() bool {
	return GetConfig(CfgAlipayMode, ModePersonal) == ModeOfficial &&
		GetConfig(CfgAlipayAppID, "") != "" &&
		GetConfig(CfgAlipayPrivateKey, "") != ""
}

func (a *AlipayOfficial) gateway() string {
	if GetConfig(CfgAlipaySandbox, "0") == "1" {
		return "https://openapi-sandbox.dl.alipaydev.com/gateway.do"
	}
	return "https://openapi.alipay.com/gateway.do"
}

func (a *AlipayOfficial) Create(ctx context.Context, req *CreateRequest) (*CreateResult, error) {
	appID := GetConfig(CfgAlipayAppID, "")
	privRaw := GetConfig(CfgAlipayPrivateKey, "")
	priv, err := ParseRSAPrivateKey(privRaw)
	if err != nil {
		return nil, fmt.Errorf("alipay private key: %w", err)
	}

	biz := map[string]string{
		"out_trade_no": req.TradeNo,
		"total_amount": req.AmountCNY,
		"subject":      req.Subject,
	}
	if len(biz["subject"]) > 256 {
		biz["subject"] = biz["subject"][:256]
	}
	bizJSON, _ := json.Marshal(biz)

	params := map[string]string{
		"app_id":      appID,
		"method":      "alipay.trade.precreate",
		"format":      "JSON",
		"charset":     "utf-8",
		"sign_type":   "RSA2",
		"timestamp":   time.Now().Format("2006-01-02 15:04:05"),
		"version":     "1.0",
		"notify_url":  req.NotifyURL,
		"biz_content": string(bizJSON),
	}

	signContent := alipaySignContent(params)
	sign, err := SignSHA256WithRSA(priv, signContent)
	if err != nil {
		return nil, fmt.Errorf("alipay sign: %w", err)
	}
	params["sign"] = sign

	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.gateway(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("alipay request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("alipay parse: %w body=%s", err, string(body))
	}
	// 响应键: alipay_trade_precreate_response
	raw, ok := envelope["alipay_trade_precreate_response"]
	if !ok {
		return nil, fmt.Errorf("alipay unexpected response: %s", string(body))
	}
	var result struct {
		Code    string `json:"code"`
		Msg     string `json:"msg"`
		SubCode string `json:"sub_code"`
		SubMsg  string `json:"sub_msg"`
		OutTradeNo string `json:"out_trade_no"`
		QRCode  string `json:"qr_code"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	if result.Code != "10000" {
		msg := result.Msg
		if result.SubMsg != "" {
			msg = result.SubMsg
		}
		return nil, fmt.Errorf("alipay error: %s (%s)", msg, result.Code)
	}
	if result.QRCode == "" {
		return nil, fmt.Errorf("alipay empty qr_code")
	}

	return &CreateResult{
		Channel:   "alipay_official",
		QRContent: result.QRCode,
		UpstreamID: result.OutTradeNo,
	}, nil
}

// alipaySignContent 待签名字符串（key 排序，无空值，k=v&）
func alipaySignContent(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k, v := range params {
		if k == "sign" || v == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(params[k])
	}
	return b.String()
}

// AlipayTestResult 配置探测结果
type AlipayTestResult struct {
	AuthOK  bool
	Code    string
	Message string
	Gateway string
}

// TestAlipayCredentials 通过 alipay.trade.query 探测 AppID/私钥是否有效
// 查询一笔不存在的订单：鉴权成功通常返回业务错误（交易不存在），鉴权失败返回签名/应用错误
func TestAlipayCredentials() (*AlipayTestResult, error) {
	a := &AlipayOfficial{}
	appID := GetConfig(CfgAlipayAppID, "")
	privRaw := GetConfig(CfgAlipayPrivateKey, "")
	if appID == "" || privRaw == "" {
		return nil, fmt.Errorf("AppID 或私钥未配置")
	}
	priv, err := ParseRSAPrivateKey(privRaw)
	if err != nil {
		return nil, fmt.Errorf("私钥解析失败: %w", err)
	}

	bizJSON, _ := json.Marshal(map[string]string{
		"out_trade_no": "k2pay_probe_" + time.Now().Format("20060102150405"),
	})
	params := map[string]string{
		"app_id":      appID,
		"method":      "alipay.trade.query",
		"format":      "JSON",
		"charset":     "utf-8",
		"sign_type":   "RSA2",
		"timestamp":   time.Now().Format("2006-01-02 15:04:05"),
		"version":     "1.0",
		"biz_content": string(bizJSON),
	}
	signContent := alipaySignContent(params)
	sign, err := SignSHA256WithRSA(priv, signContent)
	if err != nil {
		return nil, fmt.Errorf("签名失败: %w", err)
	}
	params["sign"] = sign

	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}

	gateway := a.gateway()
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Post(gateway, "application/x-www-form-urlencoded;charset=utf-8", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("请求网关失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("响应解析失败: %s", string(body))
	}
	raw, ok := envelope["alipay_trade_query_response"]
	if !ok {
		return nil, fmt.Errorf("非预期响应: %s", string(body))
	}
	var result struct {
		Code    string `json:"code"`
		Msg     string `json:"msg"`
		SubCode string `json:"sub_code"`
		SubMsg  string `json:"sub_msg"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}

	out := &AlipayTestResult{
		Code:    result.Code,
		Gateway: gateway,
	}
	// 10000=成功；40004/ACQ.TRADE_NOT_EXIST 等业务错误也说明鉴权通过
	// 40001 缺少参数、40002 非法参数、40003 权限、20000 服务不可用 等
	switch result.Code {
	case "10000":
		out.AuthOK = true
		out.Message = "接口调用成功，配置有效"
	case "40004":
		// 业务失败（交易不存在）= 鉴权成功
		out.AuthOK = true
		msg := result.SubMsg
		if msg == "" {
			msg = result.Msg
		}
		out.Message = "鉴权通过（探测单不存在，属预期）: " + msg
	case "20000":
		out.AuthOK = false
		out.Message = "支付宝服务暂不可用: " + result.Msg
	case "40001", "40002", "40003", "40006":
		out.AuthOK = false
		msg := result.SubMsg
		if msg == "" {
			msg = result.Msg
		}
		out.Message = fmt.Sprintf("鉴权/权限失败 [%s]: %s", result.Code, msg)
	default:
		// 其他：若有 sub_code 含 invalid-signature 等则失败
		sub := strings.ToLower(result.SubCode + " " + result.SubMsg + " " + result.Msg)
		if strings.Contains(sub, "sign") || strings.Contains(sub, "签名") || strings.Contains(sub, "app_id") {
			out.AuthOK = false
			out.Message = result.SubMsg
			if out.Message == "" {
				out.Message = result.Msg
			}
		} else {
			// 默认当作可达且鉴权可能通过
			out.AuthOK = true
			out.Message = fmt.Sprintf("已收到业务响应 code=%s %s", result.Code, result.SubMsg)
		}
	}
	return out, nil
}

// VerifyAlipayNotify 验证支付宝异步通知签名
func VerifyAlipayNotify(params map[string]string) error {
	sign := params["sign"]
	signType := params["sign_type"]
	if sign == "" {
		return fmt.Errorf("missing sign")
	}
	if signType != "" && signType != "RSA2" {
		return fmt.Errorf("unsupported sign_type %s", signType)
	}
	pubRaw := GetConfig(CfgAlipayPublicKey, "")
	pub, err := ParseRSAPublicKey(pubRaw)
	if err != nil {
		return fmt.Errorf("alipay public key: %w", err)
	}
	content := alipaySignContent(params)
	return VerifySHA256WithRSA(pub, content, sign)
}
