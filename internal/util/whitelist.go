package util

import (
	"fmt"
	"net"
	"strings"
)

// NormalizeIPWhitelist 规范化 IP 白名单（逗号/换行/空格分隔 → 逗号分隔）
// 支持单 IP 与 CIDR；非法项返回错误
func NormalizeIPWhitelist(raw string) (string, error) {
	parts := splitWhitelistTokens(raw)
	if len(parts) == 0 {
		return "", nil
	}
	seen := make(map[string]struct{}, len(parts))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		item, err := normalizeIPOrCIDR(p)
		if err != nil {
			return "", err
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return strings.Join(out, ","), nil
}

// NormalizeDomainWhitelist 规范化域名/Referer 白名单
// 支持 example.com、*.example.com、完整 URL（自动剥离协议与路径）
func NormalizeDomainWhitelist(raw string) (string, error) {
	parts := splitWhitelistTokens(raw)
	if len(parts) == 0 {
		return "", nil
	}
	seen := make(map[string]struct{}, len(parts))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		domain, err := normalizeDomainEntry(p)
		if err != nil {
			return "", err
		}
		key := strings.ToLower(domain)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, domain)
	}
	return strings.Join(out, ","), nil
}

// ParseHostInput 从用户输入提取主机名（可含协议、路径、端口）
func ParseHostInput(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("主机名为空")
	}
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "https://")
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	// 去掉端口
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	} else {
		// IPv6 无端口时可能带 []
		s = strings.Trim(s, "[]")
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("无法解析主机名")
	}
	// 禁止显然非法字符
	if strings.ContainsAny(s, " \t\n\r") {
		return "", fmt.Errorf("主机名格式无效")
	}
	return s, nil
}

func splitWhitelistTokens(raw string) []string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\n", ",")
	raw = strings.ReplaceAll(raw, ";", ",")
	raw = strings.ReplaceAll(raw, "\t", ",")
	// 空格在 IP/CIDR 中非法，也当分隔符；域名中一般无空格
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' '
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func normalizeIPOrCIDR(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("空 IP")
	}
	if strings.Contains(s, "/") {
		ip, network, err := net.ParseCIDR(s)
		if err != nil {
			return "", fmt.Errorf("无效 CIDR: %s", s)
		}
		// 规范为 network 字符串
		ones, _ := network.Mask.Size()
		return fmt.Sprintf("%s/%d", ip.Mask(network.Mask).String(), ones), nil
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return "", fmt.Errorf("无效 IP: %s", s)
	}
	return ip.String(), nil
}

func normalizeDomainEntry(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("空域名")
	}
	// 通配符前缀保留
	wildcard := false
	if strings.HasPrefix(s, "*.") {
		wildcard = true
		s = s[2:]
	}
	// 若是 URL，剥离
	host, err := ParseHostInput(s)
	if err != nil {
		return "", fmt.Errorf("无效域名: %s", s)
	}
	// 纯 IP 也允许作为 referer host
	host = strings.ToLower(host)
	if host == "" || host == "*" {
		return "", fmt.Errorf("无效域名: %s", s)
	}
	if wildcard {
		return "*." + host, nil
	}
	return host, nil
}
