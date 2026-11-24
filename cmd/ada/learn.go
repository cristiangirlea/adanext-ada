package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cristiangirlea/adanext-ada/config"
	"github.com/cristiangirlea/adanext-ada/learn"
	"github.com/cristiangirlea/adanext-ada/memory"
	"github.com/cristiangirlea/adanext-ada/provider"
)

func printProposals(out io.Writer, items []learn.Proposal, detail bool) error {
	for _, p := range items {
		if p.Kind == "skill" {
			var verdict string
			if p.Measure != nil {
				verdict = " verdict=" + p.Measure.Verdict
			}
			summary := terminalField(p.Text)
			if detail {
				summary = fmt.Sprintf("skill \"%s\" for %s: %s", terminalField(p.Skill.Name), terminalField(filepath.Base(p.Repo)), terminalField(p.Skill.Description))
			}
			if _, err := fmt.Fprintf(out, "%s\t%s\tskill\trun=%s\toutside=true%s\t%s\n", p.ID, p.Status, p.SourceRuns[0], verdict, summary); err != nil {
				return err
			}
			if detail {
				for line := range strings.SplitSeq(p.Skill.Body, "\n") {
					if _, err := fmt.Fprintf(out, "    %s\n", terminalField(line)); err != nil {
						return err
					}
				}
			}
			continue
		}
		if p.Kind == "knowledge" {
			if _, err := fmt.Fprintf(out, "%s\t%s\tknowledge\tdraft=%s\t%s\n", p.ID, p.Status, p.Draft, terminalField(p.Text)); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(out, "%s\t%s\tsession=%s\tmessages=%v\toutside=%t\t%s\n", p.ID, p.Status, p.Session, p.Messages, p.Outside, terminalField(p.Text)); err != nil {
			return err
		}
	}
	return nil
}

func learnCommand(ctx context.Context, args []string, path string, out, diagnostics io.Writer) error {
	const usage = "usage: ada learn propose [-session ID] [-since DURATION] | list [-all] | measure ID -spec FILE [-base REF] [-held-out SHARE -seed N] [-timeout D] | keep ID [-outside] [-version VERSION_ID] | reject ID | rollback ID | confirm ID | restore ID"
	if len(args) == 0 {
		return errors.New(usage)
	}
	command := args[0]
	flags := flag.NewFlagSet("learn "+command, flag.ContinueOnError)
	flags.SetOutput(out)
	flags.StringVar(&path, "config", path, "settings file")
	var measure learn.MeasureOptions
	var session, id, version string
	var since time.Duration
	var outside, all bool
	switch command {
	case "propose":
		flags.Func("session", "session to review", func(id string) error {
			if !memory.ValidSessionID(id) {
				return fmt.Errorf("invalid session ID")
			}
			session = id
			return nil
		})
		flags.DurationVar(&since, "since", 24*time.Hour, "review sessions updated within this duration")
	case "measure":
		flags.StringVar(&measure.Spec, "spec", "", "hrn eval spec file")
		flags.StringVar(&measure.Base, "base", "HEAD", "base git revision")
		flags.StringVar(&measure.HeldOut, "held-out", "", "held-out task share")
		flags.StringVar(&measure.Seed, "seed", "", "held-out split seed")
		flags.DurationVar(&measure.Timeout, "timeout", 4*time.Hour, "measurement time limit")
	case "list":
		flags.BoolVar(&all, "all", false, "include decided proposals")
	case "keep":
		flags.StringVar(&version, "version", "", "admitted hrn knowledge version")
		flags.BoolVar(&outside, "outside", false, "approve outside content")
	case "reject", "confirm", "restore", "rollback":
	default:
		return errors.New(usage)
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if command != "propose" && command != "list" {
		if flags.NArg() == 0 {
			return errors.New(usage)
		}
		id = flags.Arg(0)
		if err := flags.Parse(flags.Args()[1:]); err != nil {
			return err
		}
	}
	if flags.NArg() != 0 || command == "propose" && since <= 0 {
		return errors.New(usage)
	}
	settings, err := config.Load(path)
	if err != nil {
		return err
	}
	m, err := memory.NewStore(settings.DataDir)
	if err != nil {
		return err
	}
	defer m.Close()
	store := learn.Store{Memory: m, Roots: settings.WorkspaceRoots, Diagnose: func(note string) { fmt.Fprintln(diagnostics, terminalField(note)) }}
	now := time.Now().UTC()
	user := settings.Owner
	items, err := store.Sweep(user, now, settings.Learning.StaleAfter, settings.Learning.ArchiveAfter)
	if err = errors.Join(err, printProposals(out, items, false)); err != nil {
		return err
	}
	switch command {
	case "list":
		items, err = store.List(user, all)
	case "propose":
		model, _, modelErr := settings.BuildProvider(os.Getenv)
		if modelErr != nil {
			err = modelErr
		} else {
			items, err = proposeFacts(ctx, model, settings, store, session, now, since)
		}
		review := settings.KnowledgeReview().WithDiagnostics(func(message string) { fmt.Fprintln(diagnostics, message) })
		review.SkipBackoff = true
		project, knowledgeErr := learn.ProposeKnowledge(ctx, store, user, review)
		if errors.Is(knowledgeErr, learn.ErrKnowledgeDisabled) {
			_, knowledgeErr = fmt.Fprintln(diagnostics, knowledgeErr)
		}
		items = append(items, project...)
		err = errors.Join(err, knowledgeErr)
		if settings.Learning.Skills && modelErr == nil {
			review := settings.SkillReview()
			review.Diagnose = func(message string) { fmt.Fprintln(diagnostics, terminalField(message)) }
			skillsCtx, cancel := context.WithTimeout(ctx, settings.Budget.Timeout)
			defer cancel()
			skills, skillErr := store.ProposeSkills(skillsCtx, model, user, review)
			items = append(items, skills...)
			err = errors.Join(err, skillErr)
		}
	case "measure":
		if measure.Timeout <= 0 {
			return fmt.Errorf("-timeout must be positive")
		}
		measure.Options = settings.SkillReview().Options
		measure.Diagnostics = diagnostics
		p, e := store.MeasureSkill(ctx, user, id, measure)
		if p.Measure == nil {
			return e
		}
		_, err = fmt.Fprintf(out, "%s: %s\n", p.Measure.Verdict, terminalField(p.Measure.Reason))
		if e != nil {
			return errors.Join(e, err)
		}
		if err == nil && p.Measure.Verdict == "better" {
			_, err = fmt.Fprintf(out, "keep it with ada learn keep %s -outside; then merge branch %s in %s to use it\n", p.ID, p.Measure.Branch, terminalField(p.Repo))
		}
		return err
	default:
		var item learn.Proposal
		if version != "" {
			item, err = store.KeepKnowledge(ctx, user, id, version, settings.KnowledgeReview())
		} else {
			var note string
			item, note, err = store.DecideWithNote(user, id, command, outside, time.Time{})
			if err == nil && note != "" {
				_, err = fmt.Fprintln(out, terminalField(note))
			}
			if err == nil && command == "keep" && item.Kind == "skill" {
				_, err = fmt.Fprintf(out, "Merge branch %s in %s to use it. Delegated runs load skills only with hrn.skills set.\n", item.Measure.Branch, terminalField(item.Repo))
			}
		}
		if err == nil && command == "rollback" && item.Kind == "knowledge" {
			_, err = fmt.Fprintf(out, "Maintainer: run hrn knowledge rollback request.json to roll back the active project knowledge in hrn (proposal version %s).\n", terminalField(item.Version))
		}
		if err != nil {
			return err
		}
		items = []learn.Proposal{item}
	}
	return errors.Join(err, printProposals(out, items, command == "list"))
}

func proposeFacts(ctx context.Context, model provider.Provider, settings config.Settings, store learn.Store, session string, now time.Time, since time.Duration) ([]learn.Proposal, error) {
	review, cancel := context.WithTimeout(ctx, settings.Budget.Timeout)
	defer cancel()
	if session != "" {
		return learn.Propose(review, model, store, settings.Owner, session)
	}
	return store.ProposeRecent(review, model, settings.Owner, now, settings.SessionIdle, since)
}
