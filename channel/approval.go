package channel

import (
	"context"
	"crypto/rand"
	"time"

	"github.com/cristiangirlea/adanext-ada/approval"
	p "github.com/cristiangirlea/harness/contracts/pluginproto"
)

type ChannelApprover struct {
	host *Host
	in   *p.Inbound
	user string
}
type prompt struct {
	user, chat, sender string
	decision           chan bool
	expires            time.Time
}

func (a *ChannelApprover) SurfaceName() string { return a.host.Name }
func (a *ChannelApprover) Ask(ctx context.Context, r approval.Request) (approval.Decision, error) {
	h := a.host
	d := approval.Decision{By: h.Name}
	if r.User != a.user || ctx.Err() != nil {
		return d, ctx.Err()
	}
	id := rand.Text()[:12]
	wait := prompt{user: a.user, chat: a.in.Chat.ID, sender: a.in.Sender.ID, decision: make(chan bool, 1), expires: r.Expires}
	h.mu.Lock()
	if h.prompts == nil {
		h.prompts = make(map[string]prompt)
	}
	h.prompts[id] = wait
	h.mu.Unlock()
	defer func() { h.mu.Lock(); delete(h.prompts, id); h.mu.Unlock() }()
	summary := promptText(r.Summary)
	delivery := p.Deliver{Type: p.TypeDeliver, ID: id, ChatID: wait.chat, ReplyTo: &a.in.ID, Text: summary, Buttons: []p.Button{{ID: "approve", Label: "Approve"}, {ID: "deny", Label: "Deny"}}}
	err := h.ledger.record(outRecord{Deliver: &delivery})
	if err != nil {
		return d, err
	}
	select {
	case d.Approved = <-wait.decision:
	case <-ctx.Done():
	}
	return d, nil
}
func promptText(text string) string {
	const marker = " [truncated]"
	text = cleanText(text)
	if len(text) > p.MaxEventText {
		return prefixBytes(text, p.MaxEventText-len(marker)) + marker
	}
	return text
}
func (w prompt) matches(in *p.Inbound, user string) bool {
	return w.user == user && w.chat == in.Chat.ID && w.sender == in.Sender.ID && time.Now().Before(w.expires)
}
func (h *Host) awaiting(in *p.Inbound, user string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, w := range h.prompts {
		if w.matches(in, user) {
			return true
		}
	}
	return false
}
func (h *Host) action(in *p.Inbound, user string, replay bool) error {
	a := in.Action
	h.mu.Lock()
	w, ok := h.prompts[a.DeliveryID]
	if ok && w.matches(in, user) && (a.ButtonID == "approve" || a.ButtonID == "deny") {
		delete(h.prompts, a.DeliveryID)
		w.decision <- a.ButtonID == "approve"
	}
	h.mu.Unlock()
	if ok || h.service.Decision == nil {
		return nil
	}
	return h.decideOffer(in, user, replay)
}
