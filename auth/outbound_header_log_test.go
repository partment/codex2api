package auth

import (
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestOutboundHeaderLogEnabledParsesEnv(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"no", false},
		{"1", true},
		{"true", true},
		{"yes", true},
		{"on", true},
		{"TRUE", true},
		{" on ", true},
	}
	for _, tc := range cases {
		t.Setenv(outboundHeaderLogEnv, tc.value)
		if got := OutboundHeaderLogEnabled(); got != tc.want {
			t.Errorf("OUTBOUND_HEADER_LOG=%q → enabled=%t, want %t", tc.value, got, tc.want)
		}
	}
}

func TestMaskOutboundHeaderValue(t *testing.T) {
	cases := []struct {
		value string
		want  string
	}{
		{"", ""},
		{"short", "***"},
		{"12345678", "***"},
		{"123456789", "1234***6789"},
		{"Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0", "Bear***MjM0"},
	}
	for _, tc := range cases {
		if got := maskOutboundHeaderValue(tc.value); got != tc.want {
			t.Errorf("maskOutboundHeaderValue(%q) = %q, want %q", tc.value, got, tc.want)
		}
	}
}

func TestFormatOutboundHeadersSortsAndMasks(t *testing.T) {
	headers := http.Header{
		"User-Agent":      {"codex/1.0"},
		"Authorization":   {"Bearer secret-token-value"},
		"Cookie":          {"sessionKey=abc123"},
		"Content-Type":    {"application/json"},
		"Statsig-Api-Key": {"client-secret-statsig-value"},
	}
	got := formatOutboundHeaders(headers)
	want := "Authorization: Bear***alue; Content-Type: application/json; Cookie: sess***c123; Statsig-Api-Key: clie***alue; User-Agent: codex/1.0"
	if got != want {
		t.Fatalf("formatOutboundHeaders = %q, want %q", got, want)
	}
	if strings.Contains(got, "secret-token-value") || strings.Contains(got, "abc123") || strings.Contains(got, "client-secret-statsig-value") {
		t.Fatalf("sensitive values leaked into header log: %q", got)
	}
}

// recordCloseIdleRoundTripper 记录 CloseIdleConnections 调用次数的假 transport。
type recordCloseIdleRoundTripper struct {
	closed atomic.Int32
	status int
}

func (r *recordCloseIdleRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: r.status,
		Header:     make(http.Header),
		Body:       http.NoBody,
	}, nil
}

func (r *recordCloseIdleRoundTripper) CloseIdleConnections() {
	r.closed.Add(1)
}

func TestWrapOutboundHeaderLogPassesThroughAndClosesIdleConnections(t *testing.T) {
	inner := &recordCloseIdleRoundTripper{status: http.StatusNoContent}
	client := &http.Client{Transport: WrapOutboundHeaderLog("test", inner)}

	req, err := http.NewRequest(http.MethodGet, "http://upstream.test/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("wrapped RoundTrip failed: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	_ = resp.Body.Close()

	client.CloseIdleConnections()
	if got := inner.closed.Load(); got != 1 {
		t.Fatalf("CloseIdleConnections forwarded %d times, want 1", got)
	}
}

func TestWrapOutboundHeaderLogUnwrapRoundTrip(t *testing.T) {
	inner := http.DefaultTransport.(*http.Transport).Clone()
	wrapped := WrapOutboundHeaderLog("test", inner)

	if UnwrapOutboundHeaderLog(wrapped) != inner {
		t.Fatal("UnwrapOutboundHeaderLog must strip the wrapper")
	}
	if UnwrapOutboundHeaderLog(inner) != inner {
		t.Fatal("UnwrapOutboundHeaderLog must pass through non-wrapped transports")
	}
	if WrapOutboundHeaderLog("test", nil) != nil {
		t.Fatal("WrapOutboundHeaderLog(nil) must return nil")
	}
}

func TestLogOutboundHTTPRequestDisabledByDefault(t *testing.T) {
	// 默认关闭时不应 panic；开启时由 log.Printf 输出，无法在此捕获，仅验证构建路径。
	t.Setenv(outboundHeaderLogEnv, "0")
	u, err := url.Parse("https://example.com/x")
	if err != nil {
		t.Fatal(err)
	}
	LogOutboundWSHandshake("codex-ws", "wss://chatgpt.com/backend-api/codex/socket", http.Header{"Authorization": {"Bearer abcdefghij"}})
	LogOutboundHTTPRequest("probe", &http.Request{Method: "GET", URL: u, Header: http.Header{}})
}
