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
)

func TestRunChatAndVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"answer"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	config := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(config, fmt.Appendf(nil, `{"providers":[{"name":"test","kind":"openai","base_url":%q}],"chat_provider":"test"}`, srv.URL), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run(t.Context(), []string{"chat", "-config", config}, strings.NewReader("first\nsecond\n"), &out, &out); err != nil || out.String() != "answer\nanswer\n" {
		t.Fatalf("chat = %q, %v", out.String(), err)
	}
	out.Reset()
	if err := run(t.Context(), []string{"version"}, strings.NewReader(""), &out, &out); err != nil || !strings.HasPrefix(out.String(), "ada ") {
		t.Fatalf("version = %q, %v", out.String(), err)
	}
	if err := run(t.Context(), []string{"unknown"}, strings.NewReader(""), &out, &out); err == nil {
		t.Fatal("unknown command accepted")
	}
}

func TestRunContinuesAfterTurnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Messages []struct{ Content string } }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var history []string
		for _, message := range req.Messages {
			history = append(history, message.Content)
		}
		switch history[len(history)-1] {
		case "first":
			fmt.Fprint(w, `{"choices":[{"message":{"content":"first answer"},"finish_reason":"stop"}]}`)
		case "failed":
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, "failed turn diagnostic")
		case "third":
			if !slices.Equal(history, []string{"first", "first answer", "third"}) {
				t.Errorf("history = %q", history)
			}
			fmt.Fprint(w, `{"choices":[{"message":{"content":"third answer"},"finish_reason":"stop"}]}`)
		}
	}))
	defer srv.Close()
	config := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(config, fmt.Appendf(nil, `{"providers":[{"name":"test","kind":"openai","base_url":%q}],"chat_provider":"test"}`, srv.URL), 0600); err != nil {
		t.Fatal(err)
	}
	var out, diagnostics bytes.Buffer
	err := run(t.Context(), []string{"chat", "-config", config}, strings.NewReader("first\nfailed\nthird\n"), &out, &diagnostics)
	if err != nil || out.String() != "first answer\nthird answer\n" || !strings.Contains(diagnostics.String(), "failed turn diagnostic") {
		t.Fatalf("chat = %q, stderr = %q, %v", out.String(), diagnostics.String(), err)
	}
}
