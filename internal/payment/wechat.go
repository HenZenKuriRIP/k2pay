package payment

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// WechatOfficial 微信支付 APIv3 Native（扫码）
type WechatOfficial struct{}

func init() {
	Register(&WechatOfficial{})
}

func (w *WechatOfficial) Name() string { return "wechat_official" }

func (w *WechatOfficial) Enabled() bool {
	return GetConfig(CfgWechatMode, ModePersonal) == ModeOfficial &&
		GetConfig(CfgWechatAppID, "") != "" &&
		GetConfig(CfgWechatMchID, "") != "" &&
		GetConfig(CfgWechatSerialNo, "") != "" &&
		GetConfig(CfgWechatPrivateKey, "") != "" &&
		GetConfig(CfgWechatAPIv3Key, "") != ""
}

func (w *WechatOfficial) Create(ctx context.Context, req *CreateRequest) (*CreateResult, error) {
	appID := GetConfig(CfgWechatAppID, "")
	mchID := GetConfig(CfgWechatMchID, "")
	serial := GetConfig(CfgWechatSerialNo, "")
	priv, err := ParseRSAPrivateKey(GetConfig(CfgWechatPrivateKey, ""))
	if err != nil {
		return nil, fmt.Errorf("wechat private key: %w", err)
	}

	// 元转分
	fen, err := yuanToFen(req.AmountCNY)
	if err != nil {
		return nil, err
	}

	bodyObj := map[string]interface{}{
		"appid":        appID,
		"mchid":        mchID,
		"description":  truncateRunes(req.Subject, 127),
		"out_trade_no": req.TradeNo,
		"notify_url":   req.NotifyURL,
		"amount": map[string]interface{}{
			"total":    fen,
			"currency": "CNY",
		},
	}
	bodyBytes, _ := json.Marshal(bodyObj)

	const path = "/v3/pay/transactions/native"
	urlStr := "https://api.mch.weixin.qq.com" + path
	auth, err := wechatAuthorization("POST", path, string(bodyBytes), mchID, serial, priv)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, urlStr, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", auth)
	httpReq.Header.Set("User-Agent", "K2Pay/1.0")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("wechat request: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("wechat http %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		CodeURL string `json:"code_url"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("wechat parse: %w", err)
	}
	if result.CodeURL == "" {
		return nil, fmt.Errorf("wechat empty code_url: %s", string(respBody))
	}

	return &CreateResult{
		Channel:   "wechat_official",
		QRContent: result.CodeURL,
		UpstreamID: req.TradeNo,
	}, nil
}

func wechatAuthorization(method, canonicalURL, body, mchID, serial string, priv *rsa.PrivateKey) (string, error) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := hex.EncodeToString(randomBytes(16))
	message := method + "\n" + canonicalURL + "\n" + ts + "\n" + nonce + "\n" + body + "\n"
	sig, err := SignSHA256WithRSA(priv, message)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		`WECHATPAY2-SHA256-RSA2048 mchid="%s",nonce_str="%s",signature="%s",timestamp="%s",serial_no="%s"`,
		mchID, nonce, sig, ts, serial,
	), nil
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// 极低概率失败时用时间填充
		for i := range b {
			b[i] = byte(time.Now().UnixNano() >> (i % 8))
		}
	}
	return b
}

func yuanToFen(yuan string) (int, error) {
	yuan = strings.TrimSpace(yuan)
	parts := strings.SplitN(yuan, ".", 2)
	yuanPart, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, fmt.Errorf("invalid amount")
	}
	fen := yuanPart * 100
	if len(parts) == 2 {
		frac := parts[1]
		if len(frac) > 2 {
			frac = frac[:2]
		}
		for len(frac) < 2 {
			frac += "0"
		}
		f, _ := strconv.Atoi(frac)
		if yuanPart < 0 {
			fen -= f
		} else {
			fen += f
		}
	}
	if fen <= 0 {
		return 0, fmt.Errorf("amount must be positive")
	}
	return fen, nil
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// WechatNotifyResource 回调解密后的业务数据
type WechatNotifyResource struct {
	MchID          string `json:"mchid"`
	AppID          string `json:"appid"`
	OutTradeNo     string `json:"out_trade_no"`
	TransactionID  string `json:"transaction_id"`
	TradeState     string `json:"trade_state"`
	SuccessTime    string `json:"success_time"`
	Amount         struct {
		Total    int    `json:"total"`
		Currency string `json:"currency"`
	} `json:"amount"`
}

// DecryptWechatNotify 解密微信支付 V3 回调 resource
func DecryptWechatNotify(associatedData, nonce, ciphertext string) (*WechatNotifyResource, error) {
	key := []byte(GetConfig(CfgWechatAPIv3Key, ""))
	if len(key) != 32 {
		return nil, fmt.Errorf("api v3 key must be 32 bytes")
	}
	data, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, []byte(nonce), data, []byte(associatedData))
	if err != nil {
		return nil, fmt.Errorf("aes-gcm decrypt: %w", err)
	}
	var res WechatNotifyResource
	if err := json.Unmarshal(plain, &res); err != nil {
		return nil, err
	}
	return &res, nil
}
