package internal

import (
	"fmt"
	"os"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// osGetenv = os.Getenv（包一层方便测试 mock）
var osGetenv = os.Getenv

// DefaultBrowserProfile 与 tls-client 内置 Chrome_133 一致（ClientHello、HTTP/2 SETTINGS/优先级/伪头序等），便于 JA3/JA4 与 HTTP/2 指纹对齐。
var DefaultBrowserProfile = profiles.Chrome_133

// BrowserUserAgent 必须与 DefaultBrowserProfile 同源，勿随机替换，否则与 TLS 指纹不一致。
const BrowserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"

var (
	tlsClientMu sync.Mutex
	tlsBySec    = map[int]tls_client.HttpClient{}
)

// TLSHTTPClient 返回按超时（秒）复用的 tls-client 实例；所有出站 HTTPS 应通过此处以统一指纹。
func TLSHTTPClient(timeout time.Duration) (tls_client.HttpClient, error) {
	sec := int(timeout.Round(time.Second) / time.Second)
	if sec < 1 {
		sec = 1
	}
	tlsClientMu.Lock()
	defer tlsClientMu.Unlock()
	if c, ok := tlsBySec[sec]; ok {
		return c, nil
	}
	c, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(),
		tls_client.WithTimeoutSeconds(sec),
		tls_client.WithClientProfile(DefaultBrowserProfile),
		tls_client.WithRandomTLSExtensionOrder(),
	)
	if err != nil {
		return nil, fmt.Errorf("tls-client: %w", err)
	}
	// 代理支持：设了 TLS_PROXY / HTTPS_PROXY 时，Go 的 tls-client 请求也走代理。
	// 和 captcha-provider 的 NodeXHR 代理统一，解决 HF 数据中心 IP 被风控的问题。
	if proxyURL := getTLSProxyURL(); proxyURL != "" {
		if perr := c.SetProxy(proxyURL); perr == nil {
			LogInfo("tls-client 使用代理: %s", maskProxyURL(proxyURL))
		} else {
			LogWarn("tls-client 设置代理失败: %v", perr)
		}
	}
	tlsBySec[sec] = c
	return c, nil
}

// getTLSProxyURL 返回 Go 侧 tls-client 要用的代理 URL（env TLS_PROXY 优先，回退 HTTPS_PROXY）。
func getTLSProxyURL() string {
	for _, k := range []string{"TLS_PROXY", "HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY"} {
		if v := osGetenv(k); v != "" {
			return v
		}
	}
	return ""
}

// maskProxyURL 脱敏代理 URL 里的密码（日志用）。
func maskProxyURL(u string) string {
	if i := indexOf(u, "://"); i >= 0 {
		rest := u[i+3:]
		if at := indexOf(rest, "@"); at >= 0 {
			return u[:i+3] + "***" + rest[at:]
		}
	}
	return u
}

// indexOf 简单字符串查找。
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// ApplyBrowserFingerprintHeaders 设置与 Chrome 133 一致的 User-Agent 与 Sec-CH-UA（配合 TLSHTTPClient 的 TLS/H2 指纹）。
func ApplyBrowserFingerprintHeaders(h fhttp.Header) {
	h.Set("User-Agent", BrowserUserAgent)
	h.Set("sec-ch-ua", `"Google Chrome";v="133", "Chromium";v="133", "Not A(Brand";v="24"`)
	h.Set("sec-ch-ua-mobile", "?0")
	h.Set("sec-ch-ua-platform", `"Windows"`)
}
