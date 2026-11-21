package provider_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cristiangirlea/adanext-ada/provider"
)

func TestOpenAIConversation(t *testing.T) {
	testConversation(t, "openai", "/v1/chat/completions", func(t *testing.T, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected Authorization header")
		}
	}, []string{
		`{"model":"configured","max_tokens":123,"messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"A question"}],"tools":[{"type":"function","function":{"name":"lookup","description":"Look it up.","parameters":{"type":"object"}}}]}`,
		`{"model":"configured","max_tokens":123,"messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"A question"},{"role":"assistant","content":"Checking.","tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"value\"}"}}]},{"role":"tool","tool_call_id":"call-1","content":"found"}],"tools":[{"type":"function","function":{"name":"lookup","description":"Look it up.","parameters":{"type":"object"}}}]}`,
	}, []string{
		`{"choices":[{"message":{"content":"Checking.","tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"value\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":5}}`,
		`{"choices":[{"message":{"content":"It was found."},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":9}}`,
	})
}

func TestOpenAIInvalidArguments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"tool_calls":[{"id":"a","function":{"name":"lookup","arguments":"{"}}]}}]}`))
	}))
	defer srv.Close()
	p, err := provider.New(provider.Config{Kind: "openai", BaseURL: srv.URL}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Complete(t.Context(), provider.Request{}); err == nil || !strings.Contains(err.Error(), "arguments") {
		t.Fatalf("Complete = %v", err)
	}
}
