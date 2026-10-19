package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const configurationUpdateRequest = `{
	"model":"gpt-6-astra",
	"stream":true,
	"reasoning":{"effort":"medium","summary":"auto"},
	"input":[
		{"type":"configuration_update","reasoning":{"effort":"high"}},
		{"role":"user","content":"hello"},
		{"role":"assistant","content":"OK"},
		{"type":"configuration_update","reasoning":{"effort":"high"}},
		{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_1","output":"ok"},
		{"role":"user","content":"continue"},
		{"type":"configuration_update","reasoning":{"effort":"max"}}
	]
}`

func assertConfigurationUpdateRequest(t *testing.T, got, want []byte) {
	t.Helper()
	for _, path := range []string{"reasoning", "input"} {
		if !reflect.DeepEqual(gjson.GetBytes(got, path).Value(), gjson.GetBytes(want, path).Value()) {
			t.Fatalf("%s changed while forwarding configuration updates: %s", path, got)
		}
	}
}

func TestPrepareResponsesPreservesConfigurationUpdates(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func([]byte) []byte
	}{
		{"codex HTTP", func(body []byte) []byte { got, _ := PrepareResponsesBody(body); return got }},
		{"codex WebSocket", func(body []byte) []byte { got, _ := PrepareResponsesWebSocketBody(body); return got }},
		{"codex native turn", func(body []byte) []byte { got, _ := prepareResponsesWebSocketTurnBody(body); return got }},
		{"responses relay", PrepareOpenAIResponsesBody},
		{"codex compact", func(body []byte) []byte { got, _ := PrepareCompactResponsesBody(body); return got }},
		{"relay compact", PrepareOpenAIResponsesCompactBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(configurationUpdateRequest)
			assertConfigurationUpdateRequest(t, tc.prepare(body), body)
			if string(body) != configurationUpdateRequest {
				t.Fatal("preparation mutated caller's request")
			}
		})
	}
}

func TestConfigurationUpdatesSurviveResponseCacheReplay(t *testing.T) {
	resetResponseCacheForTest()
	t.Cleanup(resetResponseCacheForTest)
	body := []byte(configurationUpdateRequest)
	completed := []byte(`{"type":"response.completed","response":{"id":"resp_configuration","output":[{"type":"function_call","call_id":"call_next","name":"lookup","arguments":"{}"}]}}`)
	cacheCompletedResponse("key:1", []byte(gjson.GetBytes(body, "input").Raw), completed)

	next := []byte(`{"model":"gpt-6-astra","reasoning":{"effort":"medium","summary":"auto"},"previous_response_id":"resp_configuration","input":[{"type":"function_call_output","call_id":"call_next","output":"done"}]}`)
	prepared := prepareResponsesBodyForOwnerDetailed(next, "key:1")
	if prepared.CacheLookup.Kind != responseCacheLookupHit {
		t.Fatalf("expected a cached continuation: %+v", prepared.CacheLookup)
	}
	wantInput := append(gjson.GetBytes(body, "input").Array(), gjson.GetBytes(completed, "response.output").Array()...)
	wantInput = append(wantInput, gjson.GetBytes(next, "input").Array()...)
	gotInput := gjson.GetBytes(prepared.Body, "input").Array()
	if len(gotInput) != len(wantInput) {
		t.Fatalf("replayed input length = %d, want %d", len(gotInput), len(wantInput))
	}
	for i := range wantInput {
		if !reflect.DeepEqual(gotInput[i].Value(), wantInput[i].Value()) {
			t.Fatalf("replayed item %d changed: %s, want %s", i, gotInput[i].Raw, wantInput[i].Raw)
		}
	}
	if gjson.GetBytes(prepared.Body, "reasoning.effort").String() != "medium" || extractReasoningEffort(prepared.Body) != "max" {
		t.Fatalf("replay confused base effort with effective effort: %s", prepared.Body)
	}
	foreign := prepareResponsesBodyForOwnerDetailed(next, "key:2")
	if foreign.CacheLookup.Kind == responseCacheLookupHit {
		t.Fatal("configuration history crossed owner boundary")
	}
}

func TestConfigurationUpdatesSurviveWebSocketReplay(t *testing.T) {
	resetResponseCacheForTest()
	t.Cleanup(resetResponseCacheForTest)
	body := []byte(configurationUpdateRequest)
	completed := []byte(`{"type":"response.completed","response":{"id":"resp_configuration_ws","output":[{"type":"message","role":"assistant","content":"done"}]}}`)
	cacheResponsesWSCompletedResponse("key:ws", gjson.GetBytes(body, "input").Raw, completed, nil)

	next := []byte(`{"model":"gpt-6-astra","reasoning":{"effort":"medium","summary":"auto"},"previous_response_id":"resp_configuration_ws","input":[{"role":"user","content":"next"}]}`)
	native, _ := PrepareResponsesWebSocketBody(next)
	assertConfigurationUpdateRequest(t, native, next)
	if gjson.GetBytes(native, "previous_response_id").String() != "resp_configuration_ws" {
		t.Fatal("native continuation lost its response linkage")
	}
	fallback, apiErr := degradeResponsesWSContinuationBody(native, "key:ws")
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if gjson.GetBytes(fallback, "previous_response_id").Exists() {
		t.Fatal("materialized fallback must not also ask upstream to prepend history")
	}
	wantInput := append(gjson.GetBytes(body, "input").Array(), gjson.GetBytes(completed, "response.output").Array()...)
	wantInput = append(wantInput, gjson.GetBytes(next, "input").Array()...)
	gotInput := gjson.GetBytes(fallback, "input").Array()
	if len(gotInput) != len(wantInput) {
		t.Fatalf("WebSocket replay length = %d, want %d", len(gotInput), len(wantInput))
	}
	for i := range wantInput {
		if !reflect.DeepEqual(gotInput[i].Value(), wantInput[i].Value()) {
			t.Fatalf("WebSocket replay changed item %d: %s", i, gotInput[i].Raw)
		}
	}
	if gjson.GetBytes(fallback, "reasoning.effort").String() != "medium" || extractReasoningEffort(fallback) != "max" {
		t.Fatalf("WebSocket replay confused base and effective effort: %s", fallback)
	}
}

func TestExtractReasoningEffortConfigurationUpdates(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"last positional update wins", configurationUpdateRequest, "max"},
		{"request base", `{"reasoning":{"effort":"low"},"input":"hello"}`, "low"},
		{"chat completions", `{"reasoning_effort":"high","messages":[]}`, "high"},
		{"new update on continuation", `{"previous_response_id":"resp_old","reasoning":{"effort":"medium"},"input":[{"type":"configuration_update","reasoning":{"effort":"low"}}]}`, "low"},
		{"inherited effort is unknown", `{"previous_response_id":"resp_old","reasoning":{"effort":"medium"},"input":[{"role":"user","content":"continue"}]}`, ""},
		{"empty previous id is not continuation", `{"previous_response_id":"","reasoning":{"effort":"medium"}}`, "medium"},
		{"do not interpret tool output", `{"reasoning":{"effort":"low"},"input":[{"type":"function_call_output","output":{"type":"configuration_update","reasoning":{"effort":"high"}}}]}`, "low"},
		{"do not interpret message reasoning", `{"reasoning":{"effort":"low"},"input":[{"role":"user","reasoning":{"effort":"high"}}]}`, "low"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			if got := extractReasoningEffort(body); got != tc.want {
				t.Fatalf("effort = %q, want %q", got, tc.want)
			}
			if string(body) != tc.body {
				t.Fatal("reading effective effort must not rewrite the request")
			}
		})
	}
}

func TestCodexTelemetryConfigurationUpdateEffort(t *testing.T) {
	for _, tc := range []struct {
		body string
		want string
	}{
		{configurationUpdateRequest, "max"},
		{`{"model":"gpt-6-astra"}`, "medium"},
		{`{"model":"gpt-6-astra","previous_response_id":"resp_old","reasoning":{"effort":"medium"}}`, ""},
	} {
		profile := testCodexTelemetryProfile()
		profile = buildCodexTelemetryProfile(profile.client, codexTelemetryRequest{body: []byte(tc.body)})
		if profile.effort != tc.want {
			t.Fatalf("telemetry effort = %q, want %q", profile.effort, tc.want)
		}
		params := codexMainTurnEvent(profile, codexTelemetryTerminal{status: "completed"}).EventParams
		if tc.want == "" {
			if _, exists := params["reasoning_effort"]; exists {
				t.Fatal("telemetry must omit unknown inherited effort")
			}
		} else if params["reasoning_effort"] != tc.want {
			t.Fatalf("telemetry event effort = %v, want %q", params["reasoning_effort"], tc.want)
		}
	}
}

func TestResponsesConfigurationUpdateForwarding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		mode   string
		status int
	}{
		{"codex HTTP", http.StatusOK},
		{"codex WebSocket", http.StatusOK},
		{"responses relay", http.StatusOK},
		{"codex rejects update", http.StatusBadRequest},
		{"responses relay rejects update", http.StatusBadRequest},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			mode := tc.mode
			isolateCodexTelemetry(t)
			previousSettings := CurrentRuntimeSettings()
			previousResin := resinCfg.Load()
			previousExec := WebsocketExecuteFunc
			t.Cleanup(func() {
				ApplyRuntimeSettings(previousSettings)
				resinCfg.Store(previousResin)
				WebsocketExecuteFunc = previousExec
			})

			var calls atomic.Int32
			bodyCh := make(chan []byte, 2)
			doneCh := make(chan struct{}, 2)
			const sse = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_update\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				bodyCh <- readUpstreamRequestBody(r)
				if tc.status != http.StatusOK {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, `{"error":{"message":"Unsupported input type configuration_update","type":"invalid_request_error","code":"invalid_parameter"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, sse)
			}))
			t.Cleanup(upstream.Close)
			SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})
			WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, body []byte, sessionID, proxyURL, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
				calls.Add(1)
				bodyCh <- bytes.Clone(body)
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sse))}, nil
			}

			db := newTestModelRegistryDB(t)
			id, err := db.InsertAccount(context.Background(), "configuration-update", "test-refresh", "")
			if err != nil {
				t.Fatal(err)
			}
			store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
			t.Cleanup(store.Stop)
			account := &auth.Account{DBID: id, AccessToken: "at", PlanType: "plus", AccountID: "acct-update"}
			if strings.HasPrefix(mode, "responses relay") {
				account = &auth.Account{DBID: id, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: upstream.URL, APIKey: "test-key", Models: []string{"gpt-6-astra"}}
			}
			store.AddAccount(account)
			handler := NewHandler(store, db, &config.Config{AllowAnonymousV1: true}, nil)
			settings := CurrentRuntimeSettings()
			settings.CodexForceWebsocket = false
			ApplyRuntimeSettings(settings)
			router := gin.New()
			router.Use(func(c *gin.Context) {
				c.Next()
				doneCh <- struct{}{}
			})
			handler.RegisterRoutes(router)

			if mode == "codex WebSocket" {
				server := httptest.NewServer(router)
				t.Cleanup(server.Close)
				conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = conn.Close() })
				if err := conn.WriteMessage(websocket.TextMessage, []byte(configurationUpdateRequest)); err != nil {
					t.Fatal(err)
				}
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				_, payload, err := conn.ReadMessage()
				if err != nil || gjson.GetBytes(payload, "type").String() != "response.completed" {
					t.Fatalf("WebSocket response = %s, err = %v", payload, err)
				}
				_ = conn.Close()
			} else {
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(configurationUpdateRequest))
				request.Header.Set("Content-Type", "application/json")
				router.ServeHTTP(recorder, request)
				if recorder.Code != tc.status || tc.status == http.StatusOK && !strings.Contains(recorder.Body.String(), "response.completed") {
					t.Fatalf("HTTP response = %d %s", recorder.Code, recorder.Body.String())
				}
			}
			select {
			case got := <-bodyCh:
				assertConfigurationUpdateRequest(t, got, []byte(configurationUpdateRequest))
			case <-time.After(5 * time.Second):
				t.Fatal("request never reached upstream")
			}
			select {
			case <-doneCh:
			case <-time.After(5 * time.Second):
				t.Fatal("handler did not complete")
			}

			// WebSocket turns have isolated contexts after multiplexing. Check
			// their persisted usage, not the parent connection's middleware state.
			if tc.status == http.StatusOK {
				db.FlushUsageLogs()
				logs, err := db.ListUsageLogsByTimeRange(context.Background(), time.Now().Add(-time.Minute), time.Now().Add(time.Minute))
				if err != nil || len(logs) != 1 {
					t.Fatalf("usage logs = %v, err = %v", logs, err)
				}
				if logs[0].ReasoningEffort != "max" {
					t.Fatalf("logged effort = %q, want max", logs[0].ReasoningEffort)
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("upstream calls = %d, want one without an automatic downgrade retry", calls.Load())
			}
			if mode != "codex WebSocket" {
				invalid, _ := sjson.Set(configurationUpdateRequest, "input.7.reasoning.effort", 42)
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(invalid))
				request.Header.Set("Content-Type", "application/json")
				router.ServeHTTP(recorder, request)
				if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "input.7.reasoning.effort") {
					t.Fatalf("invalid update = %d %s", recorder.Code, recorder.Body.String())
				}
				select {
				case body := <-bodyCh:
					t.Fatalf("invalid update reached upstream: %s", body)
				default:
				}
			}
		})
	}
}
