package main

import "testing"

// A phase boundary stops the tool loop early, but the assistant message still
// declares every tool call it asked for. An OpenAI-shaped API rejects a
// transcript where a tool_call has no matching tool message, so the phase
// checkpoint — which replays that history — failed with a bare 400 and
// stranded the job ("couldn't safely cross into the next phase").
func TestAnswerUnrunToolCalls(t *testing.T) {
	for _, tc := range []struct {
		name  string
		unrun []ToolCall
		want  []string
	}{
		{"nothing skipped leaves the transcript alone", nil, nil},
		{"one skipped call is answered", []ToolCall{{ID: "b"}}, []string{"b"}},
		{"every skipped call is answered", []ToolCall{{ID: "b"}, {ID: "c"}, {ID: "d"}}, []string{"b", "c", "d"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := []Msg{
				{Role: "assistant", ToolCalls: []ToolCall{{ID: "a"}}},
				{Role: "tool", ToolCallID: "a", Content: "ran"},
			}
			got := answerUnrunToolCalls(append([]Msg(nil), before...), tc.unrun)

			if len(got) != len(before)+len(tc.want) {
				t.Fatalf("appended %d messages, want %d", len(got)-len(before), len(tc.want))
			}
			for i, id := range tc.want {
				m := got[len(before)+i]
				if m.Role != "tool" {
					t.Errorf("message for %q has role %q, want tool", id, m.Role)
				}
				if m.ToolCallID != id {
					t.Errorf("tool_call_id = %q, want %q", m.ToolCallID, id)
				}
				if m.Content == "" {
					t.Errorf("tool message for %q has empty content; the API needs a body", id)
				}
			}
		})
	}
}

// The invariant the API actually enforces: after the loop, every declared
// tool_call has exactly one matching tool message.
func TestEveryDeclaredToolCallIsAnswered(t *testing.T) {
	declared := []ToolCall{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	messages := []Msg{
		{Role: "assistant", ToolCalls: declared},
		{Role: "tool", ToolCallID: "a", Content: "ran"}, // boundary tripped here
	}
	messages = answerUnrunToolCalls(messages, declared[1:])

	answered := map[string]int{}
	for _, m := range messages {
		if m.Role == "tool" {
			answered[m.ToolCallID]++
		}
	}
	for _, call := range declared {
		if answered[call.ID] != 1 {
			t.Fatalf("tool_call %q answered %d times, want exactly 1", call.ID, answered[call.ID])
		}
	}
}
