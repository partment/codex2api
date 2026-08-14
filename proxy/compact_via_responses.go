package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ==================== compact 端点 body-signal 兼容 ====================
// 上游已下线专用的 /responses/compact 端点（返回 404），会话压缩迁移到普通
// /responses 请求体内嵌 type=compaction_trigger 的 input item（新版 Codex CLI
// 的 body-signal 形态）。系统设置 compact_via_responses_enabled 开启后，官方
// Codex OAuth 账号的 /v1/responses/compact 透传改写为该形态：注入触发器走
// /responses SSE，再把流聚合回 compact 的一次性 JSON 返回下游。中转（OpenAI
// Responses API）账号不受影响，仍请求中转自身的 /responses/compact。

// appendCompactionTriggerToResponsesBody 把已准备好的 Codex compact body 改写成
// body-signal 压缩形态。compact 预处理为旧专用端点剥除了 store/stream/include，
// 而 /responses 上游要求 stream=true 且 store=false（实测缺 store 直接
// 400 "Store must be set to false"），这里补回，并在 input 末尾追加
// compaction_trigger。客户端已经携带触发器时也会归一到 input 最后一项。
func appendCompactionTriggerToResponsesBody(body []byte) []byte {
	out, _ := sjson.SetBytes(body, "stream", true)
	out, _ = sjson.SetBytes(out, "store", false)
	if !gjson.GetBytes(out, "include").Exists() {
		out, _ = sjson.SetBytes(out, "include", []string{codexReasoningEncryptedContentInclude})
	}
	return normalizeCompactionTriggerFinal(out, true)
}

// normalizeCompactionTriggerFinal keeps at most one direct input-level
// compaction_trigger and moves it behind every history/message/tool item.
// Upstream rejects a trigger followed by any other input item with
// "The 'compaction_trigger' item must be the final input item."
//
// addIfMissing is true only when rewriting the legacy /responses/compact
// endpoint to body-signal compaction. Plain /responses preparation uses false:
// it repairs the order only when the client already supplied a trigger.
func normalizeCompactionTriggerFinal(body []byte, addIfMissing bool) []byte {
	trigger := json.RawMessage(`{"type":"compaction_trigger"}`)
	input := gjson.GetBytes(body, "input")
	switch {
	case input.IsArray():
		items := input.Array()
		triggerCount := 0
		for _, item := range items {
			if isDirectCompactionTrigger(item) {
				triggerCount++
			}
		}
		if triggerCount == 0 && !addIfMissing {
			return body
		}
		if triggerCount == 1 && len(items) > 0 && isCanonicalCompactionTrigger(items[len(items)-1]) {
			return body
		}

		normalized := make([]json.RawMessage, 0, len(items)+1)
		for _, item := range items {
			if isDirectCompactionTrigger(item) {
				continue
			}
			raw := strings.TrimSpace(item.Raw)
			if raw == "" {
				encoded, err := json.Marshal(item.Value())
				if err != nil {
					return body
				}
				raw = string(encoded)
			}
			normalized = append(normalized, json.RawMessage(raw))
		}
		normalized = append(normalized, trigger)
		encoded, err := json.Marshal(normalized)
		if err != nil {
			return body
		}
		out, err := sjson.SetRawBytes(body, "input", encoded)
		if err != nil {
			return body
		}
		return out
	case input.IsObject() && isDirectCompactionTrigger(input):
		encoded, err := json.Marshal([]json.RawMessage{trigger})
		if err != nil {
			return body
		}
		out, err := sjson.SetRawBytes(body, "input", encoded)
		if err != nil {
			return body
		}
		return out
	case addIfMissing:
		normalized := make([]json.RawMessage, 0, 2)
		switch {
		case input.Type == gjson.String:
			// input 为字符串时先转成标准 message item，再追加触发器。
			msg, err := json.Marshal(map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": input.String()},
				},
			})
			if err != nil {
				return body
			}
			normalized = append(normalized, json.RawMessage(msg))
		case input.IsObject() && !isDirectCompactionTrigger(input):
			normalized = append(normalized, json.RawMessage(input.Raw))
		}
		normalized = append(normalized, trigger)
		encoded, err := json.Marshal(normalized)
		if err != nil {
			return body
		}
		out, err := sjson.SetRawBytes(body, "input", encoded)
		if err != nil {
			return body
		}
		return out
	default:
		return body
	}
}

func isDirectCompactionTrigger(item gjson.Result) bool {
	return item.IsObject() &&
		strings.EqualFold(strings.TrimSpace(item.Get("type").String()), "compaction_trigger")
}

func isCanonicalCompactionTrigger(item gjson.Result) bool {
	return item.IsObject() && item.Get("type").String() == "compaction_trigger"
}

// collectCompactResponsesSSE 把 body-signal 压缩的上游 /responses SSE 流聚合成
// compact 端点的一次性 JSON。成功流必须恰好产生一个 compaction item；上游以
// response.failed 终止时原样返回该事件负载，缺少终止事件则视为读错误。
func collectCompactResponsesSSE(body io.Reader, requestBody []byte) (responseJSON []byte, failedPayload []byte, err error) {
	var completedResponse []byte
	var compactionItem json.RawMessage
	compactionDoneCount := 0
	completedSeen := false
	readErr := ReadSSEStream(body, func(data []byte) bool {
		parsed := gjson.ParseBytes(data)
		switch parsed.Get("type").String() {
		case "response.output_item.done":
			item := parsed.Get("item")
			if strings.EqualFold(strings.TrimSpace(item.Get("type").String()), "compaction") {
				compactionDoneCount++
				if compactionItem == nil {
					compactionItem = append(json.RawMessage(nil), item.Raw...)
				}
			}
		case "response.completed":
			completedSeen = true
			response := parsed.Get("response")
			if response.Exists() && response.IsObject() {
				completedResponse = append([]byte(nil), response.Raw...)
			}
			return false
		case "response.failed":
			failedPayload = append([]byte(nil), data...)
			return false
		}
		return true
	})
	if readErr != nil {
		return nil, nil, readErr
	}
	if len(failedPayload) > 0 {
		return nil, failedPayload, nil
	}
	if !completedSeen {
		return nil, nil, errors.New("upstream stream ended before terminal event")
	}
	if len(completedResponse) == 0 {
		return nil, nil, errors.New("response.completed missing response object")
	}

	completedCompactionCount := 0
	gjson.GetBytes(completedResponse, "output").ForEach(func(_, item gjson.Result) bool {
		if strings.EqualFold(strings.TrimSpace(item.Get("type").String()), "compaction") {
			completedCompactionCount++
			if compactionItem == nil {
				compactionItem = append(json.RawMessage(nil), item.Raw...)
			}
		}
		return true
	})
	if compactionDoneCount > 1 || completedCompactionCount > 1 {
		return nil, nil, fmt.Errorf("expected exactly one compaction output item, got done=%d completed=%d", compactionDoneCount, completedCompactionCount)
	}
	if compactionDoneCount == 0 && completedCompactionCount == 0 {
		return nil, nil, errors.New("expected exactly one compaction output item, got none")
	}

	responseJSON, err = buildLegacyCompactionResponse(completedResponse, requestBody, compactionItem)
	return responseJSON, nil, err
}

func buildLegacyCompactionResponse(responseJSON, requestBody []byte, compactionItem json.RawMessage) ([]byte, error) {
	var response map[string]any
	if err := json.Unmarshal(responseJSON, &response); err != nil {
		return nil, fmt.Errorf("decode completed response: %w", err)
	}

	retained := make([]any, 0, 4)
	input := gjson.GetBytes(requestBody, "input")
	if input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			if !item.IsObject() {
				return true
			}
			itemType := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
			role := strings.ToLower(strings.TrimSpace(item.Get("role").String()))
			keep := itemType == "agent_message" ||
				(itemType == "message" && (role == "user" || role == "developer" || role == "system")) ||
				(itemType == "" && (role == "user" || role == "developer" || role == "system"))
			if keep {
				var decoded any
				if json.Unmarshal([]byte(item.Raw), &decoded) == nil {
					retained = append(retained, decoded)
				}
			}
			return true
		})
	}

	var decodedCompaction any
	if err := json.Unmarshal(compactionItem, &decodedCompaction); err != nil {
		return nil, fmt.Errorf("decode compaction output item: %w", err)
	}
	retained = append(retained, decodedCompaction)
	response["object"] = "response.compaction"
	response["output"] = retained
	return json.Marshal(response)
}
