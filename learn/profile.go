package learn

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/cristiangirlea/adanext-ada/memory"
)

// The intent bridges the two atomic renames when a process exits between them.
type intent struct {
	DecisionInbound string    `json:"decision_inbound,omitempty"`
	ID              string    `json:"id"`
	Action          string    `json:"action"`
	At              time.Time `json:"at"`
}

func source(p *Proposal) string         { return "learn:" + p.ID + " session:" + p.Session }
func (tx *transaction) journal() string { return ".intent-" + tx.name }

func (tx *transaction) changeProfile(p *Proposal, action string, now time.Time) error {
	op := intent{ID: p.ID, Action: action, At: now}
	if action == "keep" {
		op.DecisionInbound = p.DecisionInbound
	}
	if err := memory.WriteJSON(tx.root, tx.journal(), op); err != nil {
		return err
	}
	var err error
	if action == "keep" || action == "restore" {
		var f memory.Fact
		f, err = tx.store.Memory.AddFact(tx.user, p.Text, source(p))
		if err == nil {
			p.FactID = f.ID
		}
	} else {
		err = tx.store.Memory.RemoveSourcedFact(tx.user, p.FactID, source(p))
	}
	if err != nil {
		return errors.Join(err, tx.root.Remove(tx.journal()))
	}
	return tx.finish(p, op)
}

func (tx *transaction) finish(p *Proposal, op intent) error {
	if op.DecisionInbound != "" {
		p.DecisionInbound = op.DecisionInbound
	}
	p.Status, p.Decided = transitions[op.Action].to, op.At
	if p.Status == "kept" {
		p.Confirmed = op.At
	}
	if err := tx.save(); err != nil {
		return err
	}
	return tx.root.Remove(tx.journal())
}

func (tx *transaction) recover() error {
	data, err := tx.root.ReadFile(tx.journal())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := objectWithOptional(data, []string{"decision_inbound"}, "id", "action", "at"); err != nil {
		return err
	}
	var op intent
	if err := json.Unmarshal(data, &op); err != nil {
		return err
	}
	p, err := tx.find(op.ID)
	if err != nil {
		return err
	}
	if op.DecisionInbound != "" && (!memory.ValidIdentity(op.DecisionInbound) || op.Action != "keep" || p.Offer == nil || !op.At.Before(p.Offer.Expires)) {
		return fmt.Errorf("learning intent conflicts with offer")
	}
	if p.Kind == "knowledge" {
		if op.Action != "reject" || op.At.Before(p.Created) || (p.Status != "pending" && !(p.Status == "rejected" && p.Decided.Equal(op.At))) {
			return fmt.Errorf("learning intent conflicts with knowledge proposal")
		}
		return tx.rejectDraft(p, op)
	}
	switch op.Action {
	case "keep", "restore", "archive", "rollback":
	default:
		return fmt.Errorf("invalid profile change in learning intent")
	}
	step := transitions[op.Action]
	if p.Status == step.to && p.Decided.Equal(op.At) {
		return tx.root.Remove(tx.journal())
	}
	if !slices.Contains(step.from, p.Status) || op.At.Before(p.Created) || op.At.Before(p.Decided) {
		return fmt.Errorf("learning intent conflicts with proposal")
	}
	facts, err := tx.store.Memory.ReadFacts(tx.user)
	if err != nil {
		return err
	}
	add := op.Action == "keep" || op.Action == "restore"
	for _, f := range facts {
		if add && f.Source == source(p) {
			if f.Text != p.Text || !randomID.MatchString(f.ID) {
				return fmt.Errorf("learning intent conflicts with profile")
			}
			p.FactID = f.ID
			return tx.finish(p, op)
		}
		if !add && f.ID == p.FactID {
			if f.Source != source(p) {
				return fmt.Errorf("learning intent conflicts with profile")
			}
			return tx.root.Remove(tx.journal())
		}
	}
	if add {
		return tx.root.Remove(tx.journal())
	}
	return tx.finish(p, op)
}
