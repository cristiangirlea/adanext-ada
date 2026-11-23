package channel

import (
	"context"
	"fmt"

	"github.com/cristiangirlea/adanext-ada/schedule"
)

type receipt struct {
	outcome  chan error
	attempts int
}

func (h *Host) register() {
	s := h.service
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.connected == nil {
		s.connected = make(map[string]*Host)
	}
	h.receipts = make(map[string]*receipt)
	s.connected[h.Name] = h
}
func (h *Host) unregister() {
	s := h.service
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.connected, h.Name)
	for id, r := range h.receipts {
		r.outcome <- fmt.Errorf("channel %q disconnected before receipt", h.Name)
		delete(h.receipts, id)
	}
}
func (h *Host) received(id string, err error, retry bool) {
	s := h.service
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := h.receipts[id]; r != nil {
		r.attempts++
		if retry && r.attempts < 3 {
			return
		}
		delete(h.receipts, id)
		r.outcome <- err
	}
}

// Deliver waits for the plugin's receipt for every part of the scheduled answer.
func (s *Service) Deliver(ctx context.Context, target schedule.Target, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	h := s.connected[target.Channel]
	if h == nil {
		s.mu.Unlock()
		return fmt.Errorf("channel %q is not running", target.Channel)
	}
	parts := answerParts(target.Chat, nil, text)
	outcomes := make(chan error, len(parts))
	for _, part := range parts {
		h.receipts[part.ID] = &receipt{outcome: outcomes}
	}
	err := h.ledger.record(outRecord{Deliver: &parts[0], Continuation: parts[1:]})
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, part := range parts {
			delete(h.receipts, part.ID)
		}
	}()
	if err != nil {
		return fmt.Errorf("queue scheduled delivery: %w", err)
	}
	for range parts {
		select {
		case err := <-outcomes:
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
