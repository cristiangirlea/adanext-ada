package channel

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/adanext-ada/loop"
	"github.com/cristiangirlea/adanext-ada/memory"
	"github.com/cristiangirlea/adanext-ada/provider"
	"github.com/cristiangirlea/adanext-ada/schedule"
	p "github.com/cristiangirlea/harness/contracts/pluginproto"
)

type scheduleModel struct{}

func (scheduleModel) Name() string { return "fake" }
func (scheduleModel) Complete(context.Context, provider.Request) (provider.Response, error) {
	return provider.Response{Content: []provider.Block{{Type: "text", Text: strings.Repeat("a", p.MaxEventText) + "last"}}}, nil
}

func TestServeRunsJobsWithoutChannels(t *testing.T) {
	dir := t.TempDir()
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
	at := time.Now()
	ticks := make(chan time.Time)
	logged := make(chan string, 1)
	s := Service{}
	s.Scheduler = &schedule.Runner{Store: schedule.Store{Memory: m}, Loop: &loop.Loop{Provider: scheduleModel{}}, Owner: "owner", Now: func() time.Time { return at }, Ticks: ticks, Deliver: s.Deliver, Status: func(line string) { logged <- line }}
	job, err := s.Scheduler.Store.Add(schedule.Job{Kind: "prompt", Spec: "every 1m", Prompt: "report", Target: schedule.Target{Channel: "offline", Chat: "room"}}, at.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, dir, nil) }()
	select {
	case ticks <- at:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case line := <-logged:
		if !strings.Contains(line, job.ID) || !strings.Contains(line, "offline") {
			t.Fatal(line)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	runs, err := s.Scheduler.Store.Runs(1)
	if err != nil || len(runs) != 1 || runs[0].Delivered {
		t.Fatal("expected one undelivered run", err)
	}
}

func TestScheduledAnswerOnPluginWire(t *testing.T) {
	dir := t.TempDir()
	store, err := memory.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	unlock, err := store.LockSessions()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ready := make(chan struct{})
	s := &Service{Status: func(line string) {
		if line == "one ready" {
			close(ready)
		}
	}}
	ledger, err := openLedger(dir, "one")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ledger.close()
		reopened, err := openLedger(dir, "one")
		if err != nil {
			t.Errorf("reopen scheduled deliveries: %v", err)
			return
		}
		reopened.close()
	}()
	h := &Host{Settings: Settings{Name: "one"}, service: s, ledger: ledger}
	pluginIn, hostIn, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pluginIn.Close()
	defer hostIn.Close()
	hostOut, pluginOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer hostOut.Close()
	defer pluginOut.Close()
	done := make(chan error, 1)
	go func() { _, err := h.connected(ctx, &process{in: hostIn, out: hostOut}, time.Second); done <- err }()
	defer func() { cancel(); <-done }()
	dec := json.NewDecoder(bufio.NewReader(pluginIn))
	enc := json.NewEncoder(pluginOut)
	var hello p.Hello
	if err = dec.Decode(&hello); err != nil {
		t.Fatal(err)
	}
	if err = enc.Encode(p.Ready{V: 1, Seq: 1, Type: p.TypeReady, Protocol: p.V2, Kind: p.KindChannel, Plugin: p.Program{Name: "fake", Version: "1"}, Channel: &p.Channel{Name: "one"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
	case err := <-done:
		done <- err
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	now := time.Now()
	r := schedule.Runner{Store: schedule.Store{Memory: store}, Loop: &loop.Loop{Provider: scheduleModel{}}, Owner: "owner", Deliver: s.Deliver, Now: func() time.Time { return now }}
	for _, target := range []schedule.Target{{Channel: "one", Chat: "room"}, {Channel: "offline", Chat: "elsewhere"}, {}} {
		if _, err = r.Store.Add(schedule.Job{Kind: "prompt", Spec: "every 1m", Prompt: "report", Target: target}, now); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(time.Minute)
	tick := make(chan error, 1)
	go func() { tick <- r.Tick(ctx) }()
	pluginIn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var retryID string
	for i, want := range []string{strings.Repeat("a", p.MaxEventText), "last", "last"} {
		var d p.Deliver
		if err = dec.Decode(&d); err != nil {
			t.Fatal(err)
		}
		if d.Type != p.TypeDeliver || d.ChatID != "room" || d.ReplyTo != nil || d.Text != want {
			t.Fatalf("wrong delivery: chat=%q type=%q bytes=%d", d.ChatID, d.Type, len(d.Text))
		}
		if i == 1 {
			retryID = d.ID
			if err = enc.Encode(p.DeliveryFailed{V: 1, Seq: 3, Type: p.TypeDeliveryFailed, ID: d.ID, Error: p.Problem{Code: p.CodeRateLimited, Message: "busy"}}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if i == 2 && d.ID != retryID {
			t.Fatalf("retried %q, want %q", d.ID, retryID)
		}
		if err = enc.Encode(p.Delivered{V: 1, Seq: int64(i + 2), Type: p.TypeDelivered, ID: d.ID, MessageID: "message"}); err != nil {
			t.Fatal(err)
		}
	}
	if err = <-tick; err != nil {
		t.Fatal(err)
	}
	runs, err := r.Store.Runs(3)
	if err != nil || len(runs) != 3 || runs[0].Delivered || runs[1].Delivered || !runs[2].Delivered || len(runs[2].Output) != 64<<10 {
		t.Fatalf("incorrect run history (%d rows): %v", len(runs), err)
	}
}

func TestUnconfirmedScheduledRunLeavesDeliveryPending(t *testing.T) {
	for _, reason := range []string{"timeout", "disconnect"} {
		t.Run(reason, func(t *testing.T) {
			dir := t.TempDir()
			store, err := memory.NewStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			unlock, err := store.LockSessions()
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			l, err := openLedger(dir, "one")
			if err != nil {
				t.Fatal(err)
			}
			defer l.close()
			s := &Service{}
			h := Host{Settings: Settings{Name: "one"}, service: s, ledger: l}
			h.register()
			r := schedule.Runner{Store: schedule.Store{Memory: store}, Loop: &loop.Loop{Provider: scheduleModel{}}, Owner: "owner", Deliver: s.Deliver}
			job, err := r.Store.Add(schedule.Job{Kind: "prompt", Spec: "every 1m", Prompt: "report", Target: schedule.Target{Channel: "one", Chat: "room"}, Budget: schedule.Budget{Timeout: time.Second}}, time.Now().Add(-time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- r.Tick(t.Context()) }()
			select {
			case <-l.wake:
				if reason == "disconnect" {
					h.unregister()
				}
				err = <-done
			case err = <-done:
				t.Fatalf("job finished before queuing its delivery: %v", err)
			}
			if err != nil {
				t.Fatal(err)
			}
			parts, _ := l.ready(map[string]bool{})
			if len(parts) != 2 {
				t.Fatalf("got %d pending parts, want 2", len(parts))
			}
			for _, part := range parts {
				if err := l.record(outRecord{Delivered: &p.Delivered{ID: part.ID, MessageID: "late"}}); err != nil {
					t.Fatal(err)
				}
			}
			runs, err := r.Store.Runs(1)
			if err != nil || len(runs) != 1 || runs[0].Job != job.ID || runs[0].Delivered {
				t.Fatalf("expected an undelivered run after late confirmation: %v", err)
			}
		})
	}
}
