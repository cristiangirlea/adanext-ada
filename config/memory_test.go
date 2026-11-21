package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/adanext-ada/memory"
	"github.com/cristiangirlea/adanext-ada/provider"
)

type contextRecorder struct{ systems []string }

func (*contextRecorder) Name() string { return "fake" }

func (r *contextRecorder) Complete(ctx context.Context, q provider.Request) (provider.Response, error) {
	r.systems = append(r.systems, q.System)
	return provider.Response{Content: []provider.Block{{Type: "text", Text: "ok"}}}, nil
}

func TestMemoryConfiguration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	load := func(extra string) (Settings, error) {
		t.Helper()
		if err := os.WriteFile(path, []byte(`{"chat_provider":"fake","providers":[{"name":"fake","kind":"openai","model":"fake","base_url":"http://localhost"}]`+extra+"}"), 0600); err != nil {
			t.Fatal(err)
		}
		return Load(path)
	}
	s, err := load("")
	cfgDir, _ := os.UserConfigDir()
	if err != nil || s.DataDir != filepath.Join(cfgDir, "adanext-ada", "data") || s.Owner != "owner" || s.HistoryMessages != 40 || s.SessionIdle != 12*time.Hour {
		t.Fatal(s, err)
	}
	for _, extra := range []string{`,"knowledge":{"project_id":"p"}`, `,"knowledge":{"invalid":true}`} {
		if _, err = load(extra); err == nil {
			t.Fatal(extra)
		}
	}
	for _, enabled := range []bool{false, true} {
		extra := ""
		if enabled {
			extra = `,"knowledge":{"tenant_id":"t","project_id":"p","backend":"b","model":"m","revision":"r"}`
		}
		s, err := load(extra)
		if err != nil {
			t.Fatal(err)
		}
		s.DataDir = t.TempDir()
		app, err := Build(s, os.Getenv)
		if err != nil {
			t.Fatal(err)
		}
		defer app.Store.Close()
		defer app.Tools.Close()
		names := map[string]bool{}
		for _, spec := range app.Tools.Specs() {
			names[spec.Name] = true
		}
		if !names["search_memory"] || names["search_knowledge"] != enabled {
			t.Fatal(names)
		}
		for _, name := range []string{"search_memory", "search_knowledge"} {
			if names[name] {
				if _, err := app.Tools.Run(t.Context(), name, []byte(`{"query":"   "}`)); err == nil {
					t.Fatal(name)
				}
			}
		}
		r := new(contextRecorder)
		app.Provider = r
		app.System = "base"
		if _, _, err = app.Turn(memory.WithUser(t.Context(), "owner"), nil, "one"); err != nil {
			t.Fatal(err)
		}
		if _, err = app.Store.AddFact("owner", "likes tea", "cli"); err != nil {
			t.Fatal(err)
		}
		if _, _, err = app.Turn(memory.WithUser(t.Context(), "owner"), nil, "two"); err != nil {
			t.Fatal(err)
		}
		for _, ctx := range []context.Context{memory.WithUser(t.Context(), "other"), t.Context()} {
			if _, _, err := app.Turn(ctx, nil, "three"); err != nil {
				t.Fatal(err)
			}
			if r.systems[len(r.systems)-1] != "base" {
				t.Fatal(r.systems)
			}
		}
		if r.systems[0] != "base" || !strings.HasPrefix(r.systems[1], "base\n\nFacts about") || !strings.Contains(r.systems[1], "likes tea") {
			t.Fatal(r.systems)
		}
	}
}
