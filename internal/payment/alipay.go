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
