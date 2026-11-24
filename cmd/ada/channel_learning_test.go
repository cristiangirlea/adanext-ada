package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristiangirlea/adanext-ada/config"
	"github.com/cristiangirlea/adanext-ada/learn"
	"github.com/cristiangirlea/adanext-ada/memory"
	"github.com/cristiangirlea/adanext-ada/provider"
	p "github.com/cristiangirlea/harness/contracts/pluginproto"
)

type learningModel struct {
	reviews       atomic.Int32
	failFirst     bool
	reviewStarted chan struct{}
}

func (*learningModel) Name() string { return "fake" }
func (m *learningModel) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	text := "ordinary chat reply"
	if strings.Contains(req.System, "Propose at most") {
		if m.reviews.Add(1) == 1 && m.failFirst {
			return provider.Response{}, fmt.Errorf("test learning outage")
		}
		if m.reviewStarted != nil {
			close(m.reviewStarted)
			<-ctx.Done() // Return valid facts to exercise cancellation after Complete.
		}
		text = `{"facts":[{"text":"likes tea","messages":[0]},{"text":"likes coffee","messages":[1]}]}`
	}
	if strings.Contains(req.System, "one reusable skill") {
		text = cliSkillReply
	}
	return provider.Response{Content: []provider.Block{{Type: "text", Text: text}}}, nil
}

type learningChat struct {
	*fixture
	app      *config.App
	settings config.Settings
	model    *learningModel
}

func newLearningChat(t *testing.T, groups bool) *learningChat {
	t.Helper()
	f := newFixture(t)
	f.manifest()
	f.configure(map[string]any{"name": "echo", "groups": groups})
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
	m := new(learningModel)
	app.Provider = m
	settings.SessionIdle = time.Nanosecond
	settings.Learning.ProposeEvery = 35 * time.Millisecond
	return &learningChat{f, app, settings, m}
}

func (c *learningChat) startLearning(diagnostics io.Writer) {
	c.t.Helper()
	s := newServeService(c.settings, c.app, diagnostics)
	ready := make(chan struct{}, 1)
	status := s.Status
	s.Status = func(line string) {
		status(line)
		if line == "echo ready" {
			ready <- struct{}{}
		}
	}
	c.startServer(func(ctx context.Context) error { return s.Serve(ctx, c.dir, c.settings.Channels) })
	c.connect("echo")
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		c.t.Fatal("channel did not become ready")
	}
}

func (c *learningChat) session(user, channel, chat, sender string) string {
	c.t.Helper()
	origin := memory.Origin{Channel: channel, Chat: chat, Sender: sender, Private: true}
	s, err := c.app.Store.NewSessionAt(user, origin, "")
	if err != nil {
		c.t.Fatal(err)
	}
	if err := c.app.Store.AppendAt(s.ID, origin, "", provider.Message{Role: "user", Content: []provider.Block{{Type: "text", Text: "likes tea"}}}, provider.Message{Role: "assistant", Content: []provider.Block{{Type: "text", Text: "likes coffee"}}}); err != nil {
		c.t.Fatal(err)
	}
	return s.ID
}

func (f *fixture) press(sender, chat, delivery, button string) string {
	f.t.Helper()
	f.seq++
	id := rand.Text()
	kind := p.ChatPrivate
	f.send(p.Inbound{V: 1, Seq: f.seq, Type: p.TypeInbound, ID: id, Sender: p.Sender{ID: sender}, Chat: p.Chat{ID: chat, Type: &kind}, Time: time.Now().Format(time.RFC3339), Action: &p.Action{DeliveryID: delivery, ButtonID: button}})
	f.ack(id)
	return id
}

func (c *learningChat) outbox() []byte {
	c.t.Helper()
	data, err := os.ReadFile(filepath.Join(c.dir, "channels", "echo", "outbox.jsonl"))
	if err != nil {
		c.t.Fatal(err)
	}
	return data
}

func removeOriginPrivacy(t *testing.T, dir, session string) {
	t.Helper()
	path := filepath.Join(dir, "sessions", session, "meta.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	var origins []map[string]json.RawMessage
	if err := json.Unmarshal(meta["origins"], &origins); err != nil {
		t.Fatal(err)
	}
	for _, origin := range origins {
		delete(origin, "private")
	}
	meta["origins"], err = json.Marshal(origins)
	if err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestLearningSweepRequiresRecordedPrivateOrigin(t *testing.T) {
	c := newLearningChat(t, true)
	c.settings.Learning.ProposeEvery = 0
	c.settings.SessionIdle = time.Hour
	c.startLearning(io.Discard)
	users := []string{"group", "legacy", "missing-type", "private"}
	private, group := p.ChatPrivate, p.ChatGroup
	for _, user := range users {
		if err := c.app.Store.Link("echo", user, user); err != nil {
			t.Fatal(err)
		}
		kind := &private
		switch user {
		case "group":
			kind = &group
		case "missing-type":
			kind = nil
		}
		c.seq++
		c.send(p.Inbound{V: 1, Seq: c.seq, Type: p.TypeInbound, ID: user, Time: time.Now().Format(time.RFC3339), Sender: p.Sender{ID: user}, Chat: p.Chat{ID: user, Type: kind}, Text: "likes tea"})
		c.ack(user)
		c.delivered(c.delivery())
	}
	c.inbound("private", "private", "duplicate", false)
	c.ack("private")
	c.shutdown()
	sessions, err := c.app.Store.List("legacy", 0)
	if err != nil || len(sessions) != 1 {
		t.Fatal(sessions, err)
	}
	removeOriginPrivacy(t, c.dir, sessions[0].ID)

	c.settings.Channels[0].Groups = false
	c.settings.SessionIdle = time.Nanosecond
	c.settings.Learning.ProposeEvery = 35 * time.Millisecond
	c.startLearning(io.Discard)
	// Users are swept in order. The two private offers mark the end of the sweep.
	for range 2 {
		for {
			d := c.delivery()
			c.delivered(d)
			if d.ChatID == "private" {
				break
			}
			t.Errorf("sent an offer to %s", d.ChatID)
		}
	}
	c.shutdown()
	for _, user := range users {
		items, err := (learn.Store{Memory: c.app.Store}).List(user, false)
		if err != nil || len(items) != 2 {
			t.Fatalf("%s proposals: %v, %v", user, items, err)
		}
		for _, item := range items {
			if (item.Offer != nil) != (user == "private") {
				t.Errorf("%s offer = %+v", user, item.Offer)
			}
		}
	}
}

func TestLearningOfferQueuedDuringReconnect(t *testing.T) {
	c := newLearningChat(t, false)
	if err := c.app.Store.Link("echo", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	lines := make(logLines, 16)
	c.startLearning(lines)
	c.kill()
	lines.wait(t, "disconnected")
	c.session("owner", "echo", "room", "alice")
	// Withhold the next ready handshake until the offers are durable.
	for deadline := time.Now().Add(3 * time.Second); ; {
		if bytes.Count(c.outbox(), []byte(`"offer":`)) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no offers queued during reconnect, reviews = %d", c.model.reviews.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.connect("echo")
	for range 2 {
		d := c.delivery()
		if d.ChatID != "room" || len(d.Buttons) != 2 {
			t.Fatal(d)
		}
		c.delivered(d)
	}
}

func TestShutdownDuringLearningReview(t *testing.T) {
	c := newLearningChat(t, false)
	if err := c.app.Store.Link("echo", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	c.model.reviewStarted = make(chan struct{})
	c.startLearning(io.Discard)
	c.session("owner", "echo", "room", "alice")
	select {
	case <-c.model.reviewStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("review did not enter Complete")
	}
	c.shutdown()
	items, err := (learn.Store{Memory: c.app.Store}).List("owner", true)
	if err != nil || len(items) != 0 || len(c.outbox()) != 0 {
		t.Fatal("canceled review persisted a proposal or delivery", items, err)
	}
}

func TestLearningChatOffersAndDecisions(t *testing.T) {
	c := newLearningChat(t, false)
	for _, sender := range []string{"alice", "bob"} {
		if err := c.app.Store.Link("echo", sender, "reader"); err != nil {
			t.Fatal(err)
		}
	}
	c.model.failFirst = true
	var diagnostics bytes.Buffer
	c.startLearning(&diagnostics)
	session := c.session("reader", "echo", "room", "alice")
	offers := map[string]*p.Deliver{}
	for range 2 {
		d := c.delivery()
		if d.ChatID != "room" || len(d.Buttons) != 2 || d.Buttons[1] != (p.Button{ID: "reject", Label: "Forget it"}) {
			t.Fatal(d)
		}
		offers[d.Buttons[0].Label] = d
	}
	inside, outside := offers["Keep"], offers["Keep anyway"]
	if inside == nil || outside == nil || inside.Buttons[0].ID != "keep" || outside.Buttons[0].ID != "keep" || inside.Text != `Remember this about you? "likes tea"` || outside.Text != "Remember this about you? \"likes coffee\"\nIt came from a conversation that included content from outside (a tool, a file, a page or another sender)." {
		t.Fatal(offers)
	}
	c.delivered(inside)
	c.press("bob", "room", inside.ID, "keep")
	c.press("alice", "other", inside.ID, "reject")
	c.press("alice", "room", inside.ID, "approve")
	c.press("stranger", "room", inside.ID, "keep")
	c.inbound("typed", "alice", "keep", false)
	c.ack("typed")
	c.shutdown()
	if !strings.Contains(diagnostics.String(), "test learning outage") {
		t.Fatal(diagnostics.String())
	}
	store := learn.Store{Memory: c.app.Store}
	items, err := store.List("reader", false)
	if err != nil || len(items) != 2 || len(c.app.Store.Facts("reader")) != 0 {
		t.Fatal(items, err)
	}
	// Restart with the sweep disabled to exercise the binding and the unacknowledged delivery.
	c.settings.Learning.ProposeEvery = 0
	c.startLearning(io.Discard)
	replay := c.delivery()
	if replay.ID != outside.ID {
		t.Fatal("changed offer on replay", replay)
	}
	c.delivered(replay)
	c.press("alice", "room", outside.ID, "keep")
	if d := c.delivery(); d.Text != "Kept." || d.ChatID != "room" {
		t.Fatal(d)
	} else {
		c.delivered(d)
	}
	c.press("alice", "room", outside.ID, "reject")
	c.press("alice", "room", inside.ID, "reject")
	if d := c.delivery(); d.Text != "Forgotten." || d.ChatID != "room" {
		t.Fatal(d)
	} else {
		c.delivered(d)
	}
	c.press("alice", "room", inside.ID, "keep")
	c.shutdown()
	items, err = store.List("reader", true)
	if err != nil || len(items) != 2 || items[0].Status != "rejected" || items[1].Status != "kept" {
		t.Fatal(items, err)
	}
	facts := c.app.Store.Facts("reader")
	if len(facts) != 1 || facts[0].Source != "learn:"+items[1].ID+" session:"+session || facts[0].Text != "likes coffee" {
		t.Fatal(facts)
	}
}

func TestLearningSweepUsesLatestOriginAndOnlyNewProposals(t *testing.T) {
	c := newLearningChat(t, false)
	if err := c.app.Store.Link("echo", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	session := c.session("owner", "echo", "room", "alice")
	store := learn.Store{Memory: c.app.Store}
	items, err := learn.Propose(t.Context(), c.model, store, "owner", session)
	if err != nil || len(items) != 2 {
		t.Fatal(items, err)
	}
	if err := c.app.Store.Link("echo", "reader", "another"); err != nil {
		t.Fatal(err)
	}
	writer := *c
	// A reader over the same directory has no session writer capability.
	reader, err := memory.NewStore(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close() })
	app := *c.app
	app.Store = reader
	c.app = &app
	c.settings.Learning.ProposeEvery = 350 * time.Millisecond
	c.settings.SessionIdle = 100 * time.Millisecond
	start := time.Now()
	c.startLearning(io.Discard)
	second := writer.session("another", "echo", "latest", "reader")
	msg := provider.Message{Role: "user", Content: []provider.Block{{Type: "text", Text: "more"}}}
	for _, room := range []string{"earlier", "latest"} {
		if err := writer.app.Store.AppendAt(second, memory.Origin{Channel: "echo", Chat: room, Sender: "reader", Private: true}, "", msg); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		d := c.delivery()
		if d.ChatID != "latest" || time.Since(start) < c.settings.Learning.ProposeEvery {
			t.Fatal(d)
		}
		c.delivered(d)
	}
	c.shutdown()
	items, err = store.List("owner", true)
	if err != nil || len(items) != 2 || items[0].Offer != nil || items[1].Offer != nil || c.model.reviews.Load() != 2 {
		t.Fatal(items, err, c.model.reviews.Load())
	}
}

func TestRefusedKeepLeavesChatAvailable(t *testing.T) {
	c := newLearningChat(t, false)
	if err := c.app.Store.Link("echo", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	for i := range 40 {
		if _, err := c.app.Store.AddFact("owner", fmt.Sprintf("fact %d", i), "cli"); err != nil {
			t.Fatal(err)
		}
	}
	var diagnostics bytes.Buffer
	c.startLearning(&diagnostics)
	c.session("owner", "echo", "room", "alice")
	d := c.delivery()
	c.delivered(d)
	c.delivered(c.delivery())
	press := c.press("alice", "room", d.ID, "keep")
	if reply := c.delivery(); reply.Text != "Could not keep it: profile limit is 40 facts" || reply.ChatID != "room" || reply.ReplyTo == nil || *reply.ReplyTo != press {
		t.Fatal(reply)
	} else {
		c.delivered(reply)
	}
	pending, err := (learn.Store{Memory: c.app.Store}).List("owner", false)
	if err != nil || len(pending) != 2 || len(c.app.Store.Facts("owner")) != 40 {
		t.Fatal(pending, err)
	}
	// A filesystem refusal carries a local path in diagnostics only.
	profile := filepath.Join(c.dir, "profiles", "owner.json")
	if err := os.Rename(profile, profile+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(profile, 0700); err != nil {
		t.Fatal(err)
	}
	c.press("alice", "room", d.ID, "keep")
	if reply := c.delivery(); reply.Text != "Could not keep it: the profile is unavailable" {
		t.Fatal(reply)
	} else {
		c.delivered(reply)
	}
	c.press("alice", "room", d.ID, "reject")
	if reply := c.delivery(); reply.Text != "Forgotten." {
		t.Fatal(reply)
	}
	c.shutdown()
	if !strings.Contains(diagnostics.String(), "40 facts") {
		t.Fatal(diagnostics.String())
	}
}

func TestOutstandingOffersRecheckEligibilityOnRestart(t *testing.T) {
	for _, mode := range []string{"groups", "legacy-origin", "relinked", "cli-decided"} {
		t.Run(mode, func(t *testing.T) {
			c := newLearningChat(t, false)
			for _, sender := range []string{"alice", "bob"} {
				if err := c.app.Store.Link("echo", sender, "owner"); err != nil {
					t.Fatal(err)
				}
			}
			c.startLearning(io.Discard)
			session := c.session("owner", "echo", "room", "alice")
			first := c.delivery()
			c.delivery()
			c.shutdown()
			switch mode {
			case "legacy-origin":
				removeOriginPrivacy(t, c.dir, session)
			case "groups":
				c.settings.Channels[0].Groups = true
			case "relinked":
				if err := c.app.Store.Link("echo", "alice", "someone-else"); err != nil {
					t.Fatal(err)
				}
			case "cli-decided":
				items, err := (learn.Store{Memory: c.app.Store}).List("owner", false)
				if err != nil || len(items) != 2 {
					t.Fatal(items, err)
				}
				for _, item := range items {
					if _, err := c.cli("learn", "reject", item.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			c.settings.Learning.ProposeEvery = 0
			c.startLearning(io.Discard)
			if mode == "relinked" || mode == "cli-decided" {
				c.press("alice", "room", first.ID, "keep")
			}
			c.inbound("barrier", "bob", "/stop", false)
			c.ack("barrier")
			if d := c.delivery(); d.Text != "Nothing is running." {
				t.Fatal(d)
			}
		})
	}
}

func TestReplayedKeepAfterInterruptedProfileWrite(t *testing.T) {
	for _, factWritten := range []bool{false, true} {
		t.Run(fmt.Sprintf("fact-written-%t", factWritten), func(t *testing.T) {
			c := newLearningChat(t, false)
			if err := c.app.Store.Link("echo", "alice", "owner"); err != nil {
				t.Fatal(err)
			}
			session := c.session("owner", "echo", "room", "alice")
			store := learn.Store{Memory: c.app.Store}
			items, err := learn.Propose(t.Context(), c.model, store, "owner", session)
			if err != nil || len(items) != 2 {
				t.Fatal(items, err)
			}
			item := items[0]
			offer := learn.Offer{Channel: "echo", Chat: "room", Sender: "alice", DeliveryID: "delivery", Expires: time.Now().Add(time.Hour)}
			if ok, err := store.BindOffer("owner", item.ID, offer); !ok || err != nil {
				t.Fatal(ok, err)
			}
			stamp := time.Now().UTC()
			dir := filepath.Join(c.dir, "channels", "echo")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			private := p.ChatPrivate
			press := p.Inbound{V: 1, Seq: 2, Type: p.TypeInbound, ID: "press", Time: stamp.Format(time.RFC3339), Sender: p.Sender{ID: "alice"}, Chat: p.Chat{ID: "room", Type: &private}, Action: &p.Action{DeliveryID: offer.DeliveryID, ButtonID: "keep"}}
			data, err := json.Marshal(press)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "inbox.jsonl"), append(data, '\n'), 0600); err != nil {
				t.Fatal(err)
			}
			intent := fmt.Sprintf(`{"id":%q,"action":"keep","at":%q,"decision_inbound":"press"}`, item.ID, stamp.Format(time.RFC3339Nano))
			if err := os.WriteFile(filepath.Join(c.dir, "learning", ".intent-owner.json"), []byte(intent), 0600); err != nil {
				t.Fatal(err)
			}
			source := "learn:" + item.ID + " session:" + session
			if factWritten {
				if _, err := c.app.Store.AddFact("owner", item.Text, source); err != nil {
					t.Fatal(err)
				}
			} else {
				recovered, err := store.List("owner", true)
				if err != nil || len(recovered) != 2 || recovered[0].Status != "pending" || recovered[0].DecisionInbound != "" || len(c.app.Store.Facts("owner")) != 0 {
					t.Fatal(recovered, err)
				}
			}
			c.settings.Learning.ProposeEvery = 0
			c.startLearning(io.Discard)
			reply := c.delivery()
			if reply.Text != "Kept." || reply.ChatID != "room" || reply.ReplyTo == nil || *reply.ReplyTo != press.ID {
				t.Fatal(reply)
			}
			c.delivered(reply)
			c.seq++
			press.Seq = c.seq
			c.send(press)
			c.ack(press.ID)
			c.shutdown()
			if n := bytes.Count(c.outbox(), []byte(`"completes":"press"`)); n != 1 {
				t.Fatalf("recorded %d confirmations", n)
			}
			items, err = store.List("owner", true)
			facts := c.app.Store.Facts("owner")
			if err != nil || len(items) != 2 || len(facts) != 1 || facts[0].Source != source || items[0].FactID != facts[0].ID || items[0].DecisionInbound != press.ID || items[0].Status != "kept" {
				t.Fatal(items, facts, err)
			}
		})
	}
}

func TestLearningChatEligibility(t *testing.T) {
	for _, mode := range []string{"groups", "unlinked", "disabled", "expired"} {
		t.Run(mode, func(t *testing.T) {
			c := newLearningChat(t, mode == "groups")
			if mode == "disabled" {
				c.settings.Learning.ProposeEvery = 0
			}
			if mode == "expired" {
				c.settings.Learning.OfferTTL = 500 * time.Millisecond
			}
			if mode != "unlinked" {
				if err := c.app.Store.Link("echo", "alice", "owner"); err != nil {
					t.Fatal(err)
				}
			}
			c.startLearning(io.Discard)
			c.session("owner", "echo", "room", "alice")
			store := learn.Store{Memory: c.app.Store}
			if mode == "disabled" {
				time.Sleep(110 * time.Millisecond)
				c.shutdown()
				if c.model.reviews.Load() != 0 {
					t.Fatal("disabled sweep ran")
				}
				return
			}
			if mode == "expired" {
				d := c.delivery()
				c.delivered(d)
				c.delivered(c.delivery())
				time.Sleep(c.settings.Learning.OfferTTL)
				c.press("alice", "room", d.ID, "keep")
			}
			var items []learn.Proposal
			for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
				var err error
				items, err = store.List("owner", false)
				if err != nil {
					t.Fatal(err)
				}
				if len(items) == 2 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			time.Sleep(2 * c.settings.Learning.ProposeEvery)
			c.shutdown()
			if len(items) != 2 || c.model.reviews.Load() != 1 || len(c.app.Store.Facts("owner")) != 0 {
				t.Fatal(items, c.model.reviews.Load())
			}
			if mode != "expired" {
				outbox, err := os.ReadFile(filepath.Join(c.dir, "channels", "echo", "outbox.jsonl"))
				if err != nil || len(outbox) != 0 || slices.ContainsFunc(items, func(p learn.Proposal) bool { return p.Offer != nil }) {
					t.Fatalf("%s outbox = %q, %v", mode, outbox, err)
				}
			} else {
				if _, err := c.cli("learn", "reject", items[0].ID); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
