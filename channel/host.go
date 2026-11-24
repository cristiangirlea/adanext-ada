package channel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	ada "github.com/cristiangirlea/adanext-ada"
	"github.com/cristiangirlea/adanext-ada/internal/proctree"
	p "github.com/cristiangirlea/harness/contracts/pluginproto"
)

type Host struct {
	receipts map[string]*receipt
	Settings
	ctx      context.Context
	fail     context.CancelCauseFunc
	manifest Manifest
	env      []string
	service  *Service
	ledger   *ledger
	mu       sync.Mutex
	prompts  map[string]prompt
}

func (s *Service) Serve(ctx context.Context, data string, settings []Settings) error {
	s.users = map[string]*queue{}
	s.hosts = make(map[string]*Host)
	var failures []error
	var hosts []*Host
	report := func(name string, err error) {
		s.status(name, err.Error())
		failures = append(failures, fmt.Errorf("channel %s: %w", name, err))
	}
	for _, ch := range settings {
		if !ch.Enabled {
			continue
		}
		h, err := s.openHost(data, ch)
		if err != nil {
			report(ch.Name, err)
			continue
		}
		defer h.ledger.close()
		h.ctx, h.fail = context.WithCancelCause(ctx)
		hosts = append(hosts, h)
		s.hosts[h.Name] = h
	}
	if len(hosts) == 0 && len(failures) == 0 && s.Scheduler == nil {
		return fmt.Errorf("no enabled channels")
	}
	type result struct {
		name string
		err  error
	}
	done := make(chan result, len(hosts))
	if s.Scheduler != nil {
		run, stop := context.WithCancel(ctx)
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			s.Scheduler.Serve(run)
		}()
		defer func() { stop(); <-finished }()
	}
	if s.ProposeEvery > 0 {
		run, stop := context.WithCancel(ctx)
		finished := make(chan struct{})
		go func() { defer close(finished); s.runLearning(run) }()
		defer func() { stop(); <-finished }()
	}
	queueRecovered(hosts)
	for _, h := range hosts {
		go func() {
			err := h.run(h.ctx)
			s.mu.Lock()
			delete(s.hosts, h.Name)
			s.mu.Unlock()
			if cause := context.Cause(h.ctx); cause != nil {
				err = cause
			}
			h.fail(nil)
			done <- result{h.Name, err}
		}()
	}
	for range hosts {
		r := <-done
		if r.err != nil && ctx.Err() == nil {
			report(r.name, r.err)
		}
	}
	s.workers.Wait()
	if s.Scheduler != nil && ctx.Err() == nil {
		jobs, err := s.Scheduler.Store.List()
		if err != nil {
			return err
		}
		if len(failures) == 0 || len(jobs) > 0 {
			<-ctx.Done()
		}
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return errors.Join(failures...)
}

func queueRecovered(hosts []*Host) {
	type entry struct {
		host   *Host
		in     *p.Inbound
		minute time.Time
		second string
	}
	var entries []entry
	for _, h := range hosts {
		for _, in := range h.ledger.replay() {
			// The journal parser validates RFC 3339, including leap seconds and arbitrary
			// fraction precision. Convert the minute to UTC and keep the exact second.
			stamp := strings.ToUpper(in.Time)
			zone := 19 + strings.IndexAny(stamp[19:], "Z+-")
			minute, _ := time.Parse(time.RFC3339, stamp[:17]+"00"+stamp[zone:])
			fraction := strings.TrimSuffix(strings.TrimRight(stamp[19:zone], "0"), ".")
			entries = append(entries, entry{h, in, minute, stamp[17:19] + fraction})
		}
	}
	// A stable sort preserves journal order for ties within a channel.
	slices.SortStableFunc(entries, func(a, b entry) int {
		if cmp := a.minute.Compare(b.minute); cmp != 0 {
			return cmp
		}
		if cmp := strings.Compare(a.second, b.second); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.host.Name, b.host.Name)
	})
	for _, e := range entries {
		if h := e.host; h.ctx.Err() == nil {
			if err := h.receive(h.ctx, e.in, true); err != nil {
				h.fail(err)
			}
		}
	}
}

func (s *Service) openHost(data string, ch Settings) (*Host, error) {
	if err := ch.Validate(); err != nil {
		return nil, err
	}
	if err := s.Store.SyncAllow(ch.Name, ch.Allow, s.Owner); err != nil {
		return nil, err
	}
	manifest, err := ch.Manifest()
	if err != nil {
		return nil, err
	}
	env, err := manifest.environment(s.WithoutEnv)
	if err != nil {
		return nil, err
	}
	l, err := openLedger(data, ch.Name)
	if err != nil {
		return nil, fmt.Errorf("open ledger: %w", err)
	}
	return &Host{Settings: ch, service: s, manifest: manifest, env: env, ledger: l}, nil
}

func (h *Host) redact(text string) string {
	// Match the value actually inherited at spawn even if the host environment changes.
	for _, entry := range h.env {
		name, secret, _ := strings.Cut(entry, "=")
		if name == h.manifest.SecretEnv && secret != "" {
			return strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	return text
}
func (h *Host) run(ctx context.Context) error {
	delay := time.Second
	started := false
	for ctx.Err() == nil {
		start := time.Now()
		child, err := h.start()
		if err == nil {
			started = true
			var retry bool
			retry, err = h.connected(ctx, child, 30*time.Second)
			child.stop()
			if !retry || ctx.Err() != nil {
				return err
			}
		} else if !started {
			return err
		}
		if time.Since(start) > time.Minute {
			delay = time.Second
		}
		message := fmt.Sprintf("disconnected (%v), restarting in %v", err, delay)
		if child != nil && len(child.tail) > 0 {
			message += fmt.Sprintf(" (stderr %q)", h.redact(string(child.tail)))
		}
		h.service.status(h.Name, h.redact(message))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
		delay = min(2*delay, time.Minute)
	}
	return nil
}

type stderrTail []byte

func (t *stderrTail) Write(b []byte) (int, error) {
	const limit = 4096
	n := len(b)
	if n >= limit {
		*t = append((*t)[:0], b[n-limit:]...)
	} else {
		drop := max(0, len(*t)+n-limit)
		*t = append((*t)[:copy(*t, (*t)[drop:])], b...)
	}
	return n, nil
}

type process struct {
	in            io.WriteCloser
	out           *os.File
	kill, release func()
	done          chan struct{}
	tail          stderrTail
}

func (h *Host) start() (*process, error) {
	path, err := filepath.Abs(h.Path)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(path)
	proctree.Configure(cmd)
	cmd.Env = h.env
	c := &process{done: make(chan struct{})}
	cmd.Stderr = &c.tail
	cmd.WaitDelay = time.Second
	c.in, err = cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// Own the read end so Wait cannot discard a last frame on process exit.
	reader, writer, err := os.Pipe()
	if err != nil {
		c.in.Close()
		return nil, err
	}
	c.out = reader
	cmd.Stdout = writer
	err = cmd.Start()
	writer.Close()
	if err != nil {
		reader.Close()
		c.in.Close()
		return nil, fmt.Errorf("start channel plugin: %w", err)
	}
	c.kill, c.release, err = proctree.Manage(cmd)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		reader.Close()
		c.in.Close()
		return nil, err
	}
	go func() { cmd.Wait(); c.kill(); close(c.done) }()
	return c, nil
}
func (c *process) stop() {
	// Close stdin even if a plugin has stopped draining its input pipe.
	go c.in.Close()
	select {
	case <-c.done:
	case <-time.After(4 * time.Second):
		c.kill()
		<-c.done
	}
	c.out.Close()
	c.release()
}

type incoming struct {
	frame any
	err   error
}

func sequence(frame any) int64 {
	switch f := frame.(type) {
	case *p.Ready:
		return f.Seq
	case *p.Inbound:
		return f.Seq
	case *p.Delivered:
		return f.Seq
	case *p.DeliveryFailed:
		return f.Seq
	case *p.Status:
		return f.Seq
	case *p.Error:
		return f.Seq
	default:
		return 0
	}
}
func (c *process) read(ctx context.Context, ch chan<- incoming) {
	lines := bufio.NewScanner(c.out)
	lines.Buffer(make([]byte, 4096), p.MaxLine+2)
	send := func(f any, err error) bool {
		select {
		case ch <- incoming{f, err}:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for seq := int64(1); lines.Scan(); seq++ {
		line := lines.Bytes()
		f, err := p.ParsePluginLineV2(line, p.KindChannel)
		if err == nil {
			if got := sequence(f); got != seq {
				err = fmt.Errorf("expected sequence %d, received %d", seq, got)
			}
		}
		if !send(f, err) || err != nil {
			return
		}
	}
	err := lines.Err()
	if err == nil {
		err = io.EOF
	}
	send(nil, err)
}
func (c *process) write(ctx context.Context, frames <-chan any, failed chan<- error) {
	encoder := json.NewEncoder(c.in)
	for {
		select {
		case <-ctx.Done():
			return
		case f := <-frames:
			if err := encoder.Encode(f); err != nil {
				failed <- err
				return
			}
		}
	}
}
func (h *Host) connected(ctx context.Context, c *process, timeout time.Duration) (retry bool, err error) {
	transport, stop := context.WithCancel(ctx)
	defer stop()
	input := make(chan incoming)
	go c.read(transport, input)
	output := make(chan any)
	failed := make(chan error, 1)
	go c.write(transport, output, failed)
	select {
	case output <- p.Hello{Type: p.TypeHello, Protocols: []string{p.V2}, Host: p.Program{Name: "ada", Version: ada.Version()}}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-time.After(timeout):
		return true, fmt.Errorf("channel handshake timed out")
	case err := <-failed:
		return true, err
	case msg := <-input:
		if msg.err != nil {
			return errors.Is(msg.err, io.EOF), fmt.Errorf("read channel handshake: %w", msg.err)
		}
		ready, ok := msg.frame.(*p.Ready)
		if !ok || ready.Protocol != p.V2 || ready.Kind != p.KindChannel || ready.Channel == nil || ready.Channel.Name != h.Name {
			return false, fmt.Errorf("expected ready for v2 channel %q", h.Name)
		}
	}
	h.register()
	defer h.unregister()
	h.service.status(h.Name, "ready")
	inFlight := map[string]bool{}
	var queue []any
	var retryAt <-chan time.Time
	refresh := func() {
		ready, next := h.ledger.ready(inFlight)
		for _, d := range ready {
			queue = append(queue, d)
		}
		retryAt = nil
		if !next.IsZero() {
			retryAt = time.After(time.Until(next))
		}
	}
	refresh()
	for {
		var send chan any
		var head any
		if len(queue) > 0 {
			send = output
			head = queue[0]
			if d, ok := head.(pending); ok {
				allowed, err := h.prepareDelivery(d)
				if err != nil {
					return false, err
				}
				if !allowed {
					delete(inFlight, d.ID)
					queue[0] = nil
					queue = queue[1:]
					continue
				}
				head = d.Deliver
			}
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-retryAt:
			refresh()
		case <-h.ledger.wake:
			refresh()
		case err := <-failed:
			return true, err
		case send <- head:
			queue[0] = nil
			queue = queue[1:]
		case msg := <-input:
			if msg.err != nil {
				return true, msg.err
			}
			switch f := msg.frame.(type) {
			case *p.Inbound:
				fresh, err := h.ledger.accept(f)
				if err != nil {
					return false, err
				}
				queue = append(queue, p.Ack{Type: p.TypeAck, ID: f.ID})
				if fresh {
					if err := h.receive(ctx, f, false); err != nil {
						return false, err
					}
				}
			case *p.Delivered:
				if !inFlight[f.ID] {
					continue
				}
				if err := h.ledger.record(outRecord{Delivered: f}); err != nil {
					return false, err
				}
				delete(inFlight, f.ID)
				h.received(f.ID, nil, false)
			case *p.DeliveryFailed:
				if !inFlight[f.ID] {
					continue
				}
				if err := h.ledger.record(outRecord{Failed: f}); err != nil {
					return false, err
				}
				delete(inFlight, f.ID)
				h.received(f.ID, fmt.Errorf("delivery failed (%s)", f.Error.Code), f.Error.Code == p.CodeRateLimited)
				h.service.status(h.Name, fmt.Sprintf("delivery %s failed (%s)", f.ID, f.Error.Code))
			case *p.Status:
				h.service.status(h.Name, h.redact(f.State+" "+f.Text))
			default:
				return true, fmt.Errorf("unexpected channel frame %T", f)
			}
		}
	}
}
