package api

import (
	"strings"
	"testing"
)

func TestValidateResponsesConfigurationUpdate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		item  string
		field string
	}{
		{"high", `{"type":"configuration_update","reasoning":{"effort":"high"}}`, ""},
		{"max", `{"type":"configuration_update","reasoning":{"effort":"max"}}`, ""},
		{"upstream decides supported effort", `{"type":"configuration_update","reasoning":{"effort":"future-effort"}}`, ""},
		{"missing reasoning", `{"type":"configuration_update"}`, "reasoning"},
		{"null reasoning", `{"type":"configuration_update","reasoning":null}`, "reasoning"},
		{"string reasoning", `{"type":"configuration_update","reasoning":"high"}`, "reasoning"},
		{"array reasoning", `{"type":"configuration_update","reasoning":[]}`, "reasoning"},
		{"missing effort", `{"type":"configuration_update","reasoning":{}}`, "reasoning.effort"},
		{"null effort", `{"type":"configuration_update","reasoning":{"effort":null}}`, "reasoning.effort"},
		{"empty effort", `{"type":"configuration_update","reasoning":{"effort":""}}`, "reasoning.effort"},
		{"blank effort", `{"type":"configuration_update","reasoning":{"effort":" \t"}}`, "reasoning.effort"},
		{"number effort", `{"type":"configuration_update","reasoning":{"effort":1}}`, "reasoning.effort"},
		{"bool effort", `{"type":"configuration_update","reasoning":{"effort":true}}`, "reasoning.effort"},
		{"object effort", `{"type":"configuration_update","reasoning":{"effort":{}}}`, "reasoning.effort"},
		{"array effort", `{"type":"configuration_update","reasoning":{"effort":[]}}`, "reasoning.effort"},
		{"oversized effort", `{"type":"configuration_update","reasoning":{"effort":"` + strings.Repeat("x", 65) + `"}}`, "reasoning.effort"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"gpt-6-astra","reasoning":{"effort":"medium"},"input":[{"role":"user","content":"hello"},` + tc.item + `]}`)
			result := ValidateResponsesAPIRequest(body, []string{"gpt-6-astra"})
			if tc.field == "" {
				if !result.Valid {
					t.Fatalf("valid configuration update rejected: %+v", result.Errors)
				}
				return
			}
			if result.Valid || len(result.Errors) != 1 {
				t.Fatalf("expected one validation error, got %+v", result)
			}
			if want := "input.1." + tc.field; result.Errors[0].Field != want {
				t.Fatalf("field = %q, want %q", result.Errors[0].Field, want)
			}
		})
	}
}
