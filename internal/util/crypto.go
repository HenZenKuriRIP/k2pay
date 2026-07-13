package util

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode"

	"golang.org/x/crypto/bcrypt"
)

// 常见弱口令列表（小写比较）
var weakPasswords = map[string]struct{}{
	"admin":      {},
	"admin123":   {},
	"admin1234":  {},
	"password":   {},
	"password1":  {},
	"123456":     {},
	"12345678":   {},
	"123456789":  {},
	"qwerty":     {},
	"merchant":   {},
	"merchant123": {},
	"test":       {},
	"test123":    {},
	"k2pay":      {},
	"k2pay123":   {},
	"root":       {},
	"root123":    {},
}

// HashPassword 密码加密
func HashPassword(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(bytes), err
}

// CheckPassword 验证密码
func CheckPassword(password, hash string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil
}

// IsWeakPassword 判断是否为弱口令
func IsWeakPassword(password string) bool {
	p := strings.TrimSpace(password)
	if len(p) < 8 {
		return true
	}
	if _, ok := weakPasswords[strings.ToLower(p)]; ok {
		return true
	}
	// 纯数字且过短已覆盖；纯数字 8 位以上仍视为弱
	allDigit := true
	for _, r := range p {
		if !unicode.IsDigit(r) {
			allDigit = false
			break
		}
	}
	if allDigit {
		return true
	}
	return false
}

// ValidateNewPassword 校验新密码强度，返回可读错误
func ValidateNewPassword(password string) error {
	if len(password) < 8 {
		return fmt.Errorf("密码长度至少 8 位")
	}
	if IsWeakPassword(password) {
		return fmt.Errorf("密码过于简单，请使用更复杂的密码")
	}
	return nil
}

// orderPollSecret 订单轮询令牌密钥（由 main 初始化）
var orderPollSecret string

// InitOrderPollSecret 初始化订单轮询令牌密钥
func InitOrderPollSecret(secret string) {
	orderPollSecret = secret
}

// GenerateOrderPollToken 生成收银台轮询令牌（HMAC，不落库）
func GenerateOrderPollToken(tradeNo string) string {
	if orderPollSecret == "" {
		orderPollSecret = "k2pay-default-poll-secret"
	}
	mac := hmac.New(sha256.New, []byte(orderPollSecret))
	mac.Write([]byte("poll:" + tradeNo))
	return hex.EncodeToString(mac.Sum(nil))[:24]
}

// VerifyOrderPollToken 校验订单轮询令牌（常量时间比较）
func VerifyOrderPollToken(tradeNo, token string) bool {
	if tradeNo == "" || token == "" {
		return false
	}
	expected := GenerateOrderPollToken(tradeNo)
	if len(expected) != len(token) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(token)) == 1
}

// GenerateTradeNo 生成交易号
// 格式: 年月日时分秒 + 6位随机数
func GenerateTradeNo() string {
	now := time.Now()
	random := GenerateRandomHex(3) // 6位十六进制
	return fmt.Sprintf("%s%s", now.Format("20060102150405"), random)
}

// GenerateRandomHex 生成随机十六进制字符串
func GenerateRandomHex(length int) string {
	bytes := make([]byte, length)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

// GenerateMerchantKey 生成商户密钥
func GenerateMerchantKey() string {
	return GenerateRandomHex(16) // 32位密钥
}

// GenerateMerchantPID 生成商户PID
func GenerateMerchantPID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano()%1000000000)
}
