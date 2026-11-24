// Package learn records proposals and decisions for profile facts, project knowledge and coding skills.
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

type Store struct {
	Memory   *memory.Store
	Roots    []string
	Diagnose func(string)
}

type Proposal struct {
	Repo            string       `json:"repo,omitempty"`
	Skill           *Skill       `json:"skill,omitempty"`
	Measure         *Measurement `json:"measure,omitempty"`
	Draft           string       `json:"draft,omitempty"`
	SourceRuns      []string     `json:"source_runs,omitempty"`
	Version         string       `json:"version,omitempty"`
	Offer           *Offer       `json:"offer,omitempty"`
	DecisionInbound string       `json:"decision_inbound,omitempty"`
	ID              string       `json:"id"`
	Kind            string       `json:"kind"`
	Text            string       `json:"text"`
	Session         string       `json:"session"`
	Messages        []int        `json:"messages"`
	Outside         bool         `json:"outside"`
	Status          string       `json:"status"`
	Created         time.Time    `json:"created"`
	Decided         time.Time    `json:"decided"`
	FactID          string       `json:"fact_id"`
	Confirmed       time.Time    `json:"confirmed"`
}

type state struct {
	SkillReviewedRuns   map[string]bool   `json:"skill_reviewed_runs,omitempty"`
	SkillReviewFailures map[string]int    `json:"skill_review_failures,omitempty"`
	Proposals           []Proposal        `json:"proposals"`
	Reviewed            map[string]int    `json:"reviewed"`
	RunFailures         map[string]int    `json:"review_failures,omitempty"`
	ReviewedRuns        map[string]bool   `json:"reviewed_runs,omitempty"`
	KnowledgeBackoff    *knowledgeBackoff `json:"knowledge_backoff,omitempty"`
}

type knowledgeBackoff struct {
	Failures int       `json:"failures"`
	Next     time.Time `json:"next"`
}

const maxKnowledgeBackoff = 168 * time.Hour

func (b *knowledgeBackoff) waiting(now time.Time) bool {
	return b != nil && now.Before(b.Next) && !b.Next.After(now.Add(maxKnowledgeBackoff))
}

var randomID = regexp.MustCompile(`^[A-Z2-7]{26,}$`)

func (p *Proposal) UnmarshalJSON(data []byte) error {
	fields, err := objectWithOptional(data, []string{"repo", "skill", "measure", "offer", "decision_inbound", "draft", "source_runs", "version"}, "id", "kind", "text", "session", "messages", "outside", "status", "created", "decided", "fact_id", "confirmed")
	if err != nil {
		return err
	}
	if raw, ok := fields["offer"]; ok {
		if _, err := object(raw, "channel", "chat", "sender", "delivery_id", "expires"); err != nil {
			return err
		}
	}
	type plain Proposal
	if err := json.Unmarshal(data, (*plain)(p)); err != nil {
		return err
	}
	for _, key := range []string{"repo", "skill", "measure"} {
		if fields[key] != nil && p.Kind != "skill" {
			return fmt.Errorf("%s field on %s proposal", key, p.Kind)
		}
	}
	if p.Kind == "skill" && (fields["draft"] != nil || fields["version"] != nil || fields["decision_inbound"] != nil) {
		return fmt.Errorf("skill proposal contains unrelated fields")
	}
	p.Messages, err = messageIndices(fields["messages"])
	return err
}

func (p Proposal) validate(reviewed map[string]int) error {
	if p.Kind == "skill" {
		return p.validateSkill()
	}
	if p.Skill != nil || p.Measure != nil || p.Repo != "" {
		return fmt.Errorf("unexpected skill fields")
	}
	if p.Kind == "knowledge" {
		return p.validateKnowledge()
	}
	if p.Draft != "" || len(p.SourceRuns) != 0 || p.Version != "" {
		return fmt.Errorf("fact has knowledge fields")
	}
	count, exists := reviewed[p.Session]
	if !exists || count == 0 || !randomID.MatchString(p.ID) || p.Kind != "fact" || p.Created.IsZero() || len(p.Messages) == 0 {
		return fmt.Errorf("invalid proposal identity, kind, creation time or evidence")
	}
	if p.DecisionInbound != "" && (!memory.ValidIdentity(p.DecisionInbound) || p.Offer == nil || p.Status == "pending") {
		return fmt.Errorf("invalid learning decision inbound")
	}
	if p.Offer != nil && !p.Offer.valid() {
		return fmt.Errorf("invalid learning offer")
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
		return state{Proposals: []Proposal{}, Reviewed: map[string]int{}}, nil
	}
	if err != nil {
		return st, err
	}
	fields, err := objectWithOptional(data, []string{"skill_reviewed_runs", "skill_review_failures", "reviewed_runs", "run_failures", "review_failures", "knowledge_backoff"}, "proposals", "reviewed")
	if err != nil {
		return st, err
	}
	if _, err := object(fields["reviewed"]); err != nil {
		return st, err
	}
	for _, key := range []string{"skill_reviewed_runs", "skill_review_failures", "reviewed_runs", "run_failures", "review_failures"} {
		if raw, ok := fields[key]; ok {
			if _, err := object(raw); err != nil {
				return st, err
			}
		}
	}
	if raw, ok := fields["knowledge_backoff"]; ok {
		if _, err := object(raw, "failures", "next"); err != nil {
			return st, err
		}
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, err
	}
	if b := st.KnowledgeBackoff; b != nil && (b.Failures < 1 || b.Next.IsZero()) {
		return st, fmt.Errorf("invalid knowledge back-off")
	}
	for id, reviewed := range st.ReviewedRuns {
		if !runID.MatchString(id) || !reviewed {
			return st, fmt.Errorf("invalid reviewed run")
		}
	}
	for id, done := range st.SkillReviewedRuns {
		if !runID.MatchString(id) || !done {
			return st, fmt.Errorf("invalid reviewed skill run")
		}
	}
	for id, n := range st.SkillReviewFailures {
		if !runID.MatchString(id) || st.SkillReviewedRuns[id] || n < 1 || n >= maxRunFailures {
			return st, fmt.Errorf("invalid skill review failure count")
		}
	}
	legacy := fields["run_failures"] != nil
	if legacy {
		if st.RunFailures != nil {
			return st, fmt.Errorf("conflicting knowledge failure counts")
		}
		if err := json.Unmarshal(fields["run_failures"], &st.RunFailures); err != nil {
			return st, err
		}
	}
	for id, count := range st.RunFailures {
		if !runID.MatchString(id) || st.ReviewedRuns[id] || count < 0 || count >= maxRunFailures {
			return st, fmt.Errorf("invalid knowledge run failure count")
		}
		if legacy {
			// Older counts also included batch errors. Preserve retry order only.
			st.RunFailures[id] = 0
		}
	}
	for session, n := range st.Reviewed {
		if !memory.ValidSessionID(session) || n < 0 {
			return st, fmt.Errorf("invalid reviewed session or count")
		}
	}
	deliveries := map[string]bool{}
	ids := map[string]bool{}
	for _, p := range st.Proposals {
		if ids[p.ID] {
			return st, fmt.Errorf("duplicate proposal ID")
		}
		ids[p.ID] = true
		if p.Offer != nil {
			if deliveries[p.Offer.DeliveryID] {
				return st, fmt.Errorf("duplicate offer delivery ID")
			}
			deliveries[p.Offer.DeliveryID] = true
		}
		for _, run := range p.SourceRuns {
			reviewed := st.ReviewedRuns
			if p.Kind == "skill" {
				reviewed = st.SkillReviewedRuns
			}
			if !reviewed[run] {
				return st, fmt.Errorf("proposal cites an unreviewed run")
			}
		}
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
	note       string
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
		tx := transaction{state: st, root: root, store: s, user: user, name: name}
		if err := tx.recover(); err != nil {
			return err
		}
		if err := tx.recoverMeasures(); err != nil {
			return err
		}
		return fn(&tx)
	})
}

func (tx *transaction) save() error {
	for _, p := range tx.Proposals {
		if err := p.validate(tx.Reviewed); err != nil {
			return err
		}
	}
	return memory.WriteJSON(tx.root, tx.name, tx.state)
}

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
	p, _, err := s.DecideWithNote(user, id, action, outside, now)
	return p, err
}

func (s Store) DecideWithNote(user, id, action string, outside bool, now time.Time) (Proposal, string, error) {
	var result Proposal
	var note string
	err := s.withUser(user, func(tx *transaction) error {
		p, err := tx.find(id)
		if err != nil {
			return err
		}
		switch action {
		case "keep":
			if p.Status == "pending" && p.Outside && !outside {
				return fmt.Errorf("outside evidence requires -outside to keep this proposal")
			}
		case "reject", "rollback", "restore", "confirm":
		default:
			return fmt.Errorf("unknown learning decision %q", action)
		}
		if now.IsZero() {
			now = time.Now().UTC()
		}
		if err := tx.change(p, action, now); err != nil {
			return err
		}
		result = *p
		note = tx.note
		return nil
	})
	return result, note, err
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
	if p.Kind == "skill" {
		now = p.skillDecisionTime(now)
	}
	if now.Before(p.Created) || now.Before(p.Decided) {
		return fmt.Errorf("decision precedes proposal history")
	}
	now = now.UTC()
	if p.Kind == "skill" {
		return tx.changeSkill(p, action, now)
	}
	if p.Kind == "knowledge" {
		switch action {
		case "keep":
			return fmt.Errorf("read and sign the draft, run hrn knowledge import and hrn knowledge admit, then ada learn keep %s -version VERSION_ID", p.ID)
		case "reject":
			op := intent{ID: p.ID, Action: action, At: now}
			if err := memory.WriteJSON(tx.root, tx.journal(), op); err != nil {
				return err
			}
			return tx.rejectDraft(p, op)
		case "rollback":
			p.Status, p.Decided = step.to, now
			return tx.save()
		default:
			return fmt.Errorf("cannot %s project knowledge", action)
		}
	}
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
			if p.Kind != "fact" {
				continue
			}
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
