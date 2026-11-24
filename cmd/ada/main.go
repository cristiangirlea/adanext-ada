package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	ada "github.com/cristiangirlea/adanext-ada"
	"github.com/cristiangirlea/adanext-ada/approval"
	"github.com/cristiangirlea/adanext-ada/channel"
	"github.com/cristiangirlea/adanext-ada/config"
	"github.com/cristiangirlea/adanext-ada/internal/terminal"
	"github.com/cristiangirlea/adanext-ada/memory"
	"github.com/cristiangirlea/adanext-ada/schedule"
	"github.com/cristiangirlea/adanext-ada/tool"
)

func terminalField(text string) string { return terminal.Field(text) }

func memoryCommand(command string, args []string, settings config.Settings, store *memory.Store, n int, out io.Writer) error {
	switch command {
	case "pair":
		if len(args) != 1 {
			return fmt.Errorf("usage: ada pair [-config FILE] CODE")
		}
		p, err := store.Pair(args[0], settings.Owner)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "Paired %s (%s) on %s to %s.\n", terminalField(p.Name), terminalField(p.Sender), terminalField(p.Channel), terminalField(settings.Owner))
		return err
	case "unpair":
		if len(args) != 2 {
			return fmt.Errorf("usage: ada unpair [-config FILE] CHANNEL SENDER")
		}
		return store.Unlink(args[0], args[1])
	case "approvals":
		if len(args) != 0 || n < 1 {
			return fmt.Errorf("usage: ada approvals [-n N], N must be positive")
		}
		sessions, err := store.List(settings.Owner, 0)
		if err != nil {
			return err
		}
		ids := make([]string, 0, len(sessions))
		for _, s := range sessions {
			ids = append(ids, s.ID)
		}
		records, err := approval.Log(settings.DataDir).Latest(settings.Owner, ids, n)
		if err != nil {
			return err
		}
		for _, r := range records {
			session := r.Request.Session
			if session == "" {
				session = "none"
			}
			if _, err := fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				r.Decision.At.Format(time.RFC3339Nano), terminalField(r.Request.ID), session,
				terminalField(r.Request.Tool), terminalField(r.Request.Effect), terminalField(r.Decision.Reason),
				terminalField(r.Decision.By), terminalField(r.Request.Summary)); err != nil {
				return fmt.Errorf("print approval: %w", err)
			}
		}
		return nil
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

func buildConversation(s config.Settings) (*config.App, error) {
	app, err := config.Build(s, os.Getenv)
	if err != nil {
		return nil, err
	}
	if err := app.Tools.Add(schedule.PromptTool{Store: schedule.Store{Memory: app.Store}}); err != nil {
		app.Tools.Close()
		app.Store.Close()
		return nil, err
	}
	return app, nil
}

func newServeService(settings config.Settings, conversation *config.App, diagnostics io.Writer) *channel.Service {
	var statusMu sync.Mutex
	service := channel.Service{
		Store: conversation.Store, Loop: conversation.Loop, Owner: settings.Owner,
		Idle: settings.SessionIdle, History: settings.HistoryMessages,
		WithoutEnv:   settings.ProviderKeyEnv(),
		ProposeEvery: settings.Learning.ProposeEvery, OfferTTL: settings.Learning.OfferTTL,
		WorkspaceRoots: settings.WorkspaceRoots, Knowledge: settings.KnowledgeReview(),
		Status: func(line string) {
			statusMu.Lock()
			defer statusMu.Unlock()
			fmt.Fprintln(diagnostics, terminalField(line))
		},
	}
	service.Decision = service.LearningStore().DecideOffer
	service.Scheduler = &schedule.Runner{Store: schedule.Store{Memory: conversation.Store}, Loop: conversation.Loop, Owner: settings.Owner, History: settings.HistoryMessages, WithoutEnv: service.WithoutEnv, Deliver: service.Deliver, Status: service.Status}
	if settings.Learning.Skills {
		review := settings.SkillReview()
		service.Skills = &review
	}
	return &service
}

func run(ctx context.Context, args []string, in io.Reader, out, diagnostics io.Writer) error {
	if len(args) == 1 && args[0] == "version" {
		_, err := fmt.Fprintln(out, "ada", ada.Version())
		return err
	}
	const usage = "usage: ada chat | serve | schedule | channels | pair | unpair | profile | learn | sessions | approvals | search | version"
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
	case "approvals":
		flags.IntVar(&n, "n", 20, "maximum number of approvals")
	case "profile", "search", "serve", "channels", "pair", "unpair", "schedule", "learn":
	default:
		return errors.New(usage)
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if args[0] == "schedule" {
		return scheduleCommand(ctx, flags.Args(), *path, out)
	}
	if args[0] == "learn" {
		return learnCommand(ctx, flags.Args(), *path, out, diagnostics)
	}
	settings, err := config.Load(*path)
	if err != nil {
		return err
	}
	if args[0] == "channels" {
		if flags.NArg() != 0 {
			return fmt.Errorf("usage: ada channels [-config FILE]")
		}
		for _, ch := range settings.Channels {
			info, statErr := os.Stat(ch.Path)
			found := statErr == nil && info.Mode().IsRegular()
			manifest, err := ch.Manifest()
			description := fmt.Sprintf("label=%q secret_env=%q hosts=%q", manifest.Label, manifest.SecretEnv, manifest.Hosts)
			if err != nil {
				description = terminalField(err.Error())
			}
			if _, err := fmt.Fprintf(out, "%s enabled=%t path=%q found=%t %s\n", ch.Name, ch.Enabled, ch.Path, found, description); err != nil {
				return err
			}
		}
		return nil
	}
	if args[0] != "chat" && args[0] != "serve" {
		store, err := memory.NewStore(settings.DataDir)
		if err != nil {
			return err
		}
		defer store.Close()
		return memoryCommand(args[0], flags.Args(), settings, store, n, out)
	}
	if flags.NArg() != 0 || fresh && resume != "" {
		if args[0] == "serve" {
			return fmt.Errorf("usage: ada serve [-config FILE]")
		}
		return fmt.Errorf("usage: ada chat [-config FILE] [-new | -resume ID]")
	}
	conversation, err := buildConversation(settings)
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
	if args[0] == "serve" {
		ctx, stop := serveContext(ctx, signal.Notify)
		defer stop()
		service := newServeService(settings, conversation, diagnostics)
		return service.Serve(ctx, settings.DataDir, settings.Channels)
	}
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
		if err == nil && session.ScheduledJob != "" {
			return fmt.Errorf("session belongs to scheduled job %s", session.ScheduledJob)
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

	surface := newCLISurface(in, diagnostics)
	ctx = approval.WithApprover(approval.WithSession(ctx, session.ID), surface)
	for {
		line, err := surface.read(ctx, nil)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read chat input: %w", err)
		}
		turn, stop := signal.NotifyContext(ctx, os.Interrupt)
		answer, next, err := conversation.Turn(turn, history, line.text)
		interrupted := turn.Err() == context.Canceled
		stop()
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			if interrupted && errors.Is(err, context.Canceled) {
				surface.printDiagnostic("Turn canceled.")
			} else {
				surface.printDiagnostic(err.Error())
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
}

type cliLine struct {
	text         string
	confirmation bool
	err          error
}

type cliSurface struct {
	reader      *bufio.Reader
	readErr     error
	diagnostics io.Writer
	outputMu    sync.Mutex
	readerLock  chan struct{}
	pending     chan cliLine
}

func newCLISurface(in io.Reader, diagnostics io.Writer) *cliSurface {
	// Keep the chat line limit while allowing the approver to inspect buffered input.
	return &cliSurface{reader: bufio.NewReaderSize(in, 4<<20), diagnostics: diagnostics, readerLock: make(chan struct{}, 1)}
}

var errTypedAhead = errors.New("typed ahead")

func (s *cliSurface) printDiagnostic(message string) error {
	// An expiring approval can report its decision while the chat turn resumes.
	s.outputMu.Lock()
	defer s.outputMu.Unlock()
	_, err := fmt.Fprintln(s.diagnostics, message)
	return err
}

func (s *cliSurface) read(ctx context.Context, prompt func() (string, error)) (cliLine, error) {
	select {
	case s.readerLock <- struct{}{}:
	case <-ctx.Done():
		return cliLine{}, ctx.Err()
	}
	defer func() { <-s.readerLock }()
	if err := ctx.Err(); err != nil {
		return cliLine{}, err
	}
	// A pending read owns the reader until its line is collected.
	if prompt != nil && (s.pending != nil || s.reader.Buffered() > 0) {
		return cliLine{}, errTypedAhead
	}
	if s.pending != nil {
		line, err := s.waitLine(ctx)
		if err != nil || !line.confirmation {
			return line, err
		}
	}
	var code string
	if prompt != nil {
		var err error
		code, err = prompt()
		if err != nil {
			return cliLine{}, err
		}
	}
	reply := make(chan cliLine, 1)
	s.pending = reply
	go func() {
		reply <- s.readLine(code)
	}()
	return s.waitLine(ctx)
}

func (s *cliSurface) readLine(code string) cliLine {
	if s.readErr != nil {
		return cliLine{err: s.readErr}
	}
	raw, err := s.reader.ReadSlice('\n')
	s.readErr = err
	if err != nil && !(errors.Is(err, io.EOF) && len(raw) > 0) {
		return cliLine{err: err}
	}
	text := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	if code != "" && strings.EqualFold(strings.TrimSpace(text), code) {
		// A late or unterminated code must never become chat text.
		return cliLine{confirmation: err == nil, err: err}
	}
	return cliLine{text: text}
}

func (s *cliSurface) waitLine(ctx context.Context) (cliLine, error) {
	select {
	case line := <-s.pending:
		s.pending = nil
		return line, line.err
	case <-ctx.Done():
		return cliLine{}, ctx.Err()
	}
}

func (s *cliSurface) Ask(ctx context.Context, r approval.Request) (approval.Decision, error) {
	ctx, cancel := context.WithDeadline(ctx, r.Expires)
	defer cancel()
	line, err := s.read(ctx, func() (string, error) {
		const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
		var code [4]byte
		rand.Read(code[:])
		for i, b := range code {
			code[i] = alphabet[int(b)%len(alphabet)]
		}
		left := time.Until(r.Expires)
		if left >= time.Second {
			left = left.Round(time.Second)
		}
		duration := min(left, r.Expires.Sub(r.Created)).String()
		if strings.HasSuffix(duration, "m0s") {
			duration = strings.TrimSuffix(duration, "0s")
		}
		prompt := fmt.Sprintf("Ada asks to %s: %s\nType %s to approve, anything else to deny (expires in %s):", terminalField(r.Effect), terminalField(r.Summary), code, duration)
		return string(code[:]), s.printDiagnostic(prompt)
	})
	d := approval.Decision{By: "cli", At: time.Now().UTC(), Reason: "denied"}
	switch {
	case !d.At.Before(r.Expires) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		d.Reason = "expired"
		if err := s.printDiagnostic("Approval expired; nothing was run."); err != nil {
			return d, fmt.Errorf("print approval expiry: %w", err)
		}
	case errors.Is(err, errTypedAhead):
		d.Reason = "typed ahead"
		if err := s.printDiagnostic("Approval denied: input was typed ahead."); err != nil {
			return d, fmt.Errorf("print approval denial: %w", err)
		}
	case errors.Is(err, io.EOF):
	case err != nil:
		return d, fmt.Errorf("read CLI approval: %w", err)
	case ctx.Err() == nil && line.confirmation:
		d.Approved = true
		d.Reason = "approved"
	}
	return d, nil
}

func serveContext(parent context.Context, notify func(chan<- os.Signal, ...os.Signal)) (context.Context, context.CancelFunc) {
	signals := make(chan os.Signal, 1)
	notify(signals, os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithCancel(parent)
	go func() {
		select {
		case <-signals:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() { signal.Stop(signals); cancel() }
}
