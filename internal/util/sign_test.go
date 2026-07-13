package util

import "testing"

// 彩虹易支付经典 MD5：值不编码，k=v&... 后直接拼 key
func TestClassicEpaySign(t *testing.T) {
	params := map[string]string{
		"pid":          "1001",
		"type":         "alipay",
		"out_trade_no": "ORDER001",
		"notify_url":   "https://example.com/notify",
		"return_url":   "https://example.com/return",
		"name":         "test",
		"money":        "10.00",
	}
	key := "test_key"
	// 手动计算: money=10.00&name=test&notify_url=...&out_trade_no=ORDER001&pid=1001&return_url=...&type=alipay + key
	sign := GenerateSign(params, key)
	if sign == "" || len(sign) != 32 {
		t.Fatalf("invalid sign: %s", sign)
	}
	if !VerifySign(params, key, sign) {
		t.Fatal("VerifySign should pass classic sign")
	}
	// 错误密钥
	if VerifySign(params, "wrong", sign) {
		t.Fatal("should fail with wrong key")
	}
}

func TestToEpayType(t *testing.T) {
	if ToEpayType("wechat") != "wxpay" {
		t.Fatal("wechat -> wxpay")
	}
	if ToEpayType("alipay") != "alipay" {
		t.Fatal("alipay")
	}
	if ToEpayType("usdt_trc20") != "usdt_trc20" {
		t.Fatal("usdt_trc20")
	}
}

func TestResolvePaymentType(t *testing.T) {
	m, err := ResolvePaymentType("wxpay")
	if err != nil || m.Chain != "wechat" {
		t.Fatalf("wxpay resolve: %+v %v", m, err)
	}
	m, err = ResolvePaymentType("")
	if err != nil || m != nil {
		t.Fatal("empty type should be nil,nil")
	}
	_, err = ResolvePaymentType("qqpay")
	if err == nil {
		t.Fatal("qqpay unsupported expected error")
	}
}
