package promptfilter

import (
	"fmt"
	"testing"
)

func TestResponsesConfigurationUpdateIsNotPromptText(t *testing.T) {
	const update = `{"type":"configuration_update","reasoning":{"effort":"high"}}`
	for _, input := range []string{
		`[` + update + `,{"role":"user","content":"Check the current request."}]`,
		`[{"role":"user","content":"Check the current request."},` + update + `]`,
		`[{"type":"input_text","text":"Check the current request."},` + update + `]`,
		`[{"role":"user","content":"Check the current request."},{"type":"configuration_update","role":"assistant","reasoning":{"effort":"high"}}]`,
		`[{"role":"user","content":"Check the current request."},{"type":"configuration_update","role":"user","content":"not a prompt","reasoning":{"effort":"high"}}]`,
	} {
		for _, transport := range []Transport{TransportHTTP, TransportWebSocket} {
			t.Run(fmt.Sprintf("%s/%s", transport, input), func(t *testing.T) {
				body := []byte(`{"input":` + input + `}`)
				envelope := BuildEnvelope(body, "/v1/responses", "gpt-6-astra", transport, DefaultMaxTextLength)
				if envelope.AdapterUnclassified {
					t.Fatalf("configuration update is a known control item: %+v", envelope)
				}
				current := envelope.SegmentsForOrigin(OriginCurrentUser)
				if len(current) != 1 || current[0].Text != "Check the current request." {
					t.Fatalf("control item changed current prompt selection: %+v", current)
				}
				if len(envelope.Segments) != 1 {
					t.Fatalf("control item leaked into prompt text: %+v", envelope.Segments)
				}
				if got := ExtractText(body, "/v1/responses", DefaultMaxTextLength); got != current[0].Text {
					t.Fatalf("ExtractText = %q, want %q", got, current[0].Text)
				}
			})
		}
	}

	body := []byte(`{"input":[{"role":"user","content":"Earlier request."},{"role":"assistant","content":"Earlier answer."},` + update + `]}`)
	envelope := BuildEnvelope(body, "/v1/responses", "gpt-6-astra", TransportHTTP, DefaultMaxTextLength)
	if envelope.AdapterUnclassified || len(envelope.SegmentsForOrigin(OriginCurrentUser)) != 0 {
		t.Fatalf("control item must not resurrect a historical prompt: %+v", envelope)
	}
}
