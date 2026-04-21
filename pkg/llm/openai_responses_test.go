package llm

import (
	"strings"
	"testing"
)

func TestParseResponsesSSEAccumulatesDeltaAndUsage(t *testing.T) {
	t.Parallel()

	stream := strings.Join([]string{
		"event: response.output_text.delta",
		`data: {"delta":"Hello "}`,
		"",
		"event: response.output_text.delta",
		`data: {"delta":"world"}`,
		"",
		"event: response.completed",
		`data: {"response":{"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}}`,
		"",
	}, "\n")

	text, usage, err := parseResponsesSSE(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("parse SSE: %v", err)
	}
	if text != "Hello world" {
		t.Fatalf("text = %q, want %q", text, "Hello world")
	}
	if usage.InputTokens != 11 || usage.OutputTokens != 7 || usage.TotalTokens != 18 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
}

func TestParseResponsesSSESupportsLargePayloadAndDoneFallback(t *testing.T) {
	t.Parallel()

	large := strings.Repeat("x", 70_000)
	stream := strings.Join([]string{
		"event: response.output_text.done",
		`data: {"text":"` + large + `"}`,
		"",
		"event: response.completed",
		`data: {"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}`,
		"",
	}, "\n")

	text, usage, err := parseResponsesSSE(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("parse SSE: %v", err)
	}
	if len(text) != len(large) {
		t.Fatalf("text len = %d, want %d", len(text), len(large))
	}
	if usage.TotalTokens != 8 {
		t.Fatalf("total_tokens = %d, want 8", usage.TotalTokens)
	}
}
