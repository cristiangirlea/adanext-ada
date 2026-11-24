package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristiangirlea/adanext-ada/approval"
	"github.com/cristiangirlea/adanext-ada/channel"
	"github.com/cristiangirlea/adanext-ada/learn"
	"github.com/cristiangirlea/adanext-ada/loop"
	"github.com/cristiangirlea/adanext-ada/memory"
	"github.com/cristiangirlea/adanext-ada/provider"
	"github.com/cristiangirlea/adanext-ada/tool"
	p "github.com/cristiangirlea/harness/contracts/pluginproto"
)

type writeTool struct{ ran atomic.Bool }

func (w *writeTool) Spec() provider.ToolSpec { return provider.ToolSpec{Name: "write"} }
func (w *writeTool) Effect() tool.Effect     { return tool.Write }
func (w *writeTool) Run(context.Context, json.RawMessage) (string, error) {
	w.ran.Store(true)
	return "written", nil
}

type requestWrite struct{}

func (requestWrite) Name() string { return "test" }
func (requestWrite) Complete(ctx context.Context, r provider.Request) (provider.Response, error) {
	if user, _ := memory.UserFrom(ctx); user != "owner" {
		return provider.Response{}, fmt.Errorf("missing user identity")
	}
	last := r.Messages[len(r.Messages)-1].Content[0]
	block := provider.Block{Type: "tool_use", Name: "write", ID: "call", Input: json.RawMessage(`{"item":1}`)}
	if last.Type == "tool_result" {
		block = provider.Block{Type: "text", Text: last.Text}
	}
	return provider.Response{Content: []provider.Block{block}}, nil
}
func TestChannelApprovalButtons(t *testing.T) {
	for _, answer := range []string{"approve", "deny", "expire"} {
		t.Run(answer, func(t *testing.T) {
			f := newFixture(t)
			f.manifest()
			store, err := memory.NewStore(f.dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			unlock, err := store.LockSessions()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(unlock)
			timeout := 5 * time.Second
			if answer == "expire" {
				timeout = time.Second
			}
			registry := &tool.Registry{Recorder: approval.Log(f.dir), ApprovalTimeout: timeout}
			write := new(writeTool)
			if err := registry.Add(write); err != nil {
				t.Fatal(err)
			}
			s := channel.Service{WithoutEnv: []string{"LANG"}, Store: store, Loop: &loop.Loop{Provider: requestWrite{}, Tools: registry}, Owner: "owner", Idle: time.Hour, History: 40}
			var learningActions atomic.Int32
			s.Decision = func(string, learn.ButtonPress) (bool, error) { learningActions.Add(1); return false, nil }
			f.startServer(func(ctx context.Context) error {
				return s.Serve(ctx, f.dir, []channel.Settings{{Name: "echo", Enabled: true, Path: filepath.Join(f.dir, "ada-fake-channel"+filepath.Ext(os.Args[0])), Allow: []string{"alice", "bob"}}})
			})
			f.connect("echo")
			f.inbound("request", "alice", "write", false)
			f.ack("request")
			prompt := f.delivery()
			f.delivered(prompt)
			if !strings.Contains(prompt.Text, `"item":1`) || len(prompt.Buttons) != 2 || prompt.Buttons[0] != (p.Button{ID: "approve", Label: "Approve"}) || prompt.Buttons[1] != (p.Button{ID: "deny", Label: "Deny"}) {
				t.Fatalf("prompt = %+v", prompt)
			}
			press := f.press
			press("alice", "room", prompt.ID, "keep")
			press("bob", "room", prompt.ID, "approve")
			press("alice", "other", prompt.ID, "approve")
			press("alice", "room", "unrelated", "approve")
			f.inbound("typed", "alice", "approve", false)
			f.ack("typed")
			if write.ran.Load() {
				t.Fatal("unbound decision ran write")
			}
			if answer != "expire" {
				press("alice", "room", prompt.ID, answer)
			}
			if got := learningActions.Load(); got != 1 {
				t.Fatalf("decision hook saw %d actions, want only unrelated delivery", got)
			}
			d := f.delivery()
			f.delivered(d)
			if (answer == "approve") != write.ran.Load() || (d.Text == "written") != write.ran.Load() {
				t.Fatalf("write=%v answer=%q", write.ran.Load(), d.Text)
			}
			if got, err := f.cli("approvals"); err != nil || !strings.Contains(got, "\techo\t") || answer == "expire" && !strings.Contains(got, "expired") {
				t.Fatalf("approval log = %q %v", got, err)
			}
		})
	}
}
