// Package learn keeps proposed profile facts separate from the user's approved profile.
package learn

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"time"

	"github.com/cristiangirlea/adanext-ada/memory"
)

type Store struct{ Memory *memory.Store }

type Proposal struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Text      string    `json:"text"`
	Session   string    `json:"session"`
	Messages  []int     `json:"messages"`
	Outside   bool      `json:"outside"`
	Status    string    `json:"status"`
	Created   time.Time `json:"created"`
	Decided   time.Time `json:"decided"`
	FactID    string    `json:"fact_id"`
	Confirmed time.Time `json:"confirmed"`
}

type state struct {
	Proposals []Proposal     `json:"proposals"`
	Reviewed  map[string]int `json:"reviewed"`
}

var randomID = regexp.MustCompile(`^[A-Z2-7]{26,}$`)

func (p *Proposal) UnmarshalJSON(data []byte) error {
	fields, err := object(data, "id", "kind", "text", "session", "messages", "outside", "status", "created", "decided", "fact_id", "confirmed")
	if err != nil {
		return err
	}
	type plain Proposal
	if err := json.Unmarshal(data, (*plain)(p)); err != nil {
		return err
	}
	p.Messages, err = messageIndices(fields["messages"])
	return err
}

func (p Proposal) validate(reviewed map[string]int) error {
	count, exists := reviewed[p.Session]
	if !exists || count == 0 || !randomID.MatchString(p.ID) || p.Kind != "fact" || p.Created.IsZero() || len(p.Messages) == 0 {
		return fmt.Errorf("invalid proposal identity, kind, creation time or evidence")
	}
	if err := memory.ValidateFactText(p.Text); err != nil {
		return err
	}
	for _, i := range p.Messages {
		if !p.Outside && (i < 0 || i >= count) {
			return fmt.Errorf("inside proposal cites missing evidence")
		}
	}
	switch p.Status {
	case "pending":
		if !p.Decided.IsZero() {
			return fmt.Errorf("pending proposal has a decision")
		}
	case "kept", "stale", "archived", "rolled_back", "rejected":
		if p.Decided.IsZero() || p.Decided.Before(p.Created) {
			return fmt.Errorf("invalid proposal decision time")
		}
	default:
		return fmt.Errorf("invalid proposal status %q", p.Status)
	}
	if p.Status == "pending" || p.Status == "rejected" {
		if p.FactID != "" || !p.Confirmed.IsZero() {
			return fmt.Errorf("unkept proposal has a profile fact or confirmation")
		}
	} else if !randomID.MatchString(p.FactID) || p.Confirmed.Before(p.Created) || p.Confirmed.After(p.Decided) {
		return fmt.Errorf("invalid proposal fact ID or confirmation")
	}
	return nil
}

func load(root *os.Root, name string) (state, error) {
	var st state
	data, err := root.ReadFile(name)
	if errors.Is(err, os.ErrNotExist) {
		return state{[]Proposal{}, map[string]int{}}, nil
	}
	if err != nil {
		return st, err
	}
	fields, err := object(data, "proposals", "reviewed")
	if err != nil {
		return st, err
	}
	if _, err := object(fields["reviewed"]); err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, err
	}
	for session, n := range st.Reviewed {
		if !memory.ValidSessionID(session) || n < 0 {
			return st, fmt.Errorf("invalid reviewed session or count")
		}
	}
	ids := map[string]bool{}
	for _, p := range st.Proposals {
		if ids[p.ID] {
			return st, fmt.Errorf("duplicate proposal ID")
		}
		ids[p.ID] = true
		if err := p.validate(st.Reviewed); err != nil {
			return st, err
		}
	}
	return st, nil
}

type transaction struct {
	state
	root       *os.Root
	store      Store
	user, name string
}

func (s Store) withUser(user string, fn func(*transaction) error) error {
	name, err := memory.UserFilename(user)
	if err != nil {
		return err
	}
	return s.Memory.WithLearningFiles(func(root *os.Root) error {
		st, err := load(root, name)
		if err != nil {
			return fmt.Errorf("read learning file: %w", err)
		}
		tx := transaction{st, root, s, user, name}
		if err := tx.recover(); err != nil {
			return err
		}
		return fn(&tx)
	})
}

func (tx *transaction) save() error { return memory.WriteJSON(tx.root, tx.name, tx.state) }

func (tx *transaction) find(id string) (*Proposal, error) {
	for i := range tx.Proposals {
		if tx.Proposals[i].ID == id {
			return &tx.Proposals[i], nil
		}
	}
	return nil, fmt.Errorf("unknown proposal ID %q", id)
}

func (s Store) List(user string, all bool) ([]Proposal, error) {
	var result []Proposal
	err := s.withUser(user, func(tx *transaction) error {
		for _, p := range tx.Proposals {
			if all || p.Status == "pending" {
				result = append(result, p)
			}
		}
		return nil
	})
	return result, err
}

func (s Store) Decide(user, id, action string, outside bool, now time.Time) (Proposal, error) {
	var result Proposal
	err := s.withUser(user, func(tx *transaction) error {
		p, err := tx.find(id)
		if err != nil {
			return err
		}
		switch action {
		case "keep":
			if p.Status == "pending" && p.Outside && !outside {
				return fmt.Errorf("outside evidence requires -outside to keep this fact")
			}
		case "reject", "rollback", "restore", "confirm":
		default:
			return fmt.Errorf("unknown learning decision %q", action)
		}
		if err := tx.change(p, action, now); err != nil {
			return err
		}
		result = *p
		return nil
	})
	return result, err
}

type transition struct {
	from []string
	to   string
}

var transitions = map[string]transition{
	"keep":     {[]string{"pending"}, "kept"},
	"reject":   {[]string{"pending"}, "rejected"},
	"rollback": {[]string{"kept", "stale", "archived"}, "rolled_back"},
	"confirm":  {[]string{"kept", "stale"}, "kept"},
	"restore":  {[]string{"archived"}, "kept"},
	"stale":    {[]string{"kept"}, "stale"},
	"archive":  {[]string{"stale"}, "archived"},
}

func (tx *transaction) change(p *Proposal, action string, now time.Time) error {
	step := transitions[action]
	if !slices.Contains(step.from, p.Status) {
		return fmt.Errorf("cannot %s a %s proposal", action, p.Status)
	}
	if now.Before(p.Created) || now.Before(p.Decided) {
		return fmt.Errorf("decision precedes proposal history")
	}
	now = now.UTC()
	switch action {
	case "keep", "restore", "rollback", "archive":
		return tx.changeProfile(p, action, now)
	case "confirm":
		p.Confirmed = now
	}
	p.Status, p.Decided = step.to, now
	return tx.save()
}

func (s Store) Sweep(user string, now time.Time, staleAfter, archiveAfter time.Duration) ([]Proposal, error) {
	if staleAfter <= 0 || archiveAfter <= 0 {
		return nil, fmt.Errorf("learning durations must be positive")
	}
	var changed []Proposal
	err := s.withUser(user, func(tx *transaction) error {
		for i := range tx.Proposals {
			p := &tx.Proposals[i]
			staleAt := p.Confirmed.Add(staleAfter)
			if p.Status == "kept" && now.After(staleAt) {
				if err := tx.change(p, "stale", staleAt); err != nil {
					return err
				}
				changed = append(changed, *p)
			}
			if p.Status == "stale" && now.After(p.Decided.Add(archiveAfter)) {
				if err := tx.change(p, "archive", now); err != nil {
					return err
				}
				changed = append(changed, *p)
			}
		}
		return nil
	})
	return changed, err
}
