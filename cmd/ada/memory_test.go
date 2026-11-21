package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cristiangirlea/adanext-ada/memory"
	"github.com/cristiangirlea/adanext-ada/provider"
)

func TestChatMemoryCommands(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HRN_HOME", t.TempDir())
	seen := make(chan []string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
			http.Error(w, "decode", 400)
			return
		}
		var contents []string
		for _, m := range q.Messages {
			contents = append(contents, m.Content)
		}
		seen <- contents
		if slices.Contains(contents, "fail") {
			http.Error(w, "failed", 400)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	cfg := filepath.Join(dir, "settings.json")
	dataDir := filepath.Join(dir, "data")
	content, _ := json.Marshal(map[string]any{"data_dir": dataDir, "chat_provider": "fake", "providers": []any{map[string]string{"name": "fake", "kind": "openai", "base_url": server.URL, "model": "fake"}}})
	if err := os.WriteFile(cfg, content, 0600); err != nil {
		t.Fatal(err)
	}
	invoke := func(input string, args ...string) (string, string, error) {
		var out, diag bytes.Buffer
		args = slices.Insert(args, 1, "-config", cfg)
		err := run(t.Context(), args, strings.NewReader(input), &out, &diag)
		return out.String(), diag.String(), err
	}
	_, first, err := invoke("first\nfail\n", "chat")
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := invoke("second\n", "chat")
	if err != nil || len(seen) != 3 {
		t.Fatal(first, second, err)
	}
	<-seen
	<-seen
	if got := <-seen; !slices.Equal(got, []string{"first", "answer", "second"}) {
		t.Fatal(got)
	}
	out, _, err := invoke("", "sessions", "-n", "1")
	if err != nil || !strings.Contains(out, "first") || !strings.Contains(first, strings.Fields(out)[0]) || !strings.Contains(second, strings.Fields(out)[0]) {
		t.Fatal(out, first, second, err)
	}
	out, _, err = invoke("", "search", "first")
	if err != nil || !strings.Contains(out, "session:") {
		t.Fatal(out, err)
	}
	out, _, err = invoke("", "profile", "add", "prefers tea")
	if err != nil {
		t.Fatal(err)
	}
	out, _, err = invoke("", "profile", "list")
	if err != nil || !strings.Contains(out, "prefers tea") {
		t.Fatal(out, err)
	}
	if _, _, err = invoke("", "profile", "remove", strings.Fields(out)[0]); err != nil {
		t.Fatal(err)
	}
	out, _, err = invoke("", "profile", "list")
	if err != nil || strings.Contains(out, "prefers tea") {
		t.Fatal(out, err)
	}
	var updated map[string]any
	if err := json.Unmarshal(content, &updated); err != nil {
		t.Fatal(err)
	}
	updated["history_messages"] = 2
	limited, err := json.Marshal(updated)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, limited, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err = invoke("one\ntwo\nthree\n", "chat", "-new"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 {
		t.Fatal("expected three requests", len(seen))
	}
	<-seen
	<-seen
	if got := <-seen; !slices.Equal(got, []string{"two", "answer", "three"}) {
		t.Fatal("unbounded history", got)
	}
	s, err := memory.NewStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := s.LockSessions()
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.NewSession("owner", "cli", "local", "owner")
	if err != nil {
		t.Fatal(err)
	}
	msg := provider.Message{Role: "user", Content: []provider.Block{{Type: "text", Text: "testcontrol\x1b[0m"}}}
	if err := s.Append(session.ID, "cli", "local", "owner", msg); err != nil {
		t.Fatal(err)
	}
	other, err := s.NewSession("other", "cli", "local", "other")
	defer s.Close()
	defer unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(other.ID, "cli", "local", "other", provider.Message{Role: "user", Content: []provider.Block{{Type: "text", Text: "testcontrol private"}}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := invoke("", "chat"); err == nil || !strings.Contains(err.Error(), "another Ada process") {
		t.Fatal("second chat", err)
	}
	for _, args := range [][]string{{"search", "testcontrol"}, {"sessions"}} {
		out, _, err := invoke("", args...)
		if err != nil || strings.ContainsRune(out, '\x1b') || !strings.Contains(out, "testcontrol") || strings.Contains(out, "private") {
			t.Fatal(out, err)
		}
	}
	unlock()
	if _, _, err = invoke("", "chat", "-resume", other.ID); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatal("resumed another owner")
	}
}

func TestChatReloadsIterationStop(t *testing.T) {
	const stop = "Stopped: the iteration budget (1) was reached."
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role, Content string
				ToolCallID    string `json:"tool_call_id"`
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if len(req.Messages) == 1 && req.Messages[0].Content == "first" {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"lookup","type":"function","function":{"name":"search_memory","arguments":"{\"query\":\"absent\"}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		var roles []string
		for _, msg := range req.Messages {
			roles = append(roles, msg.Role)
		}
		if !slices.Equal(roles, []string{"user", "assistant", "tool", "assistant", "user"}) || req.Messages[0].Content != "first" || req.Messages[2].ToolCallID != "lookup" || req.Messages[2].Content != "" || req.Messages[3].Content != stop || req.Messages[4].Content != "second" {
			t.Errorf("unexpected restored conversation: %+v", req.Messages)
			http.Error(w, "invalid history", 400)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"finished"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	cfg := filepath.Join(t.TempDir(), "settings.json")
	t.Setenv("HRN_HOME", t.TempDir())
	if err := os.WriteFile(cfg, fmt.Appendf(nil, `{"data_dir":%q,"history_messages":2,"budget":{"max_iterations":1},"providers":[{"name":"fake","kind":"openai","base_url":%q}],"chat_provider":"fake"}`, t.TempDir(), server.URL), 0600); err != nil {
		t.Fatal(err)
	}
	for _, turn := range []struct{ input, answer string }{{"first", stop}, {"second", "finished"}} {
		var out, diag bytes.Buffer
		if err := run(t.Context(), []string{"chat", "-config", cfg}, strings.NewReader(turn.input), &out, &diag); err != nil || strings.TrimSpace(out.String()) != turn.answer {
			t.Fatalf("chat = %q, stderr = %q, err = %v", out.String(), diag.String(), err)
		}
	}
}
