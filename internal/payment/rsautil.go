package payment

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"strings"
)

// ParseRSAPrivateKey 解析 PKCS1/PKCS8 PEM 或裸 base64 私钥
func ParseRSAPrivateKey(raw string) (*rsa.PrivateKey, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty private key")
	}
	// 已是 PEM
	if strings.Contains(raw, "BEGIN") {
		block, _ := pem.Decode([]byte(raw))
		if block == nil {
			return nil, fmt.Errorf("invalid PEM private key")
		}
		if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
			return key, nil
		}
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse private key: %w", err)
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("not RSA private key")
		}
		return rk, nil
	}
	// 裸 base64 → 包成 PKCS8 PEM 再解析
	der, err := base64.StdEncoding.DecodeString(stripWhitespace(raw))
	if err != nil {
		return nil, fmt.Errorf("decode private key base64: %w", err)
	}
	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("parse private key der: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not RSA private key")
	}
	return rk, nil
}

// ParseRSAPublicKey 解析公钥 PEM 或裸 base64
func ParseRSAPublicKey(raw string) (*rsa.PublicKey, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty public key")
	}
	var der []byte
	if strings.Contains(raw, "BEGIN") {
		block, _ := pem.Decode([]byte(raw))
		if block == nil {
			return nil, fmt.Errorf("invalid PEM public key")
		}
		der = block.Bytes
	} else {
		var err error
		der, err = base64.StdEncoding.DecodeString(stripWhitespace(raw))
		if err != nil {
			return nil, err
		}
	}
	if pub, err := x509.ParsePKIXPublicKey(der); err == nil {
		if rk, ok := pub.(*rsa.PublicKey); ok {
			return rk, nil
		}
		return nil, fmt.Errorf("not RSA public key")
	}
	// 支付宝有时给 PKCS1 公钥
	return x509.ParsePKCS1PublicKey(der)
}

// SignSHA256WithRSA 签名并 base64
func SignSHA256WithRSA(priv *rsa.PrivateKey, content string) (string, error) {
	h := sha256.Sum256([]byte(content))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, h[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// VerifySHA256WithRSA 验签
func VerifySHA256WithRSA(pub *rsa.PublicKey, content, signB64 string) error {
	sig, err := base64.StdEncoding.DecodeString(signB64)
	if err != nil {
		return err
	}
	h := sha256.Sum256([]byte(content))
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, h[:], sig)
}

func stripWhitespace(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r != ' ' && r != '\n' && r != '\r' && r != '\t' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// FormatPrivateKeyPEM 若无头则包装为 PKCS8 PEM（仅展示用）
func FormatPrivateKeyPEM(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.Contains(raw, "BEGIN") {
		return raw
	}
	return "-----BEGIN PRIVATE KEY-----\n" + chunkBase64(stripWhitespace(raw)) + "\n-----END PRIVATE KEY-----"
}

func chunkBase64(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i += 64 {
		end := i + 64
		if end > len(s) {
			end = len(s)
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(s[i:end])
	}
	return b.String()
}
