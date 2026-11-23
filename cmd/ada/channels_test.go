package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cristiangirlea/adanext-ada/memory"
	p "github.com/cristiangirlea/harness/contracts/pluginproto"
)

// The copied binary bridges a test-controlled fake service to the plugin pipes.
func TestMain(m *testing.M) {
	if strings.HasPrefix(filepath.Base(os.Args[0]), "ada-fake-channel") {
		if len(os.Args) > 1 && os.Args[1] == "tree-child" {
			c, err := net.Dial("tcp", os.Args[2])
			if err != nil {
				os.Exit(6)
			}
			fmt.Fprintln(c, "child")
			time.Sleep(time.Minute)
			c.Close()
			os.Exit(0)
		}
		addr := os.Getenv("ADA_FAKE_SERVICE")
		if addr == "" {
			addr = os.Getenv("ADA_FAKE_SECOND")
		}
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			os.Exit(2)
		}
		if _, ok := os.LookupEnv("LANG"); ok {
			os.Exit(3)
		}
		if _, ok := os.LookupEnv("ADA_PRIVATE_TEST"); ok {
			os.Exit(4)
		}
		fmt.Fprintln(conn, os.Getpid())
		if strings.Contains(filepath.Base(os.Args[0]), "-tree") {
			// Receiving hello means the host has finished assigning the process tree.
			lines := bufio.NewScanner(os.Stdin)
			if !lines.Scan() {
				os.Exit(5)
			}
			fmt.Fprintln(conn, lines.Text())
			cmd := exec.Command(os.Args[0], "tree-child", addr)
			if err := cmd.Start(); err != nil {
				os.Exit(6)
			}
			go io.Copy(os.Stdout, conn)
			cmd.Wait()
			os.Exit(0)
		}
		go func() { io.Copy(os.Stdout, conn); os.Exit(0) }()
		relay(os.Stdin, conn, p.ParseHostLineV2)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func relay(src io.Reader, dst io.Writer, parse func([]byte, string) (any, error)) {
	lines := bufio.NewScanner(src)
	lines.Buffer(make([]byte, 4096), p.MaxLine+2)
	for lines.Scan() {
		frame, err := parse(lines.Bytes(), p.KindChannel)
		if err != nil {
			os.Exit(5)
		}
		if json.NewEncoder(dst).Encode(frame) != nil {
			return
		}
	}
}

type fixture struct {
	name    string
	pid     int
	release chan struct{}

	t           *testing.T
	dir, config string
	listener    net.Listener
	conn        net.Conn
	lines       *bufio.Scanner
	seq         int64
	done        chan error
	stop        context.CancelFunc
	calls       atomic.Int32
	blocking    chan struct{}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{name: "echo", release: make(chan struct{}), t: t, dir: t.TempDir(), blocking: make(chan struct{}, 1)}
	var err error
	f.listener, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.listener.Close() })
	t.Setenv("ADA_FAKE_SERVICE", f.listener.Addr().String())
	// LANG would normally be inherited. Configuring it as the provider key must exclude it.
	t.Setenv("LANG", "test marker")
	t.Setenv("ADA_PRIVATE_TEST", "unrelated")
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Messages []struct{ Content string } }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		f.calls.Add(1)
		last := req.Messages[len(req.Messages)-1].Content
		if last == "hold" {
			f.blocking <- struct{}{}
			select {
			case <-f.release:
			case <-r.Context().Done():
				return
			}
		}
		if last == "block" {
			f.blocking <- struct{}{}
			<-r.Context().Done()
			return
		}
		if last == "use whole budget" {
			fmt.Fprint(w, `{"choices":[{"message":{"content":"partial answer"}}],"usage":{"completion_tokens":200000}}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"model answer"}}]}`)
	}))
	t.Cleanup(model.Close)
	exe := filepath.Join(f.dir, "ada-fake-channel"+filepath.Ext(os.Args[0]))
	src, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	dst, err := os.OpenFile(exe, os.O_CREATE|os.O_WRONLY, 0700)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(dst, src)
	src.Close()
	if e := dst.Close(); err != nil || e != nil {
		t.Fatalf("copy executable: %v %v", err, e)
	}
	f.config = filepath.Join(f.dir, "settings.json")
	settings := map[string]any{"channels": []map[string]any{{"name": "echo", "enabled": true, "path": exe}}, "data_dir": f.dir, "chat_provider": "fake", "providers": []map[string]string{{"kind": "openai", "name": "fake", "base_url": model.URL, "model": "fake", "api_key_env": "LANG"}}}
	b, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.config, b, 0600); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *fixture) manifest() {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, "channel.json"), []byte(`{"name":"echo","label":"Echo","secret_env":"ADA_FAKE_SERVICE","hosts":["chat.example"]}`), 0600); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) start() {
	f.startServer(func(ctx context.Context) error {
		return run(ctx, []string{"serve", "-config", f.config}, nil, io.Discard, io.Discard)
	})
}
func (f *fixture) startServer(serve func(context.Context) error) {
	ctx, stop := context.WithCancel(f.t.Context())
	f.stop = stop
	done := make(chan error, 1)
	f.done = done
	go func() { defer close(done); done <- serve(ctx) }()
	f.t.Cleanup(func() {
		stop()
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			f.t.Error("serve did not exit")
		}
	})
}
func (f *fixture) connect(name string) {
	f.t.Helper()
	f.handshake()
	f.send(p.Ready{V: 1, Seq: 1, Type: p.TypeReady, Protocol: p.V2, Plugin: p.Program{Name: "fake", Version: "1"}, Kind: p.KindChannel, Channel: &p.Channel{Name: name}})
}
func (f *fixture) handshake() *p.Hello {
	f.t.Helper()
	f.listener.(*net.TCPListener).SetDeadline(time.Now().Add(8 * time.Second))
	c, err := f.listener.Accept()
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { c.Close() })
	f.conn = c
	f.lines = bufio.NewScanner(c)
	if !f.lines.Scan() {
		f.t.Fatal("missing fake plugin PID")
	}
	f.pid, err = strconv.Atoi(f.lines.Text())
	if err != nil {
		f.t.Fatal(err)
	}
	f.seq = 1
	h, ok := f.frame().(*p.Hello)
	if !ok || strings.Join(h.Protocols, ",") != "v2" {
		f.t.Fatalf("hello = %+v", h)
	}
	return h
}
func (f *fixture) frame() any {
	f.t.Helper()
	f.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if !f.lines.Scan() {
		f.t.Fatalf("missing host frame: %v", f.lines.Err())
	}
	line, err := p.ParseHostLineV2(f.lines.Bytes(), p.KindChannel)
	if err != nil {
		f.t.Fatal(err)
	}
	return line
}
func (f *fixture) send(v any) {
	f.t.Helper()
	if err := json.NewEncoder(f.conn).Encode(v); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) inbound(id, sender, text string, group bool) {
	f.seq++
	msg := p.Inbound{V: 1, Seq: f.seq, Type: p.TypeInbound, ID: id, Time: time.Now().Format(time.RFC3339), Sender: p.Sender{ID: sender, Name: "Jo"}, Chat: p.Chat{ID: "room"}, Text: text}
	kind := p.ChatPrivate
	if group {
		kind = p.ChatGroup
	}
	msg.Chat.Type = &kind
	f.send(msg)
}
func (f *fixture) ack(id string) {
	f.t.Helper()
	a, ok := f.frame().(*p.Ack)
	if !ok || a.ID != id {
		f.t.Fatalf("ack for %s = %+v", id, a)
	}
	data, err := os.ReadFile(filepath.Join(f.dir, "channels", f.name, "inbox.jsonl"))
	if err != nil {
		f.t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(fmt.Sprintf(`"id":%q`, id))) {
		f.t.Fatal("ack before inbox append")
	}
}
func (f *fixture) delivery() *p.Deliver {
	f.t.Helper()
	frame := f.frame()
	d, ok := frame.(*p.Deliver)
	if !ok || len(d.ID) != 12 {
		f.t.Fatalf("delivery = %+v", frame)
	}
	return d
}
func (f *fixture) delivered(d *p.Deliver) {
	f.seq++
	f.send(p.Delivered{V: 1, Seq: f.seq, Type: p.TypeDelivered, ID: d.ID, MessageID: "sent"})
}
func (f *fixture) cli(cmd string, args ...string) (string, error) {
	var out bytes.Buffer
	err := run(f.t.Context(), append([]string{cmd, "-config", f.config}, args...), strings.NewReader(""), &out, io.Discard)
	return out.String(), err
}
func (f *fixture) kill() {
	f.t.Helper()
	process, err := os.FindProcess(f.pid)
	if err != nil {
		f.t.Fatal(err)
	}
	defer process.Release()
	if err := process.Kill(); err != nil {
		f.t.Fatal(err)
	}
}
func TestChannelConversation(t *testing.T) {
	f := newFixture(t)
	f.manifest()
	f.start()
	f.connect("echo")
	if _, err := f.cli("chat"); err == nil || !strings.Contains(err.Error(), "Ada is serving") {
		t.Fatalf("chat while serving: %v", err)
	}
	if got, err := f.cli("channels"); err != nil || !strings.Contains(got, "found=true") || !strings.Contains(got, "chat.example") {
		t.Fatalf("channels: %q %v", got, err)
	}
	f.inbound("group", "reader", "group", true)
	f.ack("group")
	f.inbound("first", "reader", "hello", false)
	f.ack("first")
	prompt := f.delivery()
	f.delivered(prompt)
	var code string
	if _, err := fmt.Sscanf(prompt.Text, "To talk to Ada, run: ada pair %s on your computer.", &code); err != nil || len(code) != 6 || strings.ContainsAny(code, "0O1I") {
		t.Fatalf("pair prompt = %q", prompt.Text)
	}
	f.inbound("second", "reader", "again", false)
	f.ack("second")
	if _, err := f.cli("pair", "XXXXXXX"); err == nil {
		t.Fatal("accepted wrong code")
	}
	if got, err := f.cli("pair", code); err != nil || !strings.Contains(got, "reader") {
		t.Fatalf("pair: %q %v", got, err)
	}
	if _, err := f.cli("pair", code); err == nil {
		t.Fatal("reused code")
	}
	f.inbound("message", "reader", "paired", false)
	f.ack("message")
	d := f.delivery()
	if d.Text != "model answer" {
		t.Fatalf("answer = %q", d.Text)
	}
	f.kill()
	f.connect("echo")
	replay := f.delivery()
	if replay.ID != d.ID {
		t.Fatal("replay changed delivery ID")
	}
	f.delivered(replay)
	f.inbound("message", "reader", "duplicate", false)
	f.ack("message")
	f.inbound("slow", "reader", "block", false)
	f.ack("slow")
	select {
	case <-f.blocking:
	case <-time.After(3 * time.Second):
		t.Fatal("turn did not start")
	}
	f.inbound("stop", "reader", "/stop", false)
	f.ack("stop")
	stopped := f.delivery()
	if stopped.Text != "Stopped." {
		t.Fatal(stopped.Text)
	}
	wait := int64(180)
	f.seq++
	start := time.Now()
	f.send(p.DeliveryFailed{V: 1, Seq: f.seq, Type: p.TypeDeliveryFailed, ID: stopped.ID, Error: p.Problem{Code: p.CodeRateLimited, Message: "wait"}, RetryAfterMS: &wait})
	retry := f.delivery()
	if retry.ID != stopped.ID || time.Since(start) < time.Duration(wait)*time.Millisecond {
		t.Fatal("invalid rate limit retry")
	}
	f.delivered(retry)
	if f.calls.Load() != 2 {
		t.Fatalf("model calls = %d", f.calls.Load())
	}
	if _, err := f.cli("unpair", "echo", "reader"); err != nil {
		t.Fatal(err)
	}
	f.inbound("after", "reader", "hello", false)
	f.ack("after")
	f.inbound("stranger", "stranger", "hello", false)
	f.ack("stranger")
	if d := f.delivery(); d.ReplyTo == nil || *d.ReplyTo != "stranger" || !strings.Contains(d.Text, "ada pair") {
		t.Fatalf("unexpected reply after unpair: %+v", d)
	}
	if f.calls.Load() != 2 {
		t.Fatal("unpaired sender reached the model")
	}
}

func (f *fixture) configureSecond() *fixture {
	t := f.t
	t.Helper()
	secondDir := filepath.Join(f.dir, "second")
	if err := os.Mkdir(secondDir, 0700); err != nil {
		t.Fatal(err)
	}
	bin := "ada-fake-channel" + filepath.Ext(os.Args[0])
	if err := os.Link(filepath.Join(f.dir, bin), filepath.Join(secondDir, bin)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secondDir, "channel.json"), []byte(`{"name":"second","secret_env":"ADA_FAKE_SECOND"}`), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	t.Setenv("ADA_FAKE_SECOND", listener.Addr().String())
	other := &fixture{t: t, dir: f.dir, name: "second", listener: listener}
	f.configure(map[string]any{"name": "second", "path": filepath.Join(secondDir, bin), "allow": []string{"bob"}}, map[string]any{"name": "echo", "allow": []string{"alice"}, "groups": true})
	return other
}

func TestChannelQueueSurvivesPluginExit(t *testing.T) {
	f := newFixture(t)
	f.manifest()
	other := f.configureSecond()
	f.start()
	f.connect("echo")
	other.connect("second")
	f.inbound("hold", "alice", "hold", true)
	f.ack("hold")
	select {
	case <-f.blocking:
	case <-time.After(3 * time.Second):
		t.Fatal("group turn did not start")
	}
	other.inbound("queued", "bob", "queued", false)
	other.ack("queued")
	f.kill()
	f.connect("echo")
	f.inbound("hold", "alice", "hold", true)
	f.ack("hold")
	if f.calls.Load() != 1 {
		t.Fatal("ran a second turn while the first was blocked")
	}
	close(f.release)
	a := f.delivery()
	b := other.delivery()
	if *a.ReplyTo != "hold" || *b.ReplyTo != "queued" || a.Text != "model answer" || b.Text != a.Text {
		t.Fatal("lost queued answer")
	}
	f.delivered(a)
	other.delivered(b)
	f.inbound("busy", "alice", "block", false)
	f.ack("busy")
	select {
	case <-f.blocking:
	case <-time.After(3 * time.Second):
		t.Fatal("turn did not start")
	}
	other.inbound("stop", "bob", "/stop", false)
	other.ack("stop")
	if d := other.delivery(); d.Text != "Stopped." {
		t.Fatal(d.Text)
	} else {
		other.delivered(d)
	}
	other.inbound("new", "bob", "/new", false)
	other.ack("new")
	other.delivered(other.delivery())
	if got, err := f.cli("sessions"); err != nil || strings.Count(strings.TrimSpace(got), "\n") != 1 {
		t.Fatalf("sessions after /new: %q %v", got, err)
	}
	if f.calls.Load() != 3 {
		t.Fatalf("model calls = %d", f.calls.Load())
	}
}

func TestChannelRefusesMissingManifest(t *testing.T) {
	f := newFixture(t)
	if _, err := f.cli("serve"); err == nil || !strings.Contains(err.Error(), "channel.json") {
		t.Fatalf("serve = %v", err)
	}
}
func TestChannelRefusesWrongHandshake(t *testing.T) {
	f := newFixture(t)
	f.manifest()
	f.start()
	f.connect("wrong")
	select {
	case err := <-f.done:
		if err == nil || !strings.Contains(err.Error(), "echo") {
			t.Fatalf("serve = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("accepted wrong handshake")
	}
}

func (f *fixture) configure(channels ...map[string]any) {
	f.t.Helper()
	data, err := os.ReadFile(f.config)
	if err != nil {
		f.t.Fatal(err)
	}
	var settings map[string]any
	if err = json.Unmarshal(data, &settings); err != nil {
		f.t.Fatal(err)
	}
	for _, ch := range channels {
		if ch["path"] == nil {
			ch["path"] = filepath.Join(f.dir, "ada-fake-channel"+filepath.Ext(os.Args[0]))
		}
		ch["enabled"] = true
	}
	settings["channels"] = channels
	data, err = json.Marshal(settings)
	if err != nil {
		f.t.Fatal(err)
	}
	if err = os.WriteFile(f.config, data, 0600); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) shutdown() {
	f.t.Helper()
	f.stop()
	select {
	case err := <-f.done:
		if err != nil {
			f.t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		f.t.Fatal("serve did not stop within five seconds")
	}
}
func TestChannelRequiresPrivateType(t *testing.T) {
	f := newFixture(t)
	f.manifest()
	f.configure(map[string]any{"name": "echo", "allow": []string{"reader"}})
	f.start()
	f.connect("echo")
	f.seq++
	f.send(p.Inbound{V: 1, Seq: f.seq, Type: p.TypeInbound, ID: "missing", Sender: p.Sender{ID: "reader"}, Chat: p.Chat{ID: "room"}, Time: time.Now().Format(time.RFC3339), Text: "/new"})
	f.ack("missing")
	f.inbound("valid", "reader", "hello", false)
	f.ack("valid")
	if d := f.delivery(); *d.ReplyTo != "valid" {
		t.Fatalf("answered untyped chat: %+v", d)
	}
}
func TestGroupOffersPairingOnce(t *testing.T) {
	f := newFixture(t)
	f.manifest()
	f.configure(map[string]any{"name": "echo", "groups": true, "allow": []string{"paired"}})
	f.start()
	f.connect("echo")
	f.inbound("first", "reader", "hi", true)
	f.ack("first")
	d := f.delivery()
	if !strings.Contains(d.Text, "ada pair") || *d.ReplyTo != "first" {
		t.Fatal(d)
	}
	f.delivered(d)
	f.inbound("again", "reader", "again", true)
	f.ack("again")
	f.inbound("barrier", "paired", "hello", true)
	f.ack("barrier")
	if d = f.delivery(); *d.ReplyTo != "barrier" || f.calls.Load() != 1 {
		t.Fatal(d, f.calls.Load())
	}
}
func TestAllowRevokedOnNextServe(t *testing.T) {
	f := newFixture(t)
	f.manifest()
	f.configure(map[string]any{"name": "echo", "allow": []string{"allowed"}})
	f.start()
	f.connect("echo")
	f.inbound("pre", "allowed", "hello", false)
	f.ack("pre")
	f.delivered(f.delivery())
	f.inbound("pair", "paired", "hello", false)
	f.ack("pair")
	prompt := f.delivery()
	f.delivered(prompt)
	var code string
	if _, err := fmt.Sscanf(prompt.Text, "To talk to Ada, run: ada pair %s", &code); err != nil {
		t.Fatal(err)
	}
	if _, err := f.cli("pair", code); err != nil {
		t.Fatal(err)
	}
	// An ignored group message's ack proves the delivered frame was read before shutdown.
	f.inbound("sync", "stranger", "ignored", true)
	f.ack("sync")
	f.shutdown()
	// A manual link that is temporarily also listed must remain a manual link.
	f.configure(map[string]any{"name": "echo", "allow": []string{"paired", "allowed"}})
	f.start()
	f.connect("echo")
	f.shutdown()
	f.configure(map[string]any{"name": "echo"})
	f.start()
	f.connect("echo")
	f.inbound("revoked", "allowed", "hello", false)
	f.ack("revoked")
	if d := f.delivery(); !strings.Contains(d.Text, "ada pair") {
		t.Fatal(d)
	} else {
		f.delivered(d)
	}
	f.inbound("kept", "paired", "hello", false)
	f.ack("kept")
	if d := f.delivery(); d.Text != "model answer" {
		t.Fatal(d)
	}
}
func TestRestartAnswersAcknowledgedTurnsOnceInOrder(t *testing.T) {
	f := newFixture(t)
	f.manifest()
	f.configure(map[string]any{"name": "echo", "allow": []string{"reader"}})
	f.start()
	f.connect("echo")
	f.inbound("one", "reader", "hold", false)
	f.ack("one")
	select {
	case <-f.blocking:
	case <-time.After(3 * time.Second):
		t.Fatal("turn did not start")
	}
	f.inbound("two", "reader", "second", false)
	f.ack("two")
	f.shutdown()
	close(f.release)
	f.start()
	f.connect("echo")
	for _, id := range []string{"one", "two"} {
		d := f.delivery()
		if *d.ReplyTo != id || d.Text != "model answer" {
			t.Fatal(d)
		}
		f.delivered(d)
	}
	f.inbound("two", "reader", "duplicate", false)
	f.ack("two")
	f.shutdown()
	f.start()
	f.connect("echo")
	f.inbound("idle", "reader", "/stop", false)
	f.ack("idle")
	if d := f.delivery(); *d.ReplyTo != "idle" || d.Text != "Nothing is running." || f.calls.Load() != 3 {
		t.Fatal(d, f.calls.Load())
	}
}
func (f *fixture) onlySession() memory.Session {
	f.t.Helper()
	store, err := memory.NewStore(f.dir)
	if err != nil {
		f.t.Fatal(err)
	}
	defer store.Close()
	sessions, err := store.List("owner", 0)
	if err != nil || len(sessions) != 1 {
		f.t.Fatalf("sessions = %+v, err = %v", sessions, err)
	}
	return sessions[0]
}

func TestRestartAfterSessionCommitBeforeAnswer(t *testing.T) {
	for _, tc := range []struct {
		text, answer string
		calls        int32
	}{
		{"hello", "model answer", 1},
		{"use whole budget", "Stopped: the token budget (200000) was reached.", 1},
		{"/new", "Started a new session.", 0},
	} {
		t.Run(strings.TrimPrefix(tc.text, "/"), func(t *testing.T) {
			f := newFixture(t)
			f.manifest()
			f.configure(map[string]any{"name": "echo", "allow": []string{"reader"}})
			f.start()
			f.connect("echo")
			f.inbound("message", "reader", tc.text, false)
			f.ack("message")
			if got := f.delivery().Text; got != tc.answer {
				t.Fatalf("initial answer = %q, want %q", got, tc.answer)
			}
			f.shutdown()
			session := f.onlySession()

			// Restore the journal boundary just before answer(), leaving the committed session on disk.
			dir := filepath.Join(f.dir, "channels", "echo")
			inbox := filepath.Join(dir, "inbox.jsonl")
			data, err := os.ReadFile(inbox)
			if err != nil {
				t.Fatal(err)
			}
			end := bytes.IndexByte(data, '\n')
			if end < 0 {
				t.Fatal("missing inbound")
			}
			if err := os.Truncate(inbox, int64(end+1)); err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(filepath.Join(dir, "outbox.jsonl"), 0); err != nil {
				t.Fatal(err)
			}
			f.start()
			f.connect("echo")
			d := f.delivery()
			if d.Text != tc.answer || d.ReplyTo == nil || *d.ReplyTo != "message" {
				t.Fatal(d)
			}
			f.delivered(d)
			// The next ack ensures the plugin's delivered frame has reached the journal.
			f.inbound("message", "reader", tc.text, false)
			f.ack("message")
			f.shutdown()
			if got := f.calls.Load(); got != tc.calls {
				t.Errorf("provider calls = %d, want %d", got, tc.calls)
			}
			if got := f.onlySession(); got.ID != session.ID || !got.Updated.Equal(session.Updated) {
				t.Fatalf("replay changed session: %+v", got)
			}
			data, err = os.ReadFile(filepath.Join(dir, "outbox.jsonl"))
			if err != nil || bytes.Count(data, []byte(`"completes":"message"`)) != 1 {
				t.Fatalf("expected one recorded answer: %s, %v", data, err)
			}
		})
	}
}

func TestRestartOrdersTurnsBeforePluginsConnect(t *testing.T) {
	f := newFixture(t)
	f.manifest()
	other := f.configureSecond()
	// Include equal timestamps and the same inbound ID on different channels.
	seed := func(ch, id, sender, text, stamp string) {
		t.Helper()
		dir := filepath.Join(f.dir, "channels", ch)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(filepath.Join(dir, "inbox.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		kind := p.ChatPrivate
		err = json.NewEncoder(file).Encode(p.Inbound{V: 1, Seq: 2, Type: p.TypeInbound, ID: id, Time: stamp, Sender: p.Sender{ID: sender}, Chat: p.Chat{ID: "room", Type: &kind}, Text: text})
		if err != nil {
			t.Fatal(err)
		}
	}
	seed("echo", "x", "alice", "first", "2026-01-01T02:00:00+02:00")
	seed("second", "x", "bob", "second", "2026-01-01T00:00:01Z")
	seed("echo", "z", "alice", "third", "2026-01-01T00:00:02Z")
	seed("echo", "a", "alice", "fourth", "2026-01-01T00:00:02Z")
	seed("second", "b", "bob", "fifth", "2026-01-01T00:00:02Z")
	f.start()
	other.connect("second")
	other.delivered(other.delivery())
	other.delivered(other.delivery())
	f.connect("echo")
	for range 3 {
		f.delivered(f.delivery())
	}
	f.shutdown()
	session := f.onlySession()
	store, err := memory.NewStore(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	history, err := store.History(session.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	var turns []string
	for _, msg := range history {
		if msg.Role == "user" {
			turns = append(turns, msg.Content[0].Text)
		}
	}
	if !slices.Equal(turns, []string{"first", "second", "third", "fourth", "fifth"}) {
		t.Fatalf("turn order = %v", turns)
	}
}

func TestRestartSkipsUnpairedSender(t *testing.T) {
	f := newFixture(t)
	f.manifest()
	f.configure(map[string]any{"name": "echo", "allow": []string{"reader", "owner"}})
	f.start()
	f.connect("echo")
	f.inbound("unfinished", "reader", "block", false)
	f.ack("unfinished")
	select {
	case <-f.blocking:
	case <-time.After(3 * time.Second):
		t.Fatal("turn did not start")
	}
	f.shutdown()
	if _, err := f.cli("unpair", "echo", "reader"); err != nil {
		t.Fatal(err)
	}
	f.configure(map[string]any{"name": "echo", "allow": []string{"owner"}})
	f.start()
	f.connect("echo")
	f.inbound("barrier", "owner", "/stop", false)
	f.ack("barrier")
	if d := f.delivery(); d.Text != "Nothing is running." {
		t.Fatal(d)
	}
	f.shutdown()
	dir := filepath.Join(f.dir, "channels", "echo")
	inbox, err := os.ReadFile(filepath.Join(dir, "inbox.jsonl"))
	if err != nil || !bytes.Contains(inbox, []byte(`"processed":"unfinished"`)) {
		t.Fatalf("missing processed marker: %s, %v", inbox, err)
	}
	outbox, err := os.ReadFile(filepath.Join(dir, "outbox.jsonl"))
	if err != nil || bytes.Contains(outbox, []byte("unfinished")) || f.calls.Load() != 1 {
		t.Fatalf("unexpected replay: calls=%d, outbox=%s, err=%v", f.calls.Load(), outbox, err)
	}
}

func TestTurnStorageErrorLeavesOtherChannelServing(t *testing.T) {
	f := newFixture(t)
	f.manifest()
	other := f.configureSecond()
	lines := make(logLines, 16)
	f.startServer(func(ctx context.Context) error {
		return run(ctx, []string{"serve", "-config", f.config}, nil, io.Discard, lines)
	})
	f.connect("echo")
	other.connect("second")
	f.inbound("bad", "alice", "hold", false)
	f.ack("bad")
	select {
	case <-f.blocking:
	case <-time.After(3 * time.Second):
		t.Fatal("turn did not start")
	}
	// Fail the append after the provider returns, then let the other channel use the session.
	log := filepath.Join(f.dir, "sessions", f.onlySession().ID, "messages.jsonl")
	if err := os.Rename(log, log+".saved"); err != nil {
		t.Fatal(err)
	}
	close(f.release)
	lines.wait(t, "echo turn: open message log")
	if err := os.Rename(log+".saved", log); err != nil {
		t.Fatal(err)
	}
	other.inbound("good", "bob", "hello", false)
	other.ack("good")
	if d := other.delivery(); d.Text != "model answer" || *d.ReplyTo != "good" {
		t.Fatal(d)
	}
	// The failed channel itself is stopped: Ada closes its connection.
	f.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for f.lines.Scan() {
	}
	// EOF or a reset both mean closed; only a read timeout means it is still connected.
	var netErr net.Error
	if err := f.lines.Err(); errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatalf("echo channel still connected after its turn failed: %v", err)
	}
}

func TestUnknownOutcomesAndRedactedStatus(t *testing.T) {
	f := newFixture(t)
	f.manifest()
	f.configure(map[string]any{"name": "echo", "allow": []string{"reader"}})
	var log bytes.Buffer
	f.startServer(func(ctx context.Context) error {
		return run(ctx, []string{"serve", "-config", f.config}, nil, io.Discard, &log)
	})
	f.connect("echo")
	f.seq++
	f.send(p.Status{V: 1, Seq: f.seq, Type: p.TypeStatus, State: "connected", Text: "using " + f.listener.Addr().String()})
	f.delivered(&p.Deliver{ID: "unrelated"})
	f.seq++
	f.send(p.DeliveryFailed{V: 1, Seq: f.seq, Type: p.TypeDeliveryFailed, ID: "unrelated", Error: p.Problem{Code: p.CodeRateLimited, Message: "busy"}})
	f.inbound("idle", "reader", "/stop", false)
	f.ack("idle")
	if d := f.delivery(); d.Text != "Nothing is running." {
		t.Error(d.Text)
	}
	f.shutdown()
	if !strings.Contains(log.String(), "using [redacted]") || strings.Contains(log.String(), f.listener.Addr().String()) {
		t.Fatal("plugin status was not redacted")
	}
}
func TestHelloUsesAdaVersionAndFilteredEnvironment(t *testing.T) {
	f := newFixture(t)
	f.manifest()
	f.start()
	// The fake checks the two excluded variables before returning its PID.
	h := f.handshake()
	var out bytes.Buffer
	if err := run(t.Context(), []string{"version"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := "ada " + h.Host.Version; got != strings.TrimSpace(out.String()) {
		t.Errorf("hello version %q, CLI %q", got, out.String())
	}
}
func TestServeCancellationKillsPluginTree(t *testing.T) {
	f := newFixture(t)
	f.manifest()
	normal := filepath.Join(f.dir, "ada-fake-channel"+filepath.Ext(os.Args[0]))
	tree := filepath.Join(f.dir, "ada-fake-channel-tree"+filepath.Ext(os.Args[0]))
	if err := os.Link(normal, tree); err != nil {
		t.Fatal(err)
	}
	f.configure(map[string]any{"name": "echo", "path": tree})
	f.start()
	f.connect("echo")
	child, err := f.listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	child.SetReadDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(child)
	if line, err := r.ReadString('\n'); err != nil || line != "child\n" {
		t.Fatal(line, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	child.SetReadDeadline(deadline)
	f.shutdown()
	if _, err := r.ReadByte(); err == nil || time.Now().After(deadline) {
		t.Fatalf("child still connected: %v", err)
	}
}

func TestFatalChannelLeavesOtherChannelServing(t *testing.T) {
	for _, fault := range []string{"missing-manifest", "wrong-manifest", "wrong-name", "wrong-kind", "wrong-protocol"} {
		t.Run(fault, func(t *testing.T) {
			f := newFixture(t)
			f.manifest()
			badDir := t.TempDir()
			bin := "ada-fake-channel" + filepath.Ext(os.Args[0])
			if err := os.Link(filepath.Join(f.dir, bin), filepath.Join(badDir, bin)); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			t.Setenv("ADA_FAKE_SECOND", listener.Addr().String())
			bad := &fixture{t: t, name: "bad", listener: listener}
			manifest := "bad"
			if fault == "wrong-manifest" {
				manifest = "other"
			}
			if fault != "missing-manifest" {
				if err := os.WriteFile(filepath.Join(badDir, "channel.json"), []byte(fmt.Sprintf(`{"name":%q,"secret_env":"ADA_FAKE_SECOND"}`, manifest)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			f.configure(map[string]any{"name": "bad", "path": filepath.Join(badDir, bin)}, map[string]any{"name": "echo", "allow": []string{"reader"}})
			f.start()
			if !strings.Contains(fault, "manifest") {
				bad.handshake()
				ready := p.Ready{V: 1, Seq: 1, Type: p.TypeReady, Protocol: p.V2, Kind: p.KindChannel, Plugin: p.Program{Name: "bad", Version: "1"}, Channel: &p.Channel{Name: "bad"}}
				switch fault {
				case "wrong-name":
					ready.Channel.Name = "other"
				case "wrong-kind":
					ready.Kind = "tool"
				case "wrong-protocol":
					ready.Protocol = "v1"
				}
				bad.send(ready)
				bad.conn.SetReadDeadline(time.Now().Add(4 * time.Second))
				var timeout net.Error
				if bad.lines.Scan() || errors.As(bad.lines.Err(), &timeout) && timeout.Timeout() {
					t.Fatal("invalid channel remained connected", bad.lines.Err())
				}
			}
			f.connect("echo")
			f.inbound("good", "reader", "hello", false)
			f.ack("good")
			if d := f.delivery(); d.Text != "model answer" {
				t.Fatal(d)
			}
			f.kill()
			f.connect("unexpected")
			select {
			case err := <-f.done:
				if err == nil || !strings.Contains(err.Error(), "channel bad:") || !strings.Contains(err.Error(), "channel echo:") {
					t.Fatalf("missing fatal channel diagnostics: %v", err)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("serve continued after both channels became fatal")
			}
		})
	}
}

type logLines chan string

func (lines logLines) Write(data []byte) (int, error) {
	lines <- string(data)
	return len(data), nil
}
func (lines logLines) wait(t *testing.T, part string) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case line := <-lines:
			if strings.Contains(line, part) {
				return
			}
		case <-timeout:
			t.Fatalf("missing status %q", part)
		}
	}
}
func TestStartFailureAfterDisconnectRetries(t *testing.T) {
	f := newFixture(t)
	f.manifest()
	lines := make(logLines, 16)
	f.startServer(func(ctx context.Context) error {
		return run(ctx, []string{"serve", "-config", f.config}, nil, io.Discard, lines)
	})
	f.connect("echo")
	lines.wait(t, "ready")
	f.kill()
	lines.wait(t, "restarting in 1s")
	path := filepath.Join(f.dir, "ada-fake-channel"+filepath.Ext(os.Args[0]))
	if err := os.Rename(path, path+".offline"); err != nil {
		t.Fatal(err)
	}
	lines.wait(t, "restarting in 2s")
	if err := os.Rename(path+".offline", path); err != nil {
		t.Fatal(err)
	}
	f.connect("echo")
	lines.wait(t, "ready")
}

func TestServeSignalContext(t *testing.T) {
	var sigs []os.Signal
	var input chan<- os.Signal
	ctx, stop := serveContext(t.Context(), func(c chan<- os.Signal, s ...os.Signal) { input = c; sigs = s })
	defer stop()
	if !slices.Contains(sigs, os.Signal(syscall.SIGTERM)) || !slices.Contains(sigs, os.Interrupt) {
		t.Fatal("missing serve signal", sigs)
	}
	input <- syscall.SIGTERM
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("SIGTERM did not cancel serve")
	}
}

func TestLiveMessageReusingStoredInboundIDReachesModel(t *testing.T) {
	f := newFixture(t)
	f.manifest()
	f.configure(map[string]any{"name": "echo", "allow": []string{"reader"}})
	f.start()
	f.connect("echo")
	f.inbound("message", "reader", "hello", false)
	f.ack("message")
	f.delivered(f.delivery())
	f.shutdown()

	// Forget the id in the journals, as when it falls out of the duplicate window.
	dir := filepath.Join(f.dir, "channels", "echo")
	for _, name := range []string{"inbox.jsonl", "outbox.jsonl"} {
		if err := os.Truncate(filepath.Join(dir, name), 0); err != nil {
			t.Fatal(err)
		}
	}
	f.start()
	f.connect("echo")
	f.inbound("message", "reader", "hello again", false)
	f.ack("message")
	if d := f.delivery(); d.Text != "model answer" {
		t.Fatal(d)
	}
	f.shutdown()
	if got := f.calls.Load(); got != 2 {
		t.Fatalf("provider calls = %d, want 2: a live message must never get a stored answer", got)
	}
}
