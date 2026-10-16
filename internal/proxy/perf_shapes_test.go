package proxy

import "testing"

// TestFirstOutputProviderShapes probes detectFirstOutput with real provider
// event shapes (review of SPEC "Performance metrics → Capture").
func TestFirstOutputProviderShapes(t *testing.T) {
	cases := []struct {
		name            string
		event, data     string
		output, reasons bool
	}{
		{"openai role chunk", "", `{"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"","refusal":null},"logprobs":null,"finish_reason":null}]}`, false, false},
		{"openai content", "", `{"choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}`, true, false},
		{"openai tool call", "", `{"choices":[{"index":0,"delta":{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":""}}]}}]}`, true, false},
		{"openai empty tool_calls", "", `{"choices":[{"index":0,"delta":{"tool_calls":[]}}]}`, false, false},
		{"openai usage only", "", `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}`, false, false},
		{"openai finish chunk", "", `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, false, false},
		{"ollama reasoning", "", `{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"qwen3","choices":[{"index":0,"delta":{"role":"assistant","content":"","reasoning":"Okay"},"finish_reason":null}]}`, true, true},
		{"llama.cpp reasoning_content", "", `{"choices":[{"finish_reason":null,"index":0,"delta":{"reasoning_content":"Let"}}],"object":"chat.completion.chunk"}`, true, true},
		{"llama.cpp role null content", "", `{"choices":[{"finish_reason":null,"index":0,"delta":{"role":"assistant","content":null}}],"object":"chat.completion.chunk"}`, false, false},
		{"mixed reasoning+content", "", `{"choices":[{"index":0,"delta":{"reasoning":"a","content":"b"}}]}`, true, false},
		{"responses created", "response.created", `{"type":"response.created","response":{"id":"r","output":[]}}`, false, false},
		{"responses item added", "response.output_item.added", `{"type":"response.output_item.added","item":{"type":"message","content":[]}}`, false, false},
		{"responses text delta", "response.output_text.delta", `{"type":"response.output_text.delta","item_id":"m","output_index":0,"content_index":0,"delta":"Hi"}`, true, false},
		{"responses empty delta", "response.output_text.delta", `{"type":"response.output_text.delta","delta":""}`, false, false},
		{"responses summary delta", "response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","item_id":"rs","output_index":0,"summary_index":0,"delta":"**Plan**"}`, true, true},
		{"responses reasoning text", "", `{"type":"response.reasoning_text.delta","delta":"x"}`, true, true},
		{"responses fn args", "response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","item_id":"fc","output_index":1,"delta":"{\"a\""}`, true, false},
		{"responses fn args via event name", "response.function_call_arguments.delta", `{"item_id":"fc","delta":"{"}`, true, false},
		{"responses refusal", "", `{"type":"response.refusal.delta","delta":"no"}`, true, false},
	}
	for _, c := range cases {
		out, reasoning := detectFirstOutput(c.event, []byte(c.data))
		if out != c.output || (out && reasoning != c.reasons) {
			t.Errorf("%s: output=%v reasoning=%v, want %v %v", c.name, out, reasoning, c.output, c.reasons)
		}
	}
}
