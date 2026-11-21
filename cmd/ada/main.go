package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/cristiangirlea/adanext-ada/config"
	"github.com/cristiangirlea/adanext-ada/memory"
	"github.com/cristiangirlea/adanext-ada/tool"
)

func terminalField(text string) string {
	var clean strings.Builder
	for _, r := range text {
		if unicode.IsControl(r) {
			quoted := strconv.QuoteRune(r)
			clean.WriteString(quoted[1 : len(quoted)-1])
		} else {
			clean.WriteRune(r)
		}
	}
	return clean.String()
}

func memoryCommand(command string, args []string, settings config.Settings, store *memory.Store, n int, out io.Writer) error {
	switch command {
	case "profile":
		switch {
		case len(args) == 1 && args[0] == "list":
			facts, err := store.ReadFacts(settings.Owner)
			if err != nil {
				return err
			}
			for _, f := range facts {
				if _, err := fmt.Fprintf(out, "%s\t%s\n", f.ID, f.Text); err != nil {
					return err
				}
			}
			return nil
		case len(args) >= 2 && args[0] == "add":
			f, err := store.AddFact(settings.Owner, strings.Join(args[1:], " "), "cli")
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(out, f.ID)
			return err
		case len(args) == 2 && args[0] == "remove":
			return store.RemoveFact(settings.Owner, args[1])
		default:
			return fmt.Errorf("usage: ada profile [-config FILE] list | add TEXT | remove ID")
		}
	case "sessions":
		if len(args) != 0 || n < 1 {
			return fmt.Errorf("usage: ada sessions [-n N], N must be positive")
		}
		sessions, err := store.List(settings.Owner, n)
		if err != nil {
			return err
		}
		for _, s := range sessions {
			var origins []string
			for _, o := range s.Origins {
				origins = append(origins, fmt.Sprintf("%s/%s/%s", o.Channel, o.Chat, o.Sender))
			}
			first, err := store.FirstUserLine(s.ID)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", s.ID, s.Updated.Format(time.RFC3339Nano), strings.Join(origins, ", "), terminalField(first)); err != nil {
				return err
			}
		}
		return nil
	case "search":
		hits, err := store.Search(settings.Owner, strings.Join(args, " "), 0, settings.RunsDir(os.Getenv))
		if err != nil {
			return err
		}
		if len(hits) == 0 {
			return nil
		}
		for i := range hits {
			hits[i].Ref = terminalField(hits[i].Ref)
			hits[i].Excerpt = terminalField(hits[i].Excerpt)
		}
		_, err = fmt.Fprintln(out, tool.FormatHits(hits))
		return err
	}
	return fmt.Errorf("unknown memory command %q", command)
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, in io.Reader, out, diagnostics io.Writer) error {
	if len(args) == 1 && args[0] == "version" {
		version := "dev"
		if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
			version = info.Main.Version
		}
		_, err := fmt.Fprintln(out, "ada", version)
		return err
	}
	const usage = "usage: ada chat | profile | sessions | search | version"
	if len(args) == 0 {
		return errors.New(usage)
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(out)
	path := flags.String("config", "", "settings file")
	var fresh bool
	var resume string
	var n int
	switch args[0] {
	case "chat":
		flags.BoolVar(&fresh, "new", false, "start a new session")
		flags.StringVar(&resume, "resume", "", "resume this session")
	case "sessions":
		flags.IntVar(&n, "n", 20, "maximum number of sessions")
	case "profile", "search":
	default:
		return errors.New(usage)
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	settings, err := config.Load(*path)
	if err != nil {
		return err
	}
	if args[0] != "chat" {
		store, err := memory.NewStore(settings.DataDir)
		if err != nil {
			return err
		}
		defer store.Close()
		return memoryCommand(args[0], flags.Args(), settings, store, n, out)
	}
	if flags.NArg() != 0 || fresh && resume != "" {
		return fmt.Errorf("usage: ada chat [-config FILE] [-new | -resume ID]")
	}
	conversation, err := config.Build(settings, os.Getenv)
	if err != nil {
		return err
	}
	defer conversation.Tools.Close()
	defer conversation.Store.Close()
	store := conversation.Store
	unlock, err := store.LockSessions()
	if err != nil {
		return err
	}
	defer unlock()
	owner := settings.Owner
	if err := store.Link("cli", owner, owner); err != nil {
		return err
	}
	user, ok := store.Resolve("cli", owner)
	if !ok {
		return fmt.Errorf("cannot resolve CLI owner")
	}
	ctx = memory.WithUser(ctx, user)
	var session memory.Session
	switch {
	case resume != "":
		session, err = store.Get(resume)
		if err == nil && session.User != user {
			return fmt.Errorf("session does not belong to the CLI owner")
		}
	case fresh:
		session, err = store.NewSession(user, "cli", "local", owner)
	default:
		session, err = store.Open(user, "cli", "local", owner, settings.SessionIdle)
	}
	if err != nil {
		return err
	}
	history, err := store.History(session.ID, settings.HistoryMessages)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(diagnostics, "Session", session.ID); err != nil {
		return fmt.Errorf("print session id: %w", err)
	}

	input := bufio.NewScanner(in)
	input.Buffer(make([]byte, 64<<10), 4<<20)
	for input.Scan() {
		turn, stop := signal.NotifyContext(ctx, os.Interrupt)
		answer, next, err := conversation.Turn(turn, history, input.Text())
		interrupted := turn.Err() == context.Canceled
		stop()
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			if interrupted && errors.Is(err, context.Canceled) {
				fmt.Fprintln(diagnostics, "Turn canceled.")
			} else {
				fmt.Fprintln(diagnostics, err)
			}
			continue
		default:
			if err := store.Append(session.ID, "cli", "local", owner, next[len(history):]...); err != nil {
				return err
			}
			history = memory.HistoryWindow(next, settings.HistoryMessages)
		}
		if _, err := fmt.Fprintln(out, answer); err != nil {
			return fmt.Errorf("print answer: %w", err)
		}
	}
	if err := input.Err(); err != nil {
		return fmt.Errorf("read chat input: %w", err)
	}
	return nil
}
