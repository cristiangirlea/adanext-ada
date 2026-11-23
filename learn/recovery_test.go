package learn

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/adanext-ada/memory"
)

func TestInterruptedProfileChanges(t *testing.T) {
	f := setup(t)
	f.append(t, "me", "user", "text", "hello")
	items, err := Propose(t.Context(), &fake{reply: one("prefers tea", "[0]")}, f.Store, "owner", f.session)
	if err != nil || len(items) != 1 {
		t.Fatal(items, err)
	}
	p := items[0]
	now := time.Now().UTC()
	journal := filepath.Join(f.dir, "learning", ".intent-owner.json")
	prepare := func(action string) {
		t.Helper()
		data, err := json.Marshal(intent{p.ID, action, now})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(journal, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	check := func(status string) Proposal {
		t.Helper()
		got, err := f.List("owner", true)
		if err != nil || len(got) != 1 || got[0].Status != status {
			t.Fatal(got, err)
		}
		if _, err := os.Stat(journal); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("expected recovery to remove the intent", err)
		}
		return got[0]
	}
	prepare("keep")
	check("pending")
	prepare("keep")
	fact, err := f.Memory.AddFact("owner", p.Text, source(&p))
	if err != nil {
		t.Fatal(err)
	}
	if kept := check("kept"); kept.FactID != fact.ID || !kept.Confirmed.Equal(now) {
		t.Fatal(kept)
	}
	prepare("keep")
	check("kept")
	now = now.Add(2 * time.Hour)
	if _, err := f.Sweep("owner", now, time.Hour, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	stale := check("stale")
	prepare("archive")
	if p := check("stale"); !p.Decided.Equal(stale.Decided) || !slices.Equal(f.Memory.Facts("owner"), []memory.Fact{fact}) {
		t.Fatal("archive advanced before profile removal", p)
	}
	prepare("archive")
	if err := f.Memory.RemoveFact("owner", fact.ID); err != nil {
		t.Fatal(err)
	}
	if p := check("archived"); !p.Decided.Equal(now) || p.FactID != fact.ID {
		t.Fatal(p)
	}
	now = now.Add(time.Hour)
	prepare("restore")
	restored, err := f.Memory.AddFact("owner", p.Text, source(&p))
	if err != nil {
		t.Fatal(err)
	}
	if p := check("kept"); p.FactID != restored.ID || p.FactID == fact.ID || !p.Confirmed.Equal(now) || !slices.Equal(f.Memory.Facts("owner"), []memory.Fact{restored}) {
		t.Fatal("restore did not adopt the new profile fact", p)
	}
	prepare("rollback")
	check("kept")
	prepare("rollback")
	if err := f.Memory.RemoveFact("owner", restored.ID); err != nil {
		t.Fatal(err)
	}
	check("rolled_back")
	if facts := f.Memory.Facts("owner"); len(facts) != 0 {
		t.Fatal(facts)
	}
}

func TestKeepRefusedByFullProfile(t *testing.T) {
	f := setup(t)
	f.append(t, "me", "user", "text", "hello")
	items, err := Propose(t.Context(), &fake{reply: one("likes tea", "[0]")}, f.Store, "owner", f.session)
	if err != nil || len(items) != 1 {
		t.Fatal(items, err)
	}
	var before []memory.Fact
	for i := 0; i < 40; i++ {
		fact, err := f.Memory.AddFact("owner", fmt.Sprintf("fact %d", i), "cli")
		if err != nil {
			t.Fatal(err)
		}
		before = append(before, fact)
	}
	if _, err := f.Decide("owner", items[0].ID, "keep", false, time.Now()); err == nil || !strings.Contains(err.Error(), "40 facts") {
		t.Fatal("expected profile limit", err)
	}
	// Inspect the journal before listing, which would recover a leftover intent.
	if _, err := os.Stat(filepath.Join(f.dir, "learning", ".intent-owner.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("expected refused keep to remove the intent", err)
	}
	pending, err := f.List("owner", false)
	if err != nil || len(pending) != 1 || pending[0].ID != items[0].ID || !slices.Equal(f.Memory.Facts("owner"), before) {
		t.Fatal(pending, err)
	}
}

func concurrentReviewsAndKeeps(t *testing.T) {
	f := setup(t)
	f.append(t, "me", "user", "text", "hello")
	second, err := memory.NewStore(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	type review struct {
		items []Proposal
		calls int
		err   error
	}
	done := make(chan review, 2)
	for _, s := range []Store{f.Store, {Memory: second}} {
		go func() {
			model := &fake{reply: one("fact", "[0]")}
			items, err := Propose(t.Context(), model, s, "owner", f.session)
			done <- review{items, len(model.requests), err}
		}()
	}
	a, b := <-done, <-done
	items := slices.Concat(a.items, b.items)
	if a.err != nil || b.err != nil || a.calls+b.calls != 1 || len(items) != 1 {
		t.Fatal(a, b, a.err, b.err)
	}
	errs := make(chan error, 2)
	for _, s := range []Store{f.Store, {Memory: second}} {
		go func() { _, err := s.Decide("owner", items[0].ID, "keep", false, time.Now()); errs <- err }()
	}
	if first, second := <-errs, <-errs; (first == nil) == (second == nil) || len(f.Memory.Facts("owner")) != 1 {
		t.Fatal(first, second)
	}
}

func TestRecentReviewLimitAndWindow(t *testing.T) {
	f := setup(t)
	for i := 0; i < 12; i++ {
		session, err := f.Memory.NewSession("owner", "cli", "local", "me")
		if err != nil {
			t.Fatal(err)
		}
		f.session = session.ID
		f.append(t, "me", "user", "text", "hello")
	}
	p := &fake{reply: `{"facts":[]}`}
	base := time.Now().UTC()
	for _, tc := range []struct {
		age   time.Duration
		total int
	}{
		{30 * time.Minute, 0}, {25 * time.Hour, 0}, {2 * time.Hour, 10}, {2 * time.Hour, 12}, {2 * time.Hour, 12},
	} {
		_, err := f.ProposeRecent(t.Context(), p, "owner", base.Add(tc.age), time.Hour, 24*time.Hour)
		if err != nil || len(p.requests) != tc.total {
			t.Fatal(tc, err, len(p.requests))
		}
	}
}

func TestRollbackAfterManualRemoval(t *testing.T) {
	f := setup(t)
	f.append(t, "me", "user", "text", "hello")
	ps, err := Propose(t.Context(), &fake{reply: one("tea", "[0]")}, f.Store, "owner", f.session)
	if err != nil || len(ps) != 1 {
		t.Fatal(ps, err)
	}
	p, err := f.Decide("owner", ps[0].ID, "keep", false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Memory.RemoveFact("owner", p.FactID); err != nil {
		t.Fatal(err)
	}
	replacement, err := f.Memory.AddFact("owner", "tea", "cli")
	if err != nil {
		t.Fatal(err)
	}
	if p, err = f.Decide("owner", p.ID, "rollback", false, time.Now()); err != nil || p.Status != "rolled_back" || !slices.Equal(f.Memory.Facts("owner"), []memory.Fact{replacement}) {
		t.Fatal(p, err)
	}
}

// Each round creates fresh lock files, the moment two stores race to create them.
func TestConcurrentReviewsAndKeeps(t *testing.T) {
	for i := range 25 {
		t.Run(fmt.Sprint(i), concurrentReviewsAndKeeps)
	}
}
