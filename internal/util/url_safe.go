package util

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ValidateCallbackURL 校验商户回调/跳转 URL 的协议与主机名，防止明显 SSRF 目标
// 对域名不强制 DNS 解析（避免创建订单时因 DNS 抖动失败）；发送时再用 SafeHTTPClient 校验解析 IP
func ValidateCallbackURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil // 空地址由调用方决定是否允许
	}

	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("URL 格式无效")
	}

	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("回调 URL 仅支持 http/https 协议")
	}

	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("回调 URL 缺少主机名")
	}

	// 明确禁止的主机名
	lowerHost := strings.ToLower(host)
	if lowerHost == "localhost" || lowerHost == "metadata.google.internal" ||
		strings.HasSuffix(lowerHost, ".localhost") ||
		strings.HasSuffix(lowerHost, ".local") ||
		strings.HasSuffix(lowerHost, ".internal") ||
		strings.HasSuffix(lowerHost, ".intranet") {
		return fmt.Errorf("回调 URL 不允许指向内网主机")
	}

	// IP 字面量直接检查
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateOrReservedIP(ip) {
			return fmt.Errorf("回调 URL 不允许指向内网或保留地址")
		}
	}

	return nil
}

// isPrivateOrReservedIP 判断是否为私有、回环、链路本地、未指定或特殊用途地址
func isPrivateOrReservedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	// 云元数据与 CGNAT 等
	if ip4 := ip.To4(); ip4 != nil {
		// 100.64.0.0/10 (CGNAT)
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return true
		}
		// 0.0.0.0/8
		if ip4[0] == 0 {
			return true
		}
	}
	return false
}

// SafeHTTPClient 返回禁止访问内网 IP 的 HTTP 客户端（用于异步回调）
func SafeHTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			// 解析并校验每一个 IP
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("dns lookup failed: %w", err)
			}
			var lastErr error
			for _, ipAddr := range ips {
				if isPrivateOrReservedIP(ipAddr.IP) {
					lastErr = fmt.Errorf("blocked private/reserved address: %s", ipAddr.IP.String())
					continue
				}
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ipAddr.IP.String(), port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			if lastErr == nil {
				lastErr = fmt.Errorf("no safe address for host %s", host)
			}
			return nil, lastErr
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if err := ValidateCallbackURL(req.URL.String()); err != nil {
				return fmt.Errorf("redirect blocked: %w", err)
			}
			// 重定向目标若为 IP 字面量已在 Validate 中检查；域名在下次 Dial 时再拦
			if len(via) >= 3 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}
}
