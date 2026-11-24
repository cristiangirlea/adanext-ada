package learn

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cristiangirlea/adanext-ada/hrnexec"
	"github.com/cristiangirlea/adanext-ada/memory"
)

type KnowledgeReview struct {
	Scope       hrnexec.KnowledgeScope
	Backend     string
	Options     hrnexec.Options
	Timeout     time.Duration
	SkipBackoff bool
	diagnostics *knowledgeDiagnostics
}

type knowledgeDiagnostics struct {
	write   func(string)
	pending string
}

// WithDiagnostics reports a pending draft once over the lifetime of this review schedule.
func (k KnowledgeReview) WithDiagnostics(write func(string)) KnowledgeReview {
	k.diagnostics = &knowledgeDiagnostics{write: write}
	return k
}

var ErrKnowledgeDisabled = errors.New("knowledge review disabled: configure all five knowledge fields and set learning.knowledge_backend or hrn.backend to claude or codex")

func (k KnowledgeReview) Enabled() bool {
	s := k.Scope
	for _, value := range []string{s.TenantID, s.ProjectID, s.Backend, s.Model, s.Revision} {
		if strings.TrimSpace(value) == "" {
			return false
		}
	}
	return k.Backend == "claude" || k.Backend == "codex"
}

func (k KnowledgeReview) timeout() time.Duration {
	if k.Timeout > 0 {
		return k.Timeout
	}
	return 30 * time.Minute
}

func draftPath(id string) string { return "learning/knowledge/" + id + ".json" }

func (p Proposal) validateKnowledge() error {
	if !randomID.MatchString(p.ID) || p.Draft != draftPath(p.ID) || len(p.SourceRuns) == 0 || len(p.SourceRuns) > 10 || p.Created.IsZero() {
		return fmt.Errorf("invalid knowledge proposal identity, draft or source runs")
	}
	for i, id := range p.SourceRuns {
		if !runID.MatchString(id) || slices.Contains(p.SourceRuns[:i], id) {
			return fmt.Errorf("invalid knowledge source run")
		}
	}
	if p.Session != "" || len(p.Messages) != 0 || p.Offer != nil || p.Outside || p.DecisionInbound != "" || p.FactID != "" || !p.Confirmed.IsZero() {
		return fmt.Errorf("knowledge proposal contains fact evidence or a chat offer")
	}
	switch p.Status {
	case "pending", "rejected":
		if p.Version != "" {
			return fmt.Errorf("unkept knowledge proposal has a version")
		}
	case "kept", "rolled_back":
		if !memory.ValidIdentity(p.Version) {
			return fmt.Errorf("invalid knowledge version ID")
		}
	default:
		return fmt.Errorf("invalid knowledge proposal status")
	}
	if (p.Status == "pending") != p.Decided.IsZero() || !p.Decided.IsZero() && p.Decided.Before(p.Created) {
		return fmt.Errorf("invalid knowledge decision time")
	}
	return memory.ValidateFactText(p.Text)
}

func checkKnowledgeDir(root *os.Root) error {
	info, err := root.Lstat("knowledge")
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("knowledge draft directory must be a directory, not a link")
	}
	return nil
}

func draftKeys(root *os.Root, id string, scope hrnexec.KnowledgeScope, runs []string, unsigned bool) ([]string, error) {
	if err := checkKnowledgeDir(root); err != nil {
		return nil, err
	}
	path := filepath.Join("knowledge", id+".json")
	info, err := root.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("knowledge draft must be a regular file")
	}
	f, err := root.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	const limit = 8 << 20
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("knowledge draft exceeds 8 MiB")
	}
	if _, err := object(data, "tenant_id", "project_id", "backend", "model", "revision", "origin", "reviewed_by", "review_evidence", "entries"); err != nil {
		return nil, fmt.Errorf("invalid knowledge draft: %w", err)
	}
	var d hrnexec.KnowledgeDraft
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, err
	}
	if d.KnowledgeScope != scope {
		return nil, fmt.Errorf("knowledge draft scope differs from the configured scope")
	}
	if unsigned && (d.Origin != "" || d.ReviewedBy != "" || d.ReviewEvidence != "") {
		return nil, fmt.Errorf("knowledge draft review fields must be empty")
	}
	var keys []string
	for _, e := range d.Entries {
		if e.Key == "" {
			return nil, fmt.Errorf("missing knowledge entry key")
		}
		prefix, rest, _ := strings.Cut(e.Key, "/")
		run, slug, _ := strings.Cut(rest, "/")
		if prefix == "learned" && slug != "" && slices.Contains(runs, run) {
			keys = append(keys, e.Key)
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("knowledge draft has no new learned entries from the reviewed runs")
	}
	return keys, nil
}

func ProposeKnowledge(ctx context.Context, s Store, owner string, k KnowledgeReview) ([]Proposal, error) {
	if !k.Enabled() {
		return nil, ErrKnowledgeDisabled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var result []Proposal
	err := s.Memory.WithLearningReview(func() (reviewErr error) {
		var runs []string
		var drafts *os.Root
		var output string
		id := rand.Text()
		timeout := k.timeout()
		err := s.withUser(owner, func(tx *transaction) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if b := tx.KnowledgeBackoff; !k.SkipBackoff && b.waiting(time.Now()) {
				k.diagnose("next knowledge review attempt at %s", b.Next.Format(time.RFC3339Nano))
				return nil
			}
			for _, p := range tx.Proposals {
				if p.Kind == "knowledge" && p.Status == "pending" {
					if d := k.diagnostics; d != nil && d.pending != p.ID {
						d.pending = p.ID
						k.diagnose("knowledge review paused for pending proposal %s", p.ID)
					}
					return nil
				}
			}
			var err error
			batchSize := min(10, max(1, int(timeout/(10*time.Minute))))
			runs, err = pendingDelegations(tx.root, tx.ReviewedRuns, tx.RunFailures, batchSize)
			if err != nil {
				return k.saveReview(ctx, tx, err, nil)
			}
			if len(runs) == 0 {
				return nil
			}
			if err := tx.root.MkdirAll("knowledge", 0700); err != nil {
				return k.saveReview(ctx, tx, err, nil)
			}
			// hrn receives a filesystem path and cannot use Ada's rooted descriptor.
			if err := checkKnowledgeDir(tx.root); err != nil {
				return k.saveReview(ctx, tx, err, nil)
			}
			output, err = filepath.Abs(filepath.Join(tx.root.Name(), "knowledge", id+".json"))
			if err != nil {
				return k.saveReview(ctx, tx, err, nil)
			}
			drafts, err = tx.root.OpenRoot("knowledge")
			if err != nil {
				return k.saveReview(ctx, tx, err, nil)
			}
			return nil
		})
		if err != nil || len(runs) == 0 {
			return err
		}
		defer drafts.Close()
		// On failure clean up only in the directory selected before launching hrn.
		defer func() {
			if reviewErr != nil {
				if err := drafts.Remove(id + ".json"); err != nil && !errors.Is(err, os.ErrNotExist) {
					reviewErr = errors.Join(reviewErr, err)
				}
			}
		}()
		report, callErr := hrnexec.ProposeKnowledge(ctx, k.Options, timeout, k.Scope, k.Backend, runs, output)
		return s.withUser(owner, func(tx *transaction) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if callErr != nil {
				return k.saveReview(ctx, tx, callErr, nil)
			}
			for _, f := range report.Failed {
				if f.ReviewRun != "" && !runID.MatchString(f.ReviewRun) {
					return k.saveReview(ctx, tx, fmt.Errorf("invalid review run ID for %s", f.Run), nil)
				}
			}
			var draftErr error
			result, draftErr = tx.recordKnowledgeDraft(id, report, k.Scope)
			retired := k.applyOutcomes(tx, report, draftErr == nil)
			return k.saveReview(ctx, tx, draftErr, retired)
		})
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (k KnowledgeReview) diagnose(format string, args ...any) {
	if k.diagnostics != nil && k.diagnostics.write != nil {
		k.diagnostics.write(fmt.Sprintf(format, args...))
	}
}

const maxRunFailures = 3

func (tx *transaction) markRunReviewed(id string) {
	if tx.ReviewedRuns == nil {
		tx.ReviewedRuns = map[string]bool{}
	}
	tx.ReviewedRuns[id] = true
	delete(tx.RunFailures, id)
}

func (k KnowledgeReview) applyOutcomes(tx *transaction, report hrnexec.KnowledgeProposal, draftAccepted bool) []string {
	var retired []string
	if tx.RunFailures == nil {
		tx.RunFailures = map[string]int{}
	}
	for _, run := range report.Reviewed {
		if draftAccepted {
			tx.markRunReviewed(run)
		} else {
			tx.RunFailures[run] = 0
		}
	}
	for _, skipped := range report.Skipped {
		tx.markRunReviewed(skipped.Run)
	}
	for _, deferred := range report.Deferred {
		tx.RunFailures[deferred.Run] = 0
	}
	for _, f := range report.Failed {
		k.diagnose("knowledge review failed for run %s (review run %q)", f.Run, f.ReviewRun)
		tx.RunFailures[f.Run]++
		if tx.RunFailures[f.Run] == maxRunFailures {
			tx.markRunReviewed(f.Run)
			retired = append(retired, f.Run)
		}
	}
	return retired
}

func (k KnowledgeReview) saveReview(ctx context.Context, tx *transaction, cause error, retired []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cause == nil {
		tx.KnowledgeBackoff = nil
	} else {
		failures := 1
		if prev := tx.KnowledgeBackoff; prev != nil {
			failures = min(prev.Failures, math.MaxInt-1) + 1
		}
		delay := min(time.Hour<<min(failures, 8), maxKnowledgeBackoff)
		tx.KnowledgeBackoff = &knowledgeBackoff{Failures: failures, Next: time.Now().UTC().Add(delay)}
	}
	if err := tx.save(); err != nil {
		return errors.Join(cause, err)
	}
	if len(retired) > 0 {
		k.diagnose("marked runs reviewed after %d consecutive failures: %s", maxRunFailures, strings.Join(retired, ", "))
	}
	if b := tx.KnowledgeBackoff; b != nil {
		k.diagnose("next knowledge review attempt at %s", b.Next.Format(time.RFC3339Nano))
	}
	return cause
}

func (tx *transaction) recordKnowledgeDraft(id string, report hrnexec.KnowledgeProposal, scope hrnexec.KnowledgeScope) ([]Proposal, error) {
	if report.Draft == "" {
		_, err := tx.root.Lstat(filepath.Join("knowledge", id+".json"))
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("hrn wrote an unreported knowledge draft")
	}
	keys, err := draftKeys(tx.root, id, scope, report.Reviewed, true)
	if err != nil {
		return nil, err
	}
	p := Proposal{
		ID: id, Kind: "knowledge", Status: "pending", Created: time.Now().UTC(),
		Text:  fmt.Sprintf("%d knowledge entries from runs %s", len(keys), strings.Join(report.Reviewed, ", ")),
		Draft: draftPath(id), SourceRuns: report.Reviewed, Messages: []int{},
	}
	if err := p.validateKnowledge(); err != nil {
		return nil, err
	}
	tx.Proposals = append(tx.Proposals, p)
	return []Proposal{p}, nil
}

func (tx *transaction) rejectDraft(p *Proposal, op intent) error {
	if err := checkKnowledgeDir(tx.root); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := tx.root.Remove(filepath.Join("knowledge", p.ID+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return tx.finish(p, op)
}

func (s Store) KeepKnowledge(ctx context.Context, owner, id, version string, k KnowledgeReview) (Proposal, error) {
	var result Proposal
	if !memory.ValidIdentity(version) {
		return result, fmt.Errorf("invalid knowledge version ID")
	}
	// Let reject and other decisions proceed while hrn inspects the version.
	pending := func(tx *transaction) (*Proposal, error) {
		p, err := tx.find(id)
		if err != nil {
			return nil, err
		}
		if p.Kind != "knowledge" {
			return nil, fmt.Errorf("-version requires a knowledge proposal")
		}
		if p.Status != "pending" {
			return nil, fmt.Errorf("cannot keep a %s proposal", p.Status)
		}
		return p, nil
	}
	if err := s.withUser(owner, func(tx *transaction) error { _, err := pending(tx); return err }); err != nil {
		return result, err
	}
	if !k.Enabled() {
		return result, ErrKnowledgeDisabled
	}
	v, err := hrnexec.InspectKnowledge(ctx, k.Options, k.timeout(), k.Scope, version)
	if err != nil {
		return result, err
	}
	switch v.Admission.Verdict {
	case "":
		return result, fmt.Errorf("version %s is not admitted yet", version)
	case "admitted":
	default:
		return result, fmt.Errorf("version %s has verdict %q", version, v.Admission.Verdict)
	}
	err = s.withUser(owner, func(tx *transaction) error {
		p, err := pending(tx)
		if err != nil {
			return err
		}
		keys, err := draftKeys(tx.root, id, k.Scope, p.SourceRuns, false)
		if err != nil {
			return err
		}
		for _, key := range keys {
			if !slices.ContainsFunc(v.Entries, func(e hrnexec.KnowledgeEntry) bool { return e.Key == key }) {
				return fmt.Errorf("admitted version is missing draft entry %q", key)
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		p.Status, p.Version, p.Decided = "kept", version, time.Now().UTC()
		if err := p.validateKnowledge(); err != nil {
			return err
		}
		result = *p
		return tx.save()
	})
	return result, err
}
