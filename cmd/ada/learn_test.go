package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cristiangirlea/adanext-ada/memory"
	"github.com/cristiangirlea/adanext-ada/provider"
)

func TestLearnCommands(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "settings.json")
	m, err := memory.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	unlock, err := m.LockSessions()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	s, err := m.NewSession("owner", "cli", "local", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Append(s.ID, "cli", "local", "owner", provider.Message{Role: "assistant", Content: []provider.Block{{Type: "text", Text: "likes tea"}}}); err != nil {
		t.Fatal(err)
	}
	writeConfig := func(url, idle string) {
		t.Helper()
		if err := os.WriteFile(cfg, fmt.Appendf(nil, `{"data_dir":%q,"session_idle":%q,"chat_provider":"fake","providers":[{"name":"fake","kind":"openai","base_url":%q,"model":"test-model"}]}`, dir, idle, url), 0600); err != nil {
			t.Fatal(err)
		}
	}
	invoke := func(args ...string) (string, error) {
		var out bytes.Buffer
		args = append([]string{"learn", args[0], "-config", cfg}, args[1:]...)
		err := run(t.Context(), args, strings.NewReader(""), &out, io.Discard)
		return strings.TrimSpace(out.String()), err
	}
	writeConfig("https://remote.invalid", "1ns")
	if _, err := invoke("propose", "-session", s.ID); err == nil || !strings.Contains(err.Error(), "allow_metered") {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string
			Tools []any
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model != "test-model" || len(req.Tools) != 0 {
			t.Error(req, err)
		}
		if calls.Add(1) == 1 {
			fmt.Fprint(w, `{"choices":[{"message":{"content":"{}"},"finish_reason":"stop"}]}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"facts\":[{\"text\":\"likes tea\",\"messages\":[0]}]}"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	writeConfig(server.URL, "12h")
	if out, err := invoke("propose"); err != nil || out != "" || calls.Load() != 0 {
		t.Fatal(out, err, calls.Load())
	}
	writeConfig(server.URL, "1ns")
	if _, err := invoke("propose", "-session", ""); err == nil || calls.Load() != 0 {
		t.Fatal("empty session selected a batch", err)
	}
	newer, err := m.NewSession("owner", "cli", "local", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Append(newer.ID, "cli", "local", "owner", provider.Message{Role: "user", Content: []provider.Block{{Type: "text", Text: "newer turn"}}}); err != nil {
		t.Fatal(err)
	}
	out, err := invoke("propose", "-since", "24h")
	if err == nil || !strings.Contains(err.Error(), newer.ID) || !strings.Contains(out, s.ID) || !strings.Contains(out, "outside=true") || !strings.Contains(out, "likes tea") {
		t.Fatal(out, err)
	}
	id := strings.Fields(out)[0]
	if out, err := invoke("propose"); err != nil || out != "" || calls.Load() != 3 {
		t.Fatal(out, err, calls.Load())
	}
	if out, err := invoke("list"); err != nil || !strings.Contains(out, id) {
		t.Fatal(out, err)
	}
	if _, err := invoke("keep", id); err == nil || !strings.Contains(err.Error(), "-outside") {
		t.Fatal(err)
	}
	if _, err := invoke("keep", id, "-outside"); err != nil {
		t.Fatal(err)
	}
	if out, err := invoke("list"); err != nil || out != "" {
		t.Fatal(out, err)
	}
	if out, err := invoke("list", "-all"); err != nil || !strings.Contains(out, "kept") {
		t.Fatal(out, err)
	}
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte("{"), []byte(`{"learning":{"stale_after":"1ns"},`), 1)
	if err := os.WriteFile(cfg, data, 0600); err != nil {
		t.Fatal(err)
	}
	out, err = invoke("confirm", id)
	lines := strings.Split(out, "\n")
	if err != nil || len(lines) != 2 || !strings.Contains(lines[0], "\tstale\t") || !strings.Contains(lines[1], "\tkept\t") {
		t.Fatal(out, err)
	}
	if _, err := invoke("rollback", id); err != nil || len(m.Facts("owner")) != 0 {
		t.Fatal(err)
	}
}
