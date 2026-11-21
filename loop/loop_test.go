package loop

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/adanext-ada/provider"
	"github.com/cristiangirlea/adanext-ada/tool"
)

type loopProvider func(context.Context, provider.Request) (provider.Response, error)

func (loopProvider) Name() string { return "fake" }
func (p loopProvider) Complete(ctx context.Context, r provider.Request) (provider.Response, error) {
	return p(ctx, r)
}

func TestTurnBudgets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget Budget
		calls  int
	}{
		{"iteration", Budget{MaxIterations: 2}, 2},
		{"token", Budget{MaxTokens: 12}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			p := loopProvider(func(_ context.Context, r provider.Request) (provider.Response, error) {
				calls++
				if r.MaxTokens != 4096 {
					t.Errorf("MaxTokens = %d", r.MaxTokens)
				}
				return provider.Response{Content: []provider.Block{{Type: "tool_use", ID: "call", Name: "unknown"}}, Usage: provider.Usage{InputTokens: 3, OutputTokens: 3}}, nil
			})
			l := Loop{Provider: p, Budget: tc.budget}
			answer, _, err := l.Turn(t.Context(), nil, "question")
			if err != nil || !strings.Contains(answer, tc.name+" budget") || calls != tc.calls {
				t.Fatalf("Turn = %q, %v, calls = %d", answer, err, calls)
			}
		})
	}
}

func TestTurnToolErrors(t *testing.T) {
	calls := 0
	p := loopProvider(func(_ context.Context, r provider.Request) (provider.Response, error) {
		calls++
		if calls == 1 {
			return provider.Response{Content: []provider.Block{
				{Type: "tool_use", ID: "one", Name: "unknown"},
				{Type: "tool_use", ID: "two", Name: "also_unknown"},
			}}, nil
		}
		if len(r.Messages) != 4 || r.Messages[3].Role != "user" || len(r.Messages[3].Content) != 2 {
			t.Fatalf("messages = %+v", r.Messages)
		}
		for i, id := range []string{"one", "two"} {
			got := r.Messages[3].Content[i]
			if got.Type != "tool_result" || got.ToolUseID != id || !got.IsError || !strings.Contains(got.Text, "unknown tool") {
				t.Errorf("bad tool result: %+v", got)
			}
		}
		return provider.Response{Content: []provider.Block{{Type: "text", Text: "done"}}}, nil
	})
	history := make([]provider.Message, 2, 20)
	history[0].Role = "user"
	history[1].Role = "assistant"
	snapshot := append([]provider.Message(nil), history[:cap(history)]...)
	l := Loop{Provider: p}
	answer, got, err := l.Turn(t.Context(), history[:1], "test")
	if err != nil || answer != "done" || len(got) != 5 || !reflect.DeepEqual(history[:cap(history)], snapshot) {
		t.Fatalf("Turn = %q, %v, %v; history changed = %v", answer, got, err, !reflect.DeepEqual(history[:cap(history)], snapshot))
	}
}

func TestTurnTimeoutAndCancellation(t *testing.T) {
	p := loopProvider(func(ctx context.Context, _ provider.Request) (provider.Response, error) {
		<-ctx.Done()
		return provider.Response{}, ctx.Err()
	})
	l := Loop{Provider: p, Budget: Budget{Timeout: time.Millisecond}}
	answer, _, err := l.Turn(t.Context(), nil, "question")
	if err != nil || !strings.Contains(answer, "timeout budget") {
		t.Fatalf("timeout = %q, %v", answer, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := l.Turn(ctx, nil, "question"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

type loopTool func(context.Context) (string, error)

func (loopTool) Spec() provider.ToolSpec { return provider.ToolSpec{Name: "fake_tool"} }
func (loopTool) Effect() tool.Effect     { return tool.Read }
func (f loopTool) Run(ctx context.Context, _ json.RawMessage) (string, error) {
	return f(ctx)
}

func TestTurnTokenBudgetSkipsTools(t *testing.T) {
	for _, usage := range []provider.Usage{{}, {InputTokens: 8}, {OutputTokens: 8}} {
		registry := new(tool.Registry)
		if err := registry.Add(loopTool(func(context.Context) (string, error) {
			t.Error("tool ran after the token budget was reached")
			return "", nil
		})); err != nil {
			t.Fatal(err)
		}
		calls := 0
		p := loopProvider(func(_ context.Context, _ provider.Request) (provider.Response, error) {
			calls++
			return provider.Response{Usage: usage, Content: []provider.Block{
				{Type: "text", Text: "\u00e9\u00e9"},
				{Type: "tool_use", Name: "fake_tool", ID: "new", Input: json.RawMessage(`{"x":0}`)},
			}}, nil
		})
		history := []provider.Message{
			{Role: "assistant", Content: []provider.Block{{Type: "tool_use", ID: "old", Input: json.RawMessage(`{"y":0}`)}}},
			{Role: "user", Content: []provider.Block{{Type: "tool_result", ToolUseID: "old", Text: "result"}}},
		}
		l := Loop{Provider: p, Tools: registry, System: "system", Budget: Budget{MaxTokens: 8, MaxIterations: 2}}
		answer, next, err := l.Turn(t.Context(), history, "ok")
		if err != nil || calls != 1 || !strings.Contains(answer, "token budget") {
			t.Fatalf("usage %+v: Turn = %q, %v, calls = %d", usage, answer, err, calls)
		}
		result := next[len(next)-1].Content[0]
		if !result.IsError || result.ToolUseID != "new" || result.Text != answer {
			t.Errorf("tool result = %+v", result)
		}
	}
}

func TestTurnCancellationDuringTool(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	registry := new(tool.Registry)
	if err := registry.Add(loopTool(func(ctx context.Context) (string, error) {
		cancel()
		<-ctx.Done()
		return "", ctx.Err()
	})); err != nil {
		t.Fatal(err)
	}
	l := Loop{Tools: registry, Provider: loopProvider(func(_ context.Context, _ provider.Request) (provider.Response, error) {
		return provider.Response{Content: []provider.Block{{Type: "tool_use", Name: "fake_tool", ID: "one"}}}, nil
	})}
	if _, _, err := l.Turn(ctx, nil, "question"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Turn = %v", err)
	}
}

func TestTurnKeepsPartialReportedUsage(t *testing.T) {
	for _, usage := range []provider.Usage{{InputTokens: 1}, {OutputTokens: 1}} {
		want := strings.Repeat("answer ", 8)
		l := Loop{Budget: Budget{MaxTokens: 2}, Provider: loopProvider(func(_ context.Context, _ provider.Request) (provider.Response, error) {
			return provider.Response{Usage: usage, Content: []provider.Block{{Type: "text", Text: want}}}, nil
		})}
		if answer, _, err := l.Turn(t.Context(), nil, "question"); err != nil || answer != want {
			t.Errorf("usage %+v: Turn = %q, %v", usage, answer, err)
		}
	}
}
