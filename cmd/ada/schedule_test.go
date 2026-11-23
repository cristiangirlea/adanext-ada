package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/adanext-ada/config"
	"github.com/cristiangirlea/adanext-ada/schedule"
)

func TestScheduleCLI(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	data, _ := json.Marshal(map[string]string{"data_dir": dir, "chat_provider": "unused"})
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	call := func(args ...string) (string, error) {
		var out bytes.Buffer
		args = append([]string{"schedule", "-config", path}, args...)
		err := run(context.Background(), args, strings.NewReader(""), &out, &out)
		return strings.TrimSpace(out.String()), err
	}
	must := func(args ...string) string {
		t.Helper()
		out, err := call(args...)
		if err != nil {
			t.Fatal(args, out, err)
		}
		return out
	}
	id := must("add", "-name", "report", "-spec", "every 5m", "-prompt", "check", "-to", "one:chat")
	if len(id) != 12 || !strings.Contains(must("list"), "report") {
		t.Fatal(id)
	}
	must("disable", id)
	if !strings.Contains(must("list"), "false") {
		t.Fatal("enabled after disable")
	}
	must("enable", id)
	if _, err := call("add", "-spec", "every 1m", "-prompt", "x", "-script", "--", "x"); err == nil {
		t.Fatal("accepted both kinds")
	}
	if _, err := call("runs", "-n", "0"); err == nil {
		t.Fatal("accepted zero runs")
	}
	must("remove", id)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	id = must("add", "-spec", "every 1m", "-timeout", "30s", "-to", "absent:elsewhere", "-script", "--", exe, "-test.run=^$")
	if out := must("run", id); !strings.Contains(out, "exit 0") {
		t.Fatal(out)
	}
	if out := must("runs", "-n", "1"); !strings.Contains(out, id) || !strings.Contains(out, "false") {
		t.Fatal(out)
	}
	must("remove", id)
	if out := must("list"); out != "" {
		t.Fatal(out)
	}
	if _, err := call("run", id); err == nil {
		t.Fatal("ran removed job")
	}
}

func TestChatProceedsDuringScheduledPrompt(t *testing.T) {
	f := newFixture(t)
	f.manifest()
	f.configure(map[string]any{"name": "echo", "allow": []string{"reader"}})
	settings, err := config.Load(f.config)
	if err != nil {
		t.Fatal(err)
	}
	app, err := buildConversation(settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Tools.Close(); app.Store.Close() })
	unlock, err := app.Store.LockSessions()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
	s := newServeService(settings, app, io.Discard)
	ticks := make(chan time.Time, 1)
	s.Scheduler.Ticks = ticks
	if _, err := s.Scheduler.Store.Add(schedule.Job{Kind: "prompt", Spec: "every 1m", Prompt: "hold"}, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.startServer(func(ctx context.Context) error { return s.Serve(ctx, f.dir, settings.Channels) })
	defer close(f.release)
	f.connect("echo")
	ticks <- time.Now()
	select {
	case <-f.blocking:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduled prompt did not enter the model")
	}
	f.inbound("chat", "reader", "hello", false)
	f.ack("chat")
	d := f.delivery()
	if d.ReplyTo == nil || *d.ReplyTo != "chat" || d.Text != "model answer" {
		t.Fatalf("chat reply while scheduled prompt was blocked: %+v", d)
	}
	f.delivered(d)
}
