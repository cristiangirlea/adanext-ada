package channel

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cristiangirlea/adanext-ada/approval"
	"github.com/cristiangirlea/adanext-ada/loop"
	"github.com/cristiangirlea/adanext-ada/memory"
	"github.com/cristiangirlea/adanext-ada/provider"
	p "github.com/cristiangirlea/harness/contracts/pluginproto"
)

type Service struct {
	Store      *memory.Store
	Loop       *loop.Loop
	Owner      string
	Idle       time.Duration
	History    int
	WithoutEnv []string
	Status     func(string)
	mu         sync.Mutex
	users      map[string]*queue
	workers    sync.WaitGroup
}

func (s *Service) status(name, message string) {
	if s.Status != nil {
		s.Status(name + " " + message)
	}
}

type job struct {
	ctx    context.Context
	host   *Host
	in     *p.Inbound
	replay bool
}
type queue struct {
	jobs   []job
	cancel context.CancelCauseFunc
}

var errStopped = errors.New("stopped by user")

func (s *Service) enqueue(ctx context.Context, user string, j job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j.ctx = ctx
	if q := s.users[user]; q != nil {
		q.jobs = append(q.jobs, j)
		return
	}
	turn, cancel := context.WithCancelCause(ctx)
	q := &queue{cancel: cancel}
	s.users[user] = q
	s.workers.Go(func() {
		for {
			err := j.host.turn(turn, user, j.in, j.replay)
			// An explicit /stop completes this turn. Service shutdown leaves it for replay.
			stopped := errors.Is(err, context.Canceled) && context.Cause(turn) == errStopped
			cancel(nil)
			if err == nil || stopped {
				err = j.host.ledger.processed(j.in.ID)
			}
			if err != nil && j.ctx.Err() == nil {
				j.host.fail(fmt.Errorf("turn: %w", err))
			}
			s.mu.Lock()
			if len(q.jobs) == 0 {
				delete(s.users, user)
				s.mu.Unlock()
				return
			}
			j = q.jobs[0]
			q.jobs[0] = job{}
			q.jobs = q.jobs[1:]
			turn, cancel = context.WithCancelCause(j.ctx)
			q.cancel = cancel
			s.mu.Unlock()
		}
	})
}
func cleanText(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return utf8.RuneError
		}
		return r
	}, text)
}
func prefixBytes(text string, end int) string {
	for !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end]
}

// Preserve the whole model answer across the wire text and UTF-8 boundaries.
func (h *Host) answer(in *p.Inbound, text string) error {
	delivery := func(part string) p.Deliver {
		return p.Deliver{Type: p.TypeDeliver, ID: rand.Text()[:12], ChatID: in.Chat.ID, ReplyTo: &in.ID, Text: part}
	}
	text = cleanText(text)
	var parts []p.Deliver
	for len(text) > p.MaxEventText {
		chunk := prefixBytes(text, p.MaxEventText)
		parts = append(parts, delivery(chunk))
		text = text[len(chunk):]
	}
	parts = append(parts, delivery(text))
	// Commit every part with the completion evidence before exposing any part to the plugin.
	return h.ledger.record(outRecord{Deliver: &parts[0], Continuation: parts[1:], Completes: in.ID})
}

func (h *Host) receive(ctx context.Context, in *p.Inbound, replay bool) error {
	if kind := in.Chat.Type; !h.Groups && (kind == nil || *kind != p.ChatPrivate) {
		return h.ledger.processed(in.ID)
	}
	s := h.service
	user, paired := s.Store.Resolve(h.Name, in.Sender.ID)
	var err error
	switch {
	case !paired:
		if !replay {
			var code string
			code, err = s.Store.OfferPairing(memory.Pairing{Channel: h.Name, Sender: in.Sender.ID, Chat: in.Chat.ID, Name: in.Sender.Name})
			if err == nil && code != "" {
				err = h.answer(in, "To talk to Ada, run: ada pair "+code+" on your computer.")
			}
		}
	case in.Action != nil:
		h.action(in, user)
	case in.Text == "":
	case in.Text == "/stop":
		message := "Nothing is running."
		s.mu.Lock()
		if q := s.users[user]; q != nil {
			q.cancel(errStopped)
			message = "Stopped."
		}
		s.mu.Unlock()
		err = h.answer(in, message)
	case (strings.EqualFold(in.Text, "approve") || strings.EqualFold(in.Text, "deny")) && h.awaiting(in, user):
	default:
		s.enqueue(ctx, user, job{host: h, in: in, replay: replay})
		return nil
	}
	if err != nil {
		return err
	}
	return h.ledger.processed(in.ID)
}
func (h *Host) turn(ctx context.Context, user string, in *p.Inbound, replay bool) error {
	s := h.service
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if current, ok := s.Store.Resolve(h.Name, in.Sender.ID); !ok || user != current {
		return nil
	}
	if in.Text == "/new" {
		if _, err := s.Store.NewSessionForInbound(user, h.Name, in.Chat.ID, in.Sender.ID, in.ID); err != nil {
			return err
		}
		return h.answer(in, "Started a new session.")
	}
	// Only a recovered message can have an exchange saved before its answer was recorded.
	if replay {
		saved, found, err := s.Store.AnswerForInbound(user, memory.InboundRef{Channel: h.Name, ID: in.ID})
		if err != nil {
			return err
		}
		if found {
			return h.answer(in, saved)
		}
	}
	session, err := s.Store.Open(user, h.Name, in.Chat.ID, in.Sender.ID, s.Idle)
	if err != nil {
		return err
	}
	history, err := s.Store.History(session.ID, s.History)
	if err != nil {
		return err
	}
	ctx = approval.WithSession(memory.WithUser(ctx, user), session.ID)
	ctx = approval.WithApprover(ctx, &ChannelApprover{host: h, in: in, user: user})
	answer, next, err := s.Loop.Turn(ctx, history, in.Text)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		if sendErr := h.answer(in, "Ada could not complete this turn."); sendErr != nil {
			return sendErr
		}
		s.status(h.Name, fmt.Sprintf("conversation turn: %v", err))
		return nil
	}
	// A budget stop can replace the provider's final text with a diagnostic.
	last := &next[len(next)-1]
	if last.Text() != answer {
		last.Content = []provider.Block{{Type: "text", Text: answer}}
	}
	if err := s.Store.AppendInbound(session.ID, h.Name, in.Chat.ID, in.Sender.ID, in.ID, next[len(history):]...); err != nil {
		return err
	}
	return h.answer(in, answer)
}
