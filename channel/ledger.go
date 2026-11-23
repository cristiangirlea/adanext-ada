package channel

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	p "github.com/cristiangirlea/harness/contracts/pluginproto"
)

type pending struct {
	p.Deliver
	offer    *offerTarget
	failures int
	due      time.Time
}

type ledger struct {
	mu            sync.Mutex
	writeErr      error
	inbox, outbox *os.File
	seen          map[string]bool
	recovered     []*p.Inbound
	pending       map[string]*pending
	order         []string
	wake          chan struct{}
}

func openLedger(data, name string) (*ledger, error) {
	root, err := os.OpenRoot(data)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	dir := filepath.Join("channels", name)
	if err := root.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	l := &ledger{seen: map[string]bool{}, pending: map[string]*pending{}, wake: make(chan struct{}, 1)}
	var ids []string
	unfinished := map[string]*p.Inbound{}
	inPath := filepath.Join(dir, "inbox.jsonl")
	outPath := filepath.Join(dir, "outbox.jsonl")
	err = readJournal(root, inPath, func(line []byte) error {
		var mark processedRecord
		if err := json.Unmarshal(line, &mark); err != nil {
			return err
		}
		id := mark.Processed
		if id == "" {
			frame, err := p.ParsePluginLineV2(line, p.KindChannel)
			if err != nil {
				return err
			}
			in, ok := frame.(*p.Inbound)
			if !ok {
				return fmt.Errorf("expected inbox message")
			}
			id = in.ID
			unfinished[id] = in
		} else {
			delete(unfinished, id)
		}
		if !l.seen[id] {
			ids = append(ids, id)
			l.seen[id] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = readJournal(root, outPath, func(line []byte) error {
		var r outRecord
		if err := json.Unmarshal(line, &r); err != nil {
			return err
		}
		if r.Completes != "" {
			delete(unfinished, r.Completes)
		}
		return l.apply(r)
	})
	if err != nil {
		return nil, err
	}
	// Publish inbox completion before compacting its evidence out of the outbox.
	l.inbox, err = replaceJournal(root, inPath, func(enc *json.Encoder) error {
		for i, id := range ids {
			if in := unfinished[id]; in != nil {
				if err := enc.Encode(in); err != nil {
					return err
				}
				l.recovered = append(l.recovered, in)
			} else if i >= len(ids)-10000 {
				if err := enc.Encode(processedRecord{id}); err != nil {
					return err
				}
			} else {
				delete(l.seen, id)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	l.outbox, err = replaceJournal(root, outPath, func(enc *json.Encoder) error {
		var remaining []string
		for _, id := range l.order {
			item := l.pending[id]
			// Only learning buttons have a binding that survives this process.
			if len(item.Buttons) > 0 && item.offer == nil {
				delete(l.pending, id)
				continue
			}
			if err := enc.Encode(outRecord{Deliver: &item.Deliver, Offer: item.offer, Failures: item.failures, Due: item.due}); err != nil {
				return err
			}
			remaining = append(remaining, id)
		}
		l.order = remaining
		return nil
	})
	if err != nil {
		l.inbox.Close()
		return nil, err
	}
	return l, nil
}

type processedRecord struct {
	Processed string `json:"processed"`
}

// Only newline-terminated records are committed.
func readJournal(root *os.Root, path string, visit func([]byte) error) error {
	f, err := root.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	reader := bufio.NewReader(f)
	var offset int64
	for {
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err == nil {
			err = visit(line)
		}
		if err != nil {
			return fmt.Errorf("recover %s at byte %d: %w", path, offset, err)
		}
		offset += int64(len(line))
	}
}

func replaceJournal(root *os.Root, path string, compact func(*json.Encoder) error) (*os.File, error) {
	tmp := filepath.Join(filepath.Dir(path), ".compact-"+rand.Text())
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	defer root.Remove(tmp)
	w := bufio.NewWriter(f)
	err = compact(json.NewEncoder(w))
	if err == nil {
		err = w.Flush()
	}
	if err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return nil, err
	}
	if err = root.Rename(tmp, path); err != nil {
		return nil, err
	}
	return root.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
}

// Serve drains each recovered inbox before starting plugins.
func (l *ledger) replay() []*p.Inbound {
	in := l.recovered
	l.recovered = nil
	return in
}

func (l *ledger) processed(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.writeErr == nil {
		l.writeErr = appendRecord(l.inbox, processedRecord{id})
	}
	return l.writeErr
}
func (l *ledger) close() { l.inbox.Close(); l.outbox.Close() }
func appendRecord(f *os.File, v any) error {
	if err := json.NewEncoder(f).Encode(v); err != nil {
		return fmt.Errorf("append %s: %w", f.Name(), err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", f.Name(), err)
	}
	return nil
}

// Only the host reader accepts inbound frames, including across plugin restarts.
func (l *ledger) accept(in *p.Inbound) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.writeErr != nil {
		return false, l.writeErr
	}
	if l.seen[in.ID] {
		return false, nil
	}
	if l.writeErr = appendRecord(l.inbox, in); l.writeErr != nil {
		return false, l.writeErr
	}
	l.seen[in.ID] = true
	return true, nil
}

type outRecord struct {
	Offer        *offerTarget      `json:"offer,omitempty"`
	Completes    string            `json:"completes,omitempty"`
	Continuation []p.Deliver       `json:"continuation,omitempty"`
	Failures     int               `json:"failures,omitempty"`
	Due          time.Time         `json:"due,omitzero"`
	At           time.Time         `json:"at"`
	Deliver      *p.Deliver        `json:"deliver,omitempty"`
	Delivered    *p.Delivered      `json:"delivered,omitempty"`
	Failed       *p.DeliveryFailed `json:"delivery_failed,omitempty"`
}

func (l *ledger) apply(r outRecord) error {
	if d := r.Deliver; d != nil {
		if l.pending[d.ID] != nil {
			return fmt.Errorf("duplicate pending delivery")
		}
		l.pending[d.ID] = &pending{Deliver: *d, offer: r.Offer, failures: r.Failures, due: r.Due}
		l.order = append(l.order, d.ID)
		for i := range r.Continuation {
			if err := l.apply(outRecord{Deliver: &r.Continuation[i]}); err != nil {
				return err
			}
		}
		return nil
	}
	var id string
	switch {
	case r.Delivered != nil:
		id = r.Delivered.ID
	case r.Failed != nil:
		id = r.Failed.ID
	default:
		return fmt.Errorf("missing outbox frame")
	}
	item := l.pending[id]
	if item == nil {
		return fmt.Errorf("unexpected delivery outcome for %q", id)
	}
	if f := r.Failed; f != nil && f.Error.Code == p.CodeRateLimited {
		item.failures++
		if item.failures < 3 {
			var delay time.Duration
			if f.RetryAfterMS != nil {
				delay = time.Duration(min(*f.RetryAfterMS, int64(math.MaxInt64)/int64(time.Millisecond))) * time.Millisecond
			}
			item.due = r.At.Add(delay)
			return nil
		}
	}
	delete(l.pending, id)
	i := slices.Index(l.order, id)
	l.order = slices.Delete(l.order, i, i+1)
	return nil
}
func (l *ledger) record(r outRecord) error {
	if r.Deliver != nil {
		if err := validateDelivery(*r.Deliver); err != nil {
			return err
		}
	}
	for _, d := range r.Continuation {
		if err := validateDelivery(d); err != nil {
			return err
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.writeErr != nil {
		return l.writeErr
	}
	r.At = time.Now().UTC()
	// A failed append can leave a partial line. Stop writing until recovery.
	if l.writeErr = appendRecord(l.outbox, r); l.writeErr != nil {
		return l.writeErr
	}
	if l.writeErr = l.apply(r); l.writeErr != nil {
		return l.writeErr
	}
	select {
	case l.wake <- struct{}{}:
	default:
	}
	return nil
}
func validateDelivery(d p.Deliver) error {
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if _, err = p.ParseHostLineV2(data, p.KindChannel); err != nil {
		return fmt.Errorf("invalid delivery: %w", err)
	}
	return nil
}
func (l *ledger) ready(inFlight map[string]bool) ([]pending, time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	var next time.Time
	var ready []pending
	for _, id := range l.order {
		item := l.pending[id]
		if inFlight[id] {
			continue
		}
		if item.due.After(now) {
			if next.IsZero() || item.due.Before(next) {
				next = item.due
			}
			continue
		}
		ready = append(ready, *item)
		inFlight[id] = true
	}
	return ready, next
}
