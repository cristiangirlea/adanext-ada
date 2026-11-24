package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/cristiangirlea/adanext-ada/config"
	"github.com/cristiangirlea/adanext-ada/learn"
	"github.com/cristiangirlea/adanext-ada/memory"
)

func printProposals(out io.Writer, items []learn.Proposal) error {
	for _, p := range items {
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
	const usage = "usage: ada learn propose [-session ID] [-since DURATION] | list [-all] | keep ID [-outside] [-version VERSION_ID] | reject ID | rollback ID | confirm ID | restore ID"
	if len(args) == 0 {
		return errors.New(usage)
	}
	command := args[0]
	flags := flag.NewFlagSet("learn "+command, flag.ContinueOnError)
	flags.SetOutput(out)
	flags.StringVar(&path, "config", path, "settings file")
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
	store := learn.Store{Memory: m}
	now := time.Now().UTC()
	user := settings.Owner
	items, err := store.Sweep(user, now, settings.Learning.StaleAfter, settings.Learning.ArchiveAfter)
	if err = errors.Join(err, printProposals(out, items)); err != nil {
		return err
	}
	switch command {
	case "list":
		items, err = store.List(user, all)
	case "propose":
		items, err = proposeFacts(ctx, settings, store, session, now, since)
		review := settings.KnowledgeReview().WithDiagnostics(func(message string) { fmt.Fprintln(diagnostics, message) })
		review.SkipBackoff = true
		project, knowledgeErr := learn.ProposeKnowledge(ctx, store, user, review)
		if errors.Is(knowledgeErr, learn.ErrKnowledgeDisabled) {
			_, knowledgeErr = fmt.Fprintln(diagnostics, knowledgeErr)
		}
		items = append(items, project...)
		err = errors.Join(err, knowledgeErr)
	default:
		var item learn.Proposal
		if version != "" {
			item, err = store.KeepKnowledge(ctx, user, id, version, settings.KnowledgeReview())
		} else {
			item, err = store.Decide(user, id, command, outside, time.Now().UTC())
		}
		if err == nil && command == "rollback" && item.Kind == "knowledge" {
			_, err = fmt.Fprintf(out, "Maintainer: run hrn knowledge rollback request.json to roll back the active project knowledge in hrn (proposal version %s).\n", terminalField(item.Version))
		}
		if err != nil {
			return err
		}
		items = []learn.Proposal{item}
	}
	return errors.Join(err, printProposals(out, items))
}

func proposeFacts(ctx context.Context, settings config.Settings, store learn.Store, session string, now time.Time, since time.Duration) ([]learn.Proposal, error) {
	model, _, err := settings.BuildProvider(os.Getenv)
	if err != nil {
		return nil, err
	}
	review, cancel := context.WithTimeout(ctx, settings.Budget.Timeout)
	defer cancel()
	if session != "" {
		return learn.Propose(review, model, store, settings.Owner, session)
	}
	return store.ProposeRecent(review, model, settings.Owner, now, settings.SessionIdle, since)
}
