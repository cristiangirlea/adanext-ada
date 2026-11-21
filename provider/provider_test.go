package provider_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/cristiangirlea/adanext-ada/loop"
	"github.com/cristiangirlea/adanext-ada/provider"
	"github.com/cristiangirlea/adanext-ada/tool"
)

type lookupTool struct{}

func (lookupTool) Spec() provider.ToolSpec {
	return provider.ToolSpec{Name: "lookup", Description: "Look it up.", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (lookupTool) Effect() tool.Effect { return tool.Read }
func (lookupTool) Run(_ context.Context, input json.RawMessage) (string, error) {
	if string(input) != `{"query":"value"}` {
		return "bad input", nil
	}
	return "found", nil
}

type recordingProvider struct {
	provider.Provider
	responses []provider.Response
}

func (p *recordingProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	res, err := p.Provider.Complete(ctx, req)
	p.responses = append(p.responses, res)
	return res, err
}

func testConversation(t *testing.T, kind, path string, headers func(*testing.T, *http.Request), requests, responses []string) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(calls.Add(1)) - 1
		if i >= len(requests) {
			t.Error("unexpected extra request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.URL.Path != path || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected request: %s %s (%s)", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		headers(t, r)
		var got, want map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		if err := json.Unmarshal([]byte(requests[i]), &want); err != nil {
			t.Error(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("request %d:\ngot  %#v\nwant %#v", i, got, want)
		}
		_, _ = w.Write([]byte(responses[i]))
	}))
	defer srv.Close()

	p, err := provider.New(provider.Config{Name: kind, Kind: kind, Model: "configured", BaseURL: srv.URL}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	recorder := &recordingProvider{Provider: p}
	registry := new(tool.Registry)
	if err := registry.Add(lookupTool{}); err != nil {
		t.Fatal(err)
	}
	l := loop.Loop{Provider: recorder, Tools: registry, System: "Be brief.", MaxTokens: 123}
	answer, history, err := l.Turn(t.Context(), nil, "A question")
	if err != nil || answer != "It was found." || len(history) != 4 || calls.Load() != 2 {
		t.Fatalf("Turn = %q, %v, %v; calls = %d", answer, history, err, calls.Load())
	}
	for i, want := range []struct {
		stop    string
		in, out int
	}{{"tool_use", 4, 5}, {"end_turn", 8, 9}} {
		got := recorder.responses[i]
		if got.StopReason != want.stop || got.Usage.InputTokens != want.in || got.Usage.OutputTokens != want.out {
			t.Errorf("response %d = %+v", i, got)
		}
	}
}
