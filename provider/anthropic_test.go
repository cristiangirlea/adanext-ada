package provider_test

import (
	"net/http"
	"testing"
)

func TestAnthropicConversation(t *testing.T) {
	testConversation(t, "anthropic", "/v1/messages", func(t *testing.T, r *http.Request) {
		if r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("x-api-key") != "local" {
			t.Errorf("unexpected Anthropic headers: %v", r.Header)
		}
	}, []string{
		`{"model":"configured","system":"Be brief.","max_tokens":123,"messages":[{"role":"user","content":[{"type":"text","text":"A question"}]}],"tools":[{"name":"lookup","description":"Look it up.","input_schema":{"type":"object"}}]}`,
		`{"model":"configured","system":"Be brief.","max_tokens":123,"messages":[{"role":"user","content":[{"type":"text","text":"A question"}]},{"role":"assistant","content":[{"type":"text","text":"Checking."},{"type":"tool_use","id":"call-1","name":"lookup","input":{"query":"value"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"found","is_error":false}]}],"tools":[{"name":"lookup","description":"Look it up.","input_schema":{"type":"object"}}]}`,
	}, []string{
		`{"content":[{"type":"thinking","text":{"opaque":true}},{"type":"text","text":"Checking."},{"type":"tool_use","id":"call-1","name":"lookup","input":{"query":"value"}}],"stop_reason":"tool_use","usage":{"input_tokens":4,"output_tokens":5}}`,
		`{"content":[{"type":"text","text":"It was found."}],"stop_reason":"end_turn","usage":{"input_tokens":8,"output_tokens":9}}`,
	})
}
