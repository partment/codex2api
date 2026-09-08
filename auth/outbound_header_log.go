package auth

import (
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
)

// outboundHeaderLogEnv 控制出站请求头日志的环境变量。与 CODEX_FINGERPRINT_DEBUG
// 同风格：开启后每一笔出站 HTTP / WSS 请求都会把实际送出的 header 打印到 stdout，
// 便于对照上游行为排查指纹、鉴权与路由问题。敏感值会被遮罩，见 maskOutboundHeaderValue。
const outboundHeaderLogEnv = "OUTBOUND_HEADER_LOG"

// OutboundHeaderLogEnabled 报告出站请求头日志是否开启。
func OutboundHeaderLogEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(outboundHeaderLogEnv))) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

// outboundHeaderSensitive 需要遮罩值的请求头（小写比较）。
// Authorization / Cookie 等直接携带凭据，原样写入 stdout 等同于泄漏 token。
var outboundHeaderSensitive = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"x-api-key":           true,
	"x-goog-api-key":      true,
	"x-session-key":       true,
	"session-key":         true,
	"x-auth-token":        true,
	"x-access-token":      true,
}

// maskOutboundHeaderValue 遮罩敏感 header 的值：保留首尾各 4 字符，短值整体遮罩。
func maskOutboundHeaderValue(value string) string {
	if value == "" {
		return ""
	}
	if len(value) <= 8 {
		return "***"
	}
	return value[:4] + "***" + value[len(value)-4:]
}

// formatOutboundHeaders 把 http.Header 序列化成排序稳定、便于 grep 的单行文本。
func formatOutboundHeaders(headers http.Header) string {
	if len(headers) == 0 {
		return "{}"
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		values := headers.Values(name)
		shown := make([]string, len(values))
		for i, v := range values {
			if outboundHeaderSensitive[strings.ToLower(name)] {
				v = maskOutboundHeaderValue(v)
			}
			shown[i] = v
		}
		parts = append(parts, name+": "+strings.Join(shown, ", "))
	}
	return strings.Join(parts, "; ")
}

// safeOutboundURL 回传去掉 userinfo 密码后的 URL 文本，避免把凭据写进日志。
func safeOutboundURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	if u.User != nil {
		if _, has := u.User.Password(); has {
			copy := *u
			copy.User = url.UserPassword(u.User.Username(), "***")
			u = &copy
		}
	}
	return u.String()
}

// logOutboundHeaders 输出单笔出站请求的请求头日志。
func logOutboundHeaders(kind, tag, method, rawURL, host string, headers http.Header) {
	if !OutboundHeaderLogEnabled() {
		return
	}
	if host == "" {
		host = "-"
	}
	log.Printf("[Outbound-Header] kind=%s tag=%s method=%s url=%s host=%s headers=%q",
		kind, tag, method, rawURL, host, formatOutboundHeaders(headers))
}

// LogOutboundHTTPRequest 打印一笔即将送出的 HTTP 请求头。
// 给无法走 transport wrapper 的路径使用（如 http.DefaultClient.Do 的直接调用点）。
func LogOutboundHTTPRequest(tag string, req *http.Request) {
	if req == nil {
		return
	}
	logOutboundHeaders("http", tag, req.Method, safeOutboundURL(req.URL), req.Host, req.Header)
}

// LogOutboundWSHandshake 打印一笔即将送出的 WSS 握手请求头。
func LogOutboundWSHandshake(tag, wsURL string, headers http.Header) {
	logOutboundHeaders("wss", tag, "GET", wsURL, "", headers)
}

// outboundHeaderLogTransport 包在业务 transport 外层的请求头日志 wrapper。
// RoundTrip 是出站请求的唯一必经点：所有 http.Client 调用（含重定向每一跳）
// 都会经过这里，因此在这里打印的是“实际送出的 header”。
// 注意：http.Transport 在更底层还会自动补 Host / Accept-Encoding / 默认
// User-Agent 等 header，这些不会出现在日志中（仅当调用方已显式设置时可见）。
type outboundHeaderLogTransport struct {
	tag string
	rt  http.RoundTripper
}

func (t *outboundHeaderLogTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req != nil {
		logOutboundHeaders("http", t.tag, req.Method, safeOutboundURL(req.URL), req.Host, req.Header)
	}
	return t.rt.RoundTrip(req)
}

// CloseIdleConnections 转发给内层 transport（若有），保持连接池回收语义不变。
func (t *outboundHeaderLogTransport) CloseIdleConnections() {
	if c, ok := t.rt.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// CloseAllConnections 转发给内层 transport（若有）。uTLS transport 自管连接池，
// 客户端逐出时依赖此方法连带回收在途连接（issue #446），wrapper 必须透传。
func (t *outboundHeaderLogTransport) CloseAllConnections() {
	if c, ok := t.rt.(interface{ CloseAllConnections() }); ok {
		c.CloseAllConnections()
	}
}

// Unwrap 返回内层 transport，供类型断言类代码（连接回收、测试）穿透 wrapper。
func (t *outboundHeaderLogTransport) Unwrap() http.RoundTripper {
	return t.rt
}

// WrapOutboundHeaderLog 给 transport 包上请求头日志层；rt 为 nil 时原样返回 nil。
func WrapOutboundHeaderLog(tag string, rt http.RoundTripper) http.RoundTripper {
	if rt == nil {
		return nil
	}
	return &outboundHeaderLogTransport{tag: tag, rt: rt}
}

// UnwrapOutboundHeaderLog 剥掉请求头日志 wrapper；非 wrapper 时原样返回。
func UnwrapOutboundHeaderLog(rt http.RoundTripper) http.RoundTripper {
	for {
		w, ok := rt.(interface{ Unwrap() http.RoundTripper })
		if !ok {
			return rt
		}
		inner := w.Unwrap()
		if inner == nil {
			return rt
		}
		rt = inner
	}
}
