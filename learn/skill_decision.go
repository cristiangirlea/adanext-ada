package learn

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/cristiangirlea/adanext-ada/memory"
)

func (p Proposal) skillDecisionTime(now time.Time) time.Time {
	if p.Measure != nil && now.Before(p.Measure.At) {
		return p.Measure.At
	}
	return now
}

func (tx *transaction) changeSkill(p *Proposal, action string, now time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	switch action {
	case "keep":
		if _, err := tx.root.Lstat(measureJournal(p.ID)); err == nil {
			unlock, err := memory.TryMeasureLock(tx.root, p.ID)
			if errors.Is(err, memory.ErrMeasuring) {
				return fmt.Errorf("skill is %w", err)
			}
			if err != nil {
				return err
			}
			unlock()
			return fmt.Errorf("cleanup from an earlier measurement is pending and will be retried on the next command")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if p.Measure == nil || p.Measure.Verdict != "better" {
			return fmt.Errorf("skill requires a better measurement before keep")
		}
		if err := tx.store.checkRepo(ctx, *p); err != nil {
			return err
		}
		tip, err := branchTip(ctx, *p)
		if err != nil {
			return err
		}
		if tip != p.Measure.Commit {
			return fmt.Errorf("branch changed outside Ada")
		}
		if err := verifyBlob(ctx, *p, tip); err != nil {
			return err
		}
		p.Status, p.Decided = "kept", now
		return tx.save()
	case "reject", "rollback":
		op := intent{ID: p.ID, Action: action, At: now}
		if err := memory.WriteJSON(tx.root, tx.journal(), op); err != nil {
			return err
		}
		return tx.recoverSkillDecision(p, op)
	default:
		return fmt.Errorf("cannot %s a skill", action)
	}
}

func (tx *transaction) recoverSkillDecision(p *Proposal, op intent) error {
	if op.Action != "reject" && op.Action != "rollback" {
		return fmt.Errorf("invalid skill decision intent")
	}
	if op.At.Before(p.Created) {
		return fmt.Errorf("skill intent predates proposal")
	}
	op.At = p.skillDecisionTime(op.At)
	step := transitions[op.Action]
	if p.Status == step.to && p.Decided.Equal(op.At) {
		return tx.root.Remove(tx.journal())
	}
	if !slices.Contains(step.from, p.Status) || op.At.Before(p.Decided) {
		return fmt.Errorf("skill intent conflicts with proposal")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if m := p.Measure; m != nil {
		if err := tx.store.checkRepo(ctx, *p); err != nil {
			tx.note = fmt.Sprintf("Left branch %s in %s: %v", p.branch(), p.Repo, err)
			if op.Action == "rollback" {
				tx.note += fmt.Sprintf(". Run git revert %s in %s.", m.Commit, p.Repo)
			}
			return tx.finish(p, op)
		}
		tip, err := branchTip(ctx, *p)
		if err != nil {
			return err
		}
		if op.Action == "rollback" {
			tx.note = fmt.Sprintf("Run git revert %s in %s.", m.Commit, p.Repo)
			if tip == m.Commit {
				_, err := git(ctx, p.Repo, "merge-base", "--is-ancestor", m.Commit, "HEAD")
				if exitCode(err) == 1 {
					removed, err := removeBranch(ctx, *p, m.Commit)
					if err != nil {
						return err
					}
					if removed {
						tx.note = ""
					}
				} else if err != nil {
					return err
				}
			}
			path := p.Skill.path() + "/SKILL.md"
			installed, err := hasTreePath(ctx, p.Repo, "HEAD", path)
			if err != nil {
				return err
			}
			if installed {
				added, err := git(ctx, p.Repo, "log", "-1", "--format=%H", "--diff-filter=A", "HEAD", "--", path)
				if err != nil {
					return err
				}
				tx.note = fmt.Sprintf("Remove %s in %s, or use git revert on the commit that added it.", p.Skill.path(), p.Repo)
				if hash := strings.TrimSpace(string(added)); gitHash.MatchString(hash) {
					tx.note = fmt.Sprintf("Remove %s in %s, or run git revert %s.", p.Skill.path(), p.Repo, hash)
				}
			}
		} else if tip != "" {
			removed, err := removeBranch(ctx, *p, m.Commit)
			if err != nil {
				return err
			}
			if !removed {
				tx.note = fmt.Sprintf("Left branch %s in %s because its tip changed or it is checked out.", p.branch(), p.Repo)
			}
		}
	}
	return tx.finish(p, op)
}
