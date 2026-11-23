package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/signal"
	"slices"
	"strings"
	"time"

	"github.com/cristiangirlea/adanext-ada/config"
	"github.com/cristiangirlea/adanext-ada/memory"
	"github.com/cristiangirlea/adanext-ada/schedule"
)

func scheduleCommand(ctx context.Context, args []string, path string, out io.Writer) error {
	const usage = "usage: ada schedule add | list | remove ID | enable ID | disable ID | runs | run ID"
	if len(args) == 0 {
		return errors.New(usage)
	}
	command := args[0]
	flags := flag.NewFlagSet("schedule "+command, flag.ContinueOnError)
	flags.SetOutput(out)
	flags.StringVar(&path, "config", path, "settings file")
	var job schedule.Job
	var script bool
	var to string
	var n int
	switch command {
	case "add":
		flags.StringVar(&job.Name, "name", "", "job name")
		flags.StringVar(&job.Spec, "spec", "", "cron or every <duration>")
		flags.StringVar(&job.Prompt, "prompt", "", "prompt text")
		flags.BoolVar(&script, "script", false, "execute argv after --")
		flags.StringVar(&to, "to", "", "destination channel:chat")
		flags.DurationVar(&job.Budget.Timeout, "timeout", 10*time.Minute, "job timeout")
	case "runs":
		flags.IntVar(&n, "n", 20, "maximum number of runs")
	case "list", "remove", "enable", "disable", "run":
	default:
		return errors.New(usage)
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	rest := flags.Args()
	s, err := config.Load(path)
	if err != nil {
		return err
	}
	m, err := memory.NewStore(s.DataDir)
	if err != nil {
		return err
	}
	defer m.Close()
	store := schedule.Store{Memory: m}
	switch command {
	case "add":
		job.Kind = "prompt"
		if script {
			split := len(args) - len(rest) - 1
			if len(rest) == 0 || args[split] != "--" || job.Prompt != "" {
				return fmt.Errorf("use -script -- ARGV or -prompt TEXT")
			}
			job.Kind = "script"
			job.Argv = rest
		} else if len(rest) > 0 {
			return fmt.Errorf("unexpected arguments after prompt")
		}
		if job.Budget.Timeout <= 0 {
			return fmt.Errorf("timeout must be positive")
		}
		job.Target, err = schedule.ParseTarget(to)
		if err != nil {
			return err
		}
		job, err = store.Add(job, time.Now())
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, job.ID)
	case "list":
		if len(rest) != 0 {
			return errors.New(usage)
		}
		jobs, err := store.List()
		if err != nil {
			return err
		}
		for _, j := range jobs {
			if _, err = fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\tenabled=%t\n", j.ID, terminalField(j.Name), j.Kind, terminalField(j.Spec), terminalField(j.Target.String()), j.Enabled); err != nil {
				return err
			}
		}
	case "runs":
		if len(rest) != 0 {
			return errors.New(usage)
		}
		runs, err := store.Runs(n)
		if err != nil {
			return err
		}
		for _, r := range runs {
			if _, err = fmt.Fprintf(out, "%s\t%s\t%s\tdelivered=%t\t%s\n", r.Started.Format(time.RFC3339), r.Job, terminalField(r.Status), r.Delivered, terminalField(r.Output)); err != nil {
				return err
			}
		}
	default:
		if len(rest) != 1 {
			return errors.New(usage)
		}
		switch command {
		case "remove":
			return store.Remove(rest[0])
		case "enable", "disable":
			return store.SetEnabled(rest[0], command == "enable")
		case "run":
			return runSchedule(ctx, store, s, rest[0], out)
		}
	}
	return err
}

func runSchedule(ctx context.Context, store schedule.Store, s config.Settings, id string, out io.Writer) error {
	unlock, err := store.Memory.LockSessions()
	if err != nil {
		return err
	}
	defer unlock()
	jobs, err := store.List()
	if err != nil {
		return err
	}
	idx := slices.IndexFunc(jobs, func(j schedule.Job) bool { return j.ID == id })
	if idx < 0 {
		return fmt.Errorf("unknown schedule job %q", id)
	}
	runner := schedule.Runner{Store: store, Owner: s.Owner, History: s.HistoryMessages, WithoutEnv: s.ProviderKeyEnv()}
	if jobs[idx].Kind == "prompt" {
		app, err := buildConversation(s)
		if err != nil {
			return err
		}
		defer app.Store.Close()
		defer app.Tools.Close()
		runner.Loop = app.Loop
	}
	ctx, stop := serveContext(ctx, signal.Notify)
	defer stop()
	r, err := runner.RunLocal(ctx, id)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(r.Status+"\n"+r.Output, "\n") {
		if _, err = fmt.Fprintln(out, terminalField(line)); err != nil {
			return err
		}
	}
	if r.Status != "ok" && r.Status != "exit 0" {
		return fmt.Errorf("job %s: %s", id, r.Status)
	}
	return nil
}
