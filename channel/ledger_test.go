package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cristiangirlea/adanext-ada/approval"
	p "github.com/cristiangirlea/harness/contracts/pluginproto"
)

func TestModelAnswerFitsProtocol(t *testing.T) {
	l, err := openLedger(t.TempDir(), "echo")
	if err != nil {
		t.Fatal(err)
	}
	defer l.close()
	h := Host{ledger: l}
	text := strings.Repeat("a", p.MaxEventText-1) + "\u754c\x1b\n"
	if err := h.answer(&p.Inbound{ID: "id", Chat: p.Chat{ID: "room"}}, text); err != nil {
		t.Fatal(err)
	}
	frames, _ := l.ready(map[string]bool{})
	if len(frames) != 2 || !utf8.ValidString(frames[0].Text) || frames[0].Text+frames[1].Text != strings.ReplaceAll(text, "\x1b", string(utf8.RuneError)) {
		t.Fatal("answer was lost or split inside a rune")
	}
	data, err := os.ReadFile(l.outbox.Name())
	if err != nil {
		t.Fatal(err)
	}
	var record outRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("expected one outbox record: %v", err)
	}
	if record.Completes != "id" || record.Deliver == nil || record.Deliver.ID != frames[0].ID || len(record.Continuation) != 1 || record.Continuation[0].ID != frames[1].ID {
		t.Fatal("outbox record did not commit both parts with inbound completion")
	}
}

func TestLedgerReopensPendingDelivery(t *testing.T) {
	dir := t.TempDir()
	open := func() *ledger {
		t.Helper()
		l, err := openLedger(dir, "echo")
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	l := open()
	in := p.Inbound{V: 1, Seq: 2, Type: p.TypeInbound, ID: "message", Time: time.Now().Format(time.RFC3339), Sender: p.Sender{ID: "reader"}, Chat: p.Chat{ID: "chat"}, Text: "hello"}
	if fresh, err := l.accept(&in); err != nil || !fresh {
		t.Fatal(fresh, err)
	}
	d := p.Deliver{Type: p.TypeDeliver, ID: "pending", ChatID: "chat", Text: "reply"}
	if err := l.record(outRecord{Deliver: &d}); err != nil {
		t.Fatal(err)
	}
	l.close()
	for _, name := range []string{"inbox", "outbox"} {
		f, err := os.OpenFile(filepath.Join(dir, "channels", "echo", name+".jsonl"), os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.WriteString("{"); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	l = open()
	for _, f := range []*os.File{l.inbox, l.outbox} {
		data, err := os.ReadFile(f.Name())
		if err != nil || bytes.HasSuffix(data, []byte("{")) {
			t.Fatalf("torn suffix in %s: %v", f.Name(), err)
		}
	}
	if fresh, err := l.accept(&in); err != nil || fresh {
		t.Fatal(fresh, err)
	}
	for i := 0; i < 3; i++ {
		ready, _ := l.ready(map[string]bool{})
		if len(ready) != 1 || ready[0].ID != d.ID {
			t.Fatalf("attempt %d: %+v", i+1, ready)
		}
		failure := p.DeliveryFailed{V: 1, Seq: 3, Type: p.TypeDeliveryFailed, ID: d.ID, Error: p.Problem{Code: p.CodeRateLimited, Message: "wait"}}
		if i != 1 {
			delay := int64(25)
			failure.RetryAfterMS = &delay
		}
		if err := l.record(outRecord{Failed: &failure}); err != nil {
			t.Fatal(err)
		}
		var due time.Time
		if item := l.pending[d.ID]; item != nil {
			due = item.due
		}
		l.close()
		l = open()
		if i < 2 {
			item := l.pending[d.ID]
			if item == nil || !item.due.Equal(due) || item.failures != i+1 {
				t.Fatalf("compaction lost retry state after attempt %d", i+1)
			}
			time.Sleep(time.Until(due))
		}
	}
	defer l.close()
	if ready, _ := l.ready(map[string]bool{}); len(ready) != 0 {
		t.Fatalf("retried terminal failure: %+v", ready)
	}
}
func TestManifestMustMatchSettings(t *testing.T) {
	ch := Settings{Name: "echo", Path: filepath.Join(t.TempDir(), "bin")}
	for _, input := range []string{`{"name":"other"}`, `{"name":"echo","extra":true}`, `{"name":"echo"}{}`} {
		if err := os.WriteFile(filepath.Join(filepath.Dir(ch.Path), "channel.json"), []byte(input), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ch.Manifest(); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}

func TestApprovalSummaryFitsOneDelivery(t *testing.T) {
	l, err := openLedger(t.TempDir(), "echo")
	if err != nil {
		t.Fatal(err)
	}
	defer l.close()
	a := ChannelApprover{host: &Host{ledger: l}, user: "reader", in: &p.Inbound{ID: "source", Chat: p.Chat{ID: "room"}, Sender: p.Sender{ID: "reader"}}}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := a.Ask(ctx, approval.Request{User: "reader", Expires: time.Now().Add(time.Minute), Summary: "start\x1b" + strings.Repeat("\u754c", p.MaxEventText/3+1)})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("no approval delivery: %v", err)
	case <-l.wake:
	}
	ds, _ := l.ready(map[string]bool{})
	if len(ds) != 1 {
		t.Fatal(len(ds))
	}
	text := ds[0].Text
	if !strings.HasPrefix(text, "start\ufffd") || !strings.HasSuffix(text, "[truncated]") || !utf8.ValidString(text) || len(text) > p.MaxEventText {
		t.Fatal("summary not safely truncated")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func TestReopenedOutboxDropsApprovalPrompts(t *testing.T) {
	dir := t.TempDir()
	l, err := openLedger(dir, "echo")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"prompt", "reply"} {
		d := p.Deliver{Type: p.TypeDeliver, ID: id, ChatID: "room", Text: "hello"}
		if id == "prompt" {
			d.Buttons = []p.Button{{ID: "approve", Label: "Approve"}, {ID: "deny", Label: "Deny"}}
		}
		if err = l.record(outRecord{Deliver: &d}); err != nil {
			t.Fatal(err)
		}
	}
	l.close()
	l, err = openLedger(dir, "echo")
	if err != nil {
		t.Fatal(err)
	}
	defer l.close()
	ds, _ := l.ready(map[string]bool{})
	if len(ds) != 1 || ds[0].ID != "reply" {
		t.Fatalf("replayed %+v", ds)
	}
}
func TestCompactionKeepsUnprocessedAndRecentIDs(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "channels", "echo")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	var inbox, outbox bytes.Buffer
	ie, oe := json.NewEncoder(&inbox), json.NewEncoder(&outbox)
	in := p.Inbound{V: 1, Seq: 2, Type: p.TypeInbound, Time: time.Now().Format(time.RFC3339), Sender: p.Sender{ID: "r"}, Chat: p.Chat{ID: "c"}, Text: strings.Repeat("x", 200)}
	for i := 0; i < 10005; i++ {
		in.ID = strconv.Itoa(i)
		if err := ie.Encode(in); err != nil {
			t.Fatal(err)
		}
		if i != 0 {
			if err := ie.Encode(map[string]string{"processed": in.ID}); err != nil {
				t.Fatal(err)
			}
		}
		d := p.Deliver{Type: p.TypeDeliver, ID: in.ID, ChatID: "c", Text: "reply"}
		if err := oe.Encode(outRecord{Deliver: &d}); err != nil {
			t.Fatal(err)
		}
		if i != 0 {
			if err := oe.Encode(outRecord{Delivered: &p.Delivered{ID: d.ID}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, data := range map[string]*bytes.Buffer{"inbox": &inbox, "outbox": &outbox} {
		if err := os.WriteFile(filepath.Join(root, name+".jsonl"), data.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		l, err := openLedger(dir, "echo")
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"0", "5", "10004"} {
			in.ID = id
			if fresh, err := l.accept(&in); err != nil || fresh {
				t.Fatal(id, fresh, err)
			}
		}
		if l.seen["1"] {
			t.Fatal("kept obsolete id")
		}
		remaining := l.replay()
		if len(remaining) != 1 || remaining[0].ID != "0" || remaining[0].Text != in.Text {
			t.Fatal("lost pending inbound")
		}
		if ds, _ := l.ready(map[string]bool{}); len(ds) != 1 || ds[0].ID != "0" {
			t.Fatal("lost pending delivery")
		}
		l.close()
	}
	for name, old := range map[string]int{"inbox": inbox.Len(), "outbox": outbox.Len()} {
		info, err := os.Stat(filepath.Join(root, name+".jsonl"))
		if err != nil || info.Size() >= int64(old/2) {
			t.Fatalf("%s did not shrink: %v %v", name, info, err)
		}
	}
}
func TestFailureWithoutRetryAfterIsTerminal(t *testing.T) {
	l, err := openLedger(t.TempDir(), "echo")
	if err != nil {
		t.Fatal(err)
	}
	defer l.close()
	d := p.Deliver{Type: p.TypeDeliver, ID: "failed", ChatID: "c", Text: "reply"}
	if err = l.record(outRecord{Deliver: &d}); err != nil {
		t.Fatal(err)
	}
	if err = l.record(outRecord{Failed: &p.DeliveryFailed{ID: d.ID, Error: p.Problem{Code: p.CodeUnauthorised, Message: "denied"}}}); err != nil {
		t.Fatal(err)
	}
	if ds, next := l.ready(map[string]bool{}); len(ds) != 0 || !next.IsZero() {
		t.Fatal(ds, next)
	}
}

func TestRecoveryAfterFinalReplyBeforeInboxMarker(t *testing.T) {
	dir := t.TempDir()
	l, err := openLedger(dir, "echo")
	if err != nil {
		t.Fatal(err)
	}
	in := p.Inbound{V: 1, Seq: 2, Type: p.TypeInbound, ID: "message", Time: time.Now().Format(time.RFC3339), Sender: p.Sender{ID: "reader"}, Chat: p.Chat{ID: "room"}, Text: "hello"}
	if _, err := l.accept(&in); err != nil {
		t.Fatal(err)
	}
	h := Host{ledger: l}
	answer := strings.Repeat("a", p.MaxEventText) + "end"
	if err := h.answer(&in, answer); err != nil {
		t.Fatal(err)
	}
	l.close() // Exit between the two journal writes.
	for range 2 {
		l, err = openLedger(dir, "echo")
		if err != nil {
			t.Fatal(err)
		}
		if len(l.replay()) != 0 {
			t.Error("queued a turn that already has its final reply")
		}
		ds, _ := l.ready(map[string]bool{})
		if len(ds) != 2 || ds[0].Text+ds[1].Text != answer {
			t.Fatal("lost part of the final answer")
		}
		l.close()
	}
}

func TestHandshakeTimeoutRetries(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	stdin, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	h := Host{}
	retry, err := h.connected(t.Context(), &process{in: stdin, out: r}, time.Millisecond)
	if !retry || err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("handshake: retry=%v err=%v", retry, err)
	}
}
