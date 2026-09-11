package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"

	"github.com/codex2api/auth"
)

// sendCodexTelemetryJob 使用账号网络配置发送一批遥测数据。
//
// 遥测与 /responses 一样携带账号身份打 chatgpt.com。Resin 启用时必须同样经反代
// 发出，否则所有账号会共享本机出口 IP 直连，与该账号 /responses 流量的出口不一致
// （issue #372 的不变量）。metrics 端点虽不带 Bearer，但真实客户端从同一出口发出，
// 这里保持一致。
func sendCodexTelemetryJob(job codexTelemetryJob) error {
	ctx, cancel := context.WithTimeout(context.Background(), codexTelemetryTimeout)
	defer cancel()
	finalURL, resinClient, viaResin := resinMaintenanceTarget(job.client.account, job.url)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, finalURL, bytes.NewReader(job.body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	if job.metrics {
		req.Header.Set("User-Agent", "OTel-OTLP-Exporter-Rust/0.31.0")
		req.Header.Set("statsig-api-key", codexStatsigAPIKey())
	} else {
		applyCodexAnalyticsHeaders(req.Header, job.client)
	}
	client := codexTelemetryHTTPClient(getPooledClient(job.client.account, job.client.proxyURL))
	if viaResin {
		req.Header.Set("X-Resin-Account", ResinAccountID(job.client.account))
		client = codexTelemetryHTTPClient(resinClient)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &codexTelemetryHTTPError{status: resp.StatusCode}
	}
	return nil
}

// codexTelemetryHTTPClient 克隆共享池化客户端并禁止跟随重定向。
//
// 分析请求携带账号的 Bearer access token；Go 默认策略只在跨 host 重定向时剥离
// Authorization，同 host 的 https→http 降级不会剥离。池化客户端按账号复用、
// 不可就地改写，因此这里浅拷贝一份并覆盖 CheckRedirect，避免凭证被转发到
// 重定向目标。与 grok_media.go / claude_api_key.go 的既有做法一致。
func codexTelemetryHTTPClient(base *http.Client) *http.Client {
	if base == nil {
		base = &http.Client{}
	}
	clone := *base
	clone.CheckRedirect = codexTelemetryRejectRedirect
	transport := clone.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	// 池化 client 已带通用 codex 日志层。先剥掉再加 telemetry 专用 tag，
	// 避免 OUTBOUND_HEADER_LOG 开启时同一请求重复记录。
	clone.Transport = auth.WrapOutboundHeaderLog("codex-telemetry", auth.UnwrapOutboundHeaderLog(transport))
	return &clone
}

// codexTelemetryRejectRedirect 拒绝跟随一切重定向。
func codexTelemetryRejectRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

type codexTelemetryHTTPError struct{ status int }

// Error 返回遥测上游的 HTTP 状态描述。
func (e *codexTelemetryHTTPError) Error() string { return http.StatusText(e.status) }

// applyCodexAnalyticsHeaders 添加 Codex 分析接口要求的客户端身份头。
//
// 真实客户端 send_track_events_request 只显式加 auth 头与 Content-Type，但其
// create_client() 经 default_headers() 给所有请求注入 originator 与 User-Agent
// （codex-rs/login/src/auth/default_client.rs），因此这里同样带上这两个头。
func applyCodexAnalyticsHeaders(headers http.Header, client codexTelemetryClient) {
	headers.Set("Authorization", "Bearer "+client.accessToken)
	headers.Set("Chatgpt-Account-Id", client.accountID)
	headers.Set("User-Agent", client.userAgent)
	headers.Set("Originator", client.originator)
}
