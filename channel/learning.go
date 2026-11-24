package channel

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/cristiangirlea/adanext-ada/learn"
	"github.com/cristiangirlea/adanext-ada/memory"
	p "github.com/cristiangirlea/harness/contracts/pluginproto"
)

// The outbox carries enough information to finish binding an offer after a crash.
type offerTarget struct {
	User     string    `json:"user"`
	Proposal string    `json:"proposal"`
	Sender   string    `json:"sender"`
	Expires  time.Time `json:"expires"`
}

func (s *Service) runLearning(ctx context.Context) {
	review := s.Knowledge.WithDiagnostics(func(message string) { s.status("learning", message) })
	if !review.Enabled() {
		s.status("learning", learn.ErrKnowledgeDisabled.Error())
	}
	timer := time.NewTimer(s.ProposeEvery)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := s.proposeRecent(ctx, review); err != nil && ctx.Err() == nil {
				s.status("learning", err.Error())
			}
			timer.Reset(s.ProposeEvery)
		}
	}
}

func (s *Service) proposeRecent(ctx context.Context, review learn.KnowledgeReview) error {
	users, err := s.Store.LinkedUsers(s.Owner)
	if err != nil {
		return err
	}
	store := learn.Store{Memory: s.Store}
	var failures []error
	for _, user := range users {
		if err := ctx.Err(); err != nil {
			return err
		}
		review, cancel := context.WithTimeout(ctx, s.Loop.Budget.WithDefaults().Timeout)
		items, err := store.ProposeRecent(review, s.Loop.Provider, user, time.Now(), s.Idle, 24*time.Hour)
		cancel()
		if err != nil {
			failures = append(failures, fmt.Errorf("review %q: %w", user, err))
		}
		for _, item := range items {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := s.offer(user, item); err != nil {
				failures = append(failures, fmt.Errorf("offer %s: %w", item.ID, err))
			}
		}
	}
	if review.Enabled() {
		_, err := learn.ProposeKnowledge(ctx, store, s.Owner, review)
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (s *Service) offer(user string, item learn.Proposal) error {
	if item.Kind != "fact" {
		return nil
	}
	origin, err := s.Store.LatestOrigin(user, item.Session)
	if err != nil {
		return err
	}
	if !origin.Private {
		return nil
	}
	if current, ok := s.Store.Resolve(origin.Channel, origin.Sender); !ok || current != user {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.hosts[origin.Channel]
	if h == nil || h.Groups {
		return nil
	}
	keep := "Keep"
	text := fmt.Sprintf("Remember this about you? \"%s\"", item.Text)
	if item.Outside {
		keep = "Keep anyway"
		text += "\nIt came from a conversation that included content from outside (a tool, a file, a page or another sender)."
	}
	d := p.Deliver{Type: p.TypeDeliver, ID: rand.Text()[:12], ChatID: origin.Chat, Text: promptText(text), Buttons: []p.Button{{ID: "keep", Label: keep}, {ID: "reject", Label: "Forget it"}}}
	target := offerTarget{User: user, Proposal: item.ID, Sender: origin.Sender, Expires: time.Now().UTC().Add(s.OfferTTL)}
	return h.ledger.record(outRecord{Deliver: &d, Offer: &target})
}

func (h *Host) prepareDelivery(d pending) (bool, error) {
	target := d.offer
	if target == nil {
		return true, nil
	}
	if !h.Groups && h.service.Decision != nil {
		store := learn.Store{Memory: h.service.Store}
		offer := learn.Offer{Channel: h.Name, Chat: d.ChatID, Sender: target.Sender, DeliveryID: d.ID, Expires: target.Expires}
		allowed, err := store.BindOffer(target.User, target.Proposal, offer)
		if err != nil {
			h.service.status("learning", fmt.Sprintf("bind offer %s: %v", target.Proposal, err))
		}
		if allowed {
			return true, nil
		}
	}
	failure := p.DeliveryFailed{ID: d.ID, Error: p.Problem{Code: p.CodeUnauthorised, Message: "learning offer is no longer eligible"}}
	return false, h.ledger.record(outRecord{Failed: &failure})
}

func (h *Host) awaitingOffer(in *p.Inbound, user string) (bool, error) {
	items, err := (learn.Store{Memory: h.service.Store}).List(user, false)
	if err != nil {
		return false, err
	}
	for _, item := range items {
		if o := item.Offer; o != nil && o.Channel == h.Name && o.Chat == in.Chat.ID && o.Sender == in.Sender.ID && time.Now().Before(o.Expires) {
			return true, nil
		}
	}
	return false, nil
}

func (h *Host) decideOffer(in *p.Inbound, user string, replay bool) error {
	origin := memory.Origin{Channel: h.Name, Chat: in.Chat.ID, Sender: in.Sender.ID}
	decided, err := h.service.Decision(user, learn.ButtonPress{Origin: origin, DeliveryID: in.Action.DeliveryID, Button: in.Action.ButtonID, InboundID: in.ID, Replay: replay})
	if err != nil {
		h.service.status(h.Name, fmt.Sprintf("learning decision: %v", err))
		if refusal, ok := errors.AsType[*memory.FactRefusal](err); ok && in.Action.ButtonID == "keep" {
			return h.answer(in, promptText("Could not keep it: "+refusal.Reason))
		}
		return nil
	}
	if !decided {
		return nil
	}
	text := "Forgotten."
	if in.Action.ButtonID == "keep" {
		text = "Kept."
	}
	return h.answer(in, text)
}
