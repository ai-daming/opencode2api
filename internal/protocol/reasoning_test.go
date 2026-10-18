package protocol

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestReasoningConversionCompatibility(t *testing.T) {
	cases := []struct {
		name     string
		from, to Protocol
		controls string
		want     string
	}{
		{"disabled-alias", Chat, Responses, `{"reasoning_effort":"disabled"}`, `{"reasoning":{"effort":"none"}}`},
		{"normalized-none", Chat, Anthropic, `{"reasoning_effort":" NONE "}`, `{"thinking":{"type":"disabled"}}`},
		{"chat-object", Chat, Anthropic, `{"reasoning":{"type":"disabled"}}`, `{"thinking":{"type":"disabled"}}`},
		{"anthropic-output-effort", Anthropic, Responses, `{"output_config":{"effort":"none"},"thinking":{"type":"enabled","budget_tokens":8192}}`, `{"reasoning":{"effort":"none"}}`},
		{"anthropic-top-effort", Anthropic, Chat, `{"effort":"none"}`, `{"reasoning_effort":"none"}`},
		{"disabled-over-budget", Anthropic, Chat, `{"thinking":{"type":"disabled","budget_tokens":8192},"output_config":{"effort":"max"}}`, `{"reasoning_effort":"none"}`},
		{"chat-native-disabled", Chat, Responses, `{"thinking":{"type":"disabled"},"reasoning_effort":"high"}`, `{"reasoning":{"effort":"none"}}`},
		{"responses-disabled", Responses, Anthropic, `{"reasoning":{"effort":"none","summary":"detailed"}}`, `{"thinking":{"type":"disabled"}}`},
		{"explicit-max-over-budget", Anthropic, Chat, `{"thinking":{"type":"enabled","budget_tokens":2048},"output_config":{"effort":"max"}}`, `{"reasoning_effort":"max"}`},
		{"budget-32000", Anthropic, Responses, `{"thinking":{"type":"enabled","budget_tokens":32000}}`, `{"reasoning":{"effort":"xhigh"}}`},
		{"enabled-high", Chat, Anthropic, `{"reasoning_effort":"high"}`, `{"thinking":{"type":"enabled","budget_tokens":8192},"output_config":{"effort":"high"}}`},
		{"unknown-explicit-effort", Chat, Responses, `{"reasoning_effort":"future-level"}`, `{"reasoning":{"effort":"future-level"}}`},
		{"null-chat", Chat, Anthropic, `{"reasoning_effort":null}`, `{}`},
		{"null-anthropic", Anthropic, Responses, `{"thinking":null}`, `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var input, want map[string]any
			if err := json.Unmarshal([]byte(tc.controls), &input); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatal(err)
			}
			input["model"] = "test-model"
			input["max_tokens"] = float64(36096)
			if tc.from == Responses {
				input["input"] = "Reply OK"
			} else {
				input["messages"] = []any{map[string]any{"role": "user", "content": "Reply OK"}}
			}
			before, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			output, err := PrepareRequest(tc.from, tc.to, input, "https://example.invalid/v1")
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]any{}
			for _, key := range []string{"reasoning_effort", "reasoning", "thinking", "output_config"} {
				if value, ok := output[key]; ok {
					got[key] = value
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("controls = %#v, want %#v", got, want)
			}
			after, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Error("conversion changed the source request")
			}
		})
	}
}

func TestNativeDisabledRequestPreservesOtherFields(t *testing.T) {
	input := map[string]any{
		"model": "test-model", "max_tokens": float64(1024),
		"thinking":      map[string]any{"type": "disabled"},
		"output_config": map[string]any{"format": map[string]any{"type": "json_schema", "schema": map[string]any{"type": "object"}}},
		"messages":      []any{map[string]any{"role": "user", "content": "Reply OK"}},
	}
	for _, effort := range []string{"high", "none"} {
		t.Run(effort, func(t *testing.T) {
			output, err := PrepareRequest(Anthropic, Anthropic, input, "https://example.invalid/v1")
			if err != nil {
				t.Fatal(err)
			}
			ForcedEffort(Anthropic, output, effort)
			if !reflect.DeepEqual(output, input) {
				t.Errorf("default %q changed disabled request: %#v", effort, output)
			}
			output["thinking"].(map[string]any)["type"] = "enabled"
			if input["thinking"].(map[string]any)["type"] != "disabled" {
				t.Error("prepared body shares mutable state with input")
			}
		})
	}
}
