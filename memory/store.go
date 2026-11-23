// Package memory stores Ada's local conversation history and user profile.
package memory

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cristiangirlea/adanext-ada/provider"
)

const DefaultIdle = 12 * time.Hour
const DefaultHistory = 40

type Store struct {
	mu          sync.Mutex
	root        *os.Root
	sessionLock *os.File
}

type Origin struct {
	Channel   string    `json:"channel"`
	Chat      string    `json:"chat"`
	Sender    string    `json:"sender"`
	FirstSeen time.Time `json:"first_seen"`
}

type Session struct {
	ScheduledJob string     `json:"scheduled_job,omitempty"`
	ID           string     `json:"id"`
	User         string     `json:"user"`
	Created      time.Time  `json:"created"`
	Updated      time.Time  `json:"updated"`
	Origins      []Origin   `json:"origins"`
	Inbound      InboundRef `json:"inbound,omitzero"`
}

// InboundRef identifies a message within its channel's ID namespace.
type InboundRef struct {
	Channel string `json:"channel"`
	ID      string `json:"id"`
}

type record struct {
	Time    time.Time        `json:"time"`
	Channel string           `json:"channel"`
	Chat    string           `json:"chat"`
	Sender  string           `json:"sender"`
	Message provider.Message `json:"message"`
	Inbound InboundRef       `json:"inbound,omitzero"`
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open data directory: %w", err)
	}
	if err := root.MkdirAll("sessions", 0700); err != nil {
		root.Close()
		return nil, fmt.Errorf("create sessions directory: %w", err)
	}
	if err := root.MkdirAll("profiles", 0700); err != nil {
		root.Close()
		return nil, fmt.Errorf("create profiles directory: %w", err)
	}
	return &Store{root: root}, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionLock != nil {
		releaseLock(s.sessionLock)
		s.sessionLock = nil
	}
	return s.root.Close()
}

func (s *Store) lockFile(name string) (*os.File, error) {
	file, err := s.root.OpenFile(name, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	if err := lockExclusive(file); err != nil {
		file.Close()
		return nil, fmt.Errorf("lock %s: %w", name, err)
	}
	return file, nil
}

var errDirectoryBusy = errors.New("another Ada process is using the data directory")
var errWriterRequired = errors.New("session writer lock required")

func (s *Store) LockSessions() (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionLock != nil {
		return nil, errDirectoryBusy
	}
	file, err := s.lockFile("sessions.lock")
	if errors.Is(err, errDirectoryBusy) {
		return nil, fmt.Errorf("Ada is serving or chatting: %w", err)
	}
	if err != nil {
		return nil, err
	}
	s.sessionLock = file
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.sessionLock == file {
			releaseLock(file)
			s.sessionLock = nil
		}
	}, nil
}

type userKey struct{}

// WithScheduleFiles holds the cross-process lock only for the file transaction.
func (s *Store) WithScheduleFiles(fn func(*os.Root) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withFiles("schedule", filepath.Join("schedule", "schedule.lock"), fn)
}

// Learning transactions may acquire profile.lock while holding learning.lock.
func (s *Store) WithLearningFiles(fn func(*os.Root) error) error {
	return s.withFiles("learning", "learning.lock", fn)
}

// A separate review slot prevents duplicate model calls without blocking an owner's decisions.
func (s *Store) WithLearningReview(fn func() error) error {
	return s.withFileLock("learning-review.lock", fn)
}

func (s *Store) withFiles(dir, lockPath string, fn func(*os.Root) error) error {
	if err := s.root.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create %s directory: %w", dir, err)
	}
	return s.withFileLock(lockPath, func() error {
		root, err := s.root.OpenRoot(dir)
		if err != nil {
			return fmt.Errorf("open %s directory: %w", dir, err)
		}
		defer root.Close()
		return fn(root)
	})
}

func (s *Store) withFileLock(lockPath string, fn func() error) error {
	file, err := s.lockFile(lockPath)
	// Brief concurrent CLI edits should finish before the next writer gives up.
	for deadline := time.Now().Add(5 * time.Second); errors.Is(err, errDirectoryBusy) && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
		file, err = s.lockFile(lockPath)
	}
	if err != nil {
		return err
	}
	defer releaseLock(file)
	return fn()
}

func WithUser(ctx context.Context, user string) context.Context {
	return context.WithValue(ctx, userKey{}, user)
}

func UserFrom(ctx context.Context) (string, bool) {
	user, _ := ctx.Value(userKey{}).(string)
	if !ValidIdentity(user) {
		return "", false
	}
	return user, true
}

func readJSON(root *os.Root, path string, value any) error {
	data, err := root.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func (s *Store) writeJSON(path string, value any) error {
	return WriteJSON(s.root, path, value)
}

// WriteJSON publishes a synced private file with a same-directory rename.
func WriteJSON(root *os.Root, path string, value any) error {
	tmp := filepath.Join(filepath.Dir(path), ".tmp-"+rand.Text())
	file, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	defer root.Remove(tmp)
	err = json.NewEncoder(file).Encode(value)
	if err == nil {
		err = file.Sync()
	}
	if err = errors.Join(err, file.Close()); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := root.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

func ValidIdentity(id string) bool {
	return len(id) > 0 && len(id) <= 128 && utf8.ValidString(id) && strings.IndexFunc(id, unicode.IsControl) < 0
}

func (s *Store) Link(channel, sender, user string) error {
	if !ValidIdentity(channel) || !ValidIdentity(sender) || !ValidIdentity(user) {
		return fmt.Errorf("invalid identity: expected 1..128 bytes without controls")
	}
	return s.withIdentities(func() error { return s.setLink(channel, sender, user) })
}

func (s *Store) links() (map[string]string, error) {
	links := map[string]string{}
	if err := readJSON(s.root, "identities.json", &links); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if links == nil {
		return nil, fmt.Errorf("invalid identities.json: expected an object")
	}
	return links, nil
}

func (s *Store) setLink(channel, sender, user string) error {
	links, err := s.links()
	if err != nil {
		return err
	}
	key := channel + "\x00" + sender
	if links[key] == user {
		return nil
	}
	if user == "" {
		delete(links, key)
	} else {
		links[key] = user
	}
	return s.writeJSON("identities.json", links)
}

func (s *Store) Resolve(channel, sender string) (string, bool) {
	if !ValidIdentity(channel) || !ValidIdentity(sender) {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var links map[string]string
	if readJSON(s.root, "identities.json", &links) != nil {
		return "", false
	}
	user := links[channel+"\x00"+sender]
	if !ValidIdentity(user) {
		return "", false
	}
	return user, true
}

var sessionID = regexp.MustCompile(`^[0-9A-Za-z-]+$`)

func ValidSessionID(id string) bool {
	return sessionID.MatchString(id) && filepath.IsLocal(id)
}

func sessionPath(id, file string) string { return filepath.Join("sessions", id, file) }

func (s *Store) get(id string) (Session, error) {
	var session Session
	if !ValidSessionID(id) {
		return session, fmt.Errorf("invalid session id")
	}
	data, err := s.root.ReadFile(sessionPath(id, "meta.json"))
	if err == nil {
		err = json.Unmarshal(data, &session)
	}
	if err != nil {
		if pathErr, ok := errors.AsType[*os.PathError](err); ok {
			err = pathErr.Err
		}
		return Session{}, fmt.Errorf("read metadata for session %q: %w", id, err)
	}
	if session.ID != id || !ValidIdentity(session.User) {
		return Session{}, fmt.Errorf("invalid session metadata for %q", id)
	}
	return session, nil
}

func (s *Store) Get(id string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(id)
}

func (s *Store) list(user string, n int) ([]Session, error) {
	dirs, err := fs.ReadDir(s.root.FS(), "sessions")
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	var list []Session
	for _, d := range dirs {
		if !d.IsDir() || !sessionID.MatchString(d.Name()) {
			continue
		}
		item, err := s.get(d.Name())
		if err != nil {
			return nil, err
		}
		if user == "" || item.User == user {
			list = append(list, item)
		}
	}
	slices.SortFunc(list, func(a, b Session) int {
		if cmp := b.Updated.Compare(a.Updated); cmp != 0 {
			return cmp
		}
		return strings.Compare(b.ID, a.ID)
	})
	if n > 0 && len(list) > n {
		list = list[:n]
	}
	return list, nil
}

func (s *Store) List(user string, n int) ([]Session, error) {
	if !ValidIdentity(user) {
		return nil, fmt.Errorf("invalid session user")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.list(user, n)
}

func validOrigin(channel, chat, sender string) bool {
	return ValidIdentity(channel) && ValidIdentity(sender) && chat != "" && utf8.ValidString(chat) && strings.IndexFunc(chat, unicode.IsControl) < 0
}

func (s *Store) Open(user, channel, chat, sender string, idle time.Duration) (Session, error) {
	return s.open(user, channel, chat, sender, idle, "")
}

// OpenScheduled keeps a job's history separate even when the owner chats between runs.
func (s *Store) OpenScheduled(user, job string) (Session, error) {
	return s.open(user, "schedule", job, user, 0, job)
}

func (s *Store) open(user, channel, chat, sender string, idle time.Duration, job string) (Session, error) {
	if !ValidIdentity(user) || !validOrigin(channel, chat, sender) {
		return Session{}, fmt.Errorf("invalid session user or origin")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionLock == nil {
		return Session{}, errWriterRequired
	}
	list, err := s.list(user, 0)
	if err != nil {
		return Session{}, err
	}
	if idle <= 0 {
		idle = DefaultIdle
	}
	now := time.Now().UTC()
	origin := Origin{channel, chat, sender, now}
	list = slices.DeleteFunc(list, func(item Session) bool { return item.ScheduledJob != job })
	if len(list) == 0 {
		return s.createSession(user, origin, InboundRef{}, job)
	}
	session := list[0]
	if err := s.recoverAppend(session); err != nil {
		return Session{}, err
	}
	if job == "" && now.Sub(session.Updated) > idle {
		return s.create(user, origin, InboundRef{})
	}
	for _, o := range session.Origins {
		if o.Channel == channel && o.Chat == chat && o.Sender == sender {
			return session, nil
		}
	}
	session.Origins = append(session.Origins, origin)
	if now.After(session.Updated) {
		session.Updated = now
	}
	return session, s.writeJSON(sessionPath(session.ID, "meta.json"), session)
}

func (s *Store) NewSession(user, channel, chat, sender string) (Session, error) {
	return s.NewSessionForInbound(user, channel, chat, sender, "")
}

func (s *Store) NewSessionForInbound(user, channel, chat, sender, inboundID string) (Session, error) {
	if !ValidIdentity(user) || !validOrigin(channel, chat, sender) {
		return Session{}, fmt.Errorf("invalid session user or origin")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var ref InboundRef
	if inboundID != "" {
		ref = InboundRef{channel, inboundID}
		list, err := s.list(user, 0)
		if err != nil {
			return Session{}, err
		}
		for _, session := range list {
			if session.Inbound == ref {
				return session, nil
			}
		}
	}
	return s.create(user, Origin{channel, chat, sender, time.Now().UTC()}, ref)
}

func (s *Store) create(user string, o Origin, ref InboundRef) (Session, error) {
	return s.createSession(user, o, ref, "")
}

func (s *Store) createSession(user string, o Origin, ref InboundRef, job string) (Session, error) {
	if s.sessionLock == nil {
		return Session{}, errWriterRequired
	}
	id := o.FirstSeen.Format("20060102T150405") + "-" + rand.Text()
	session := Session{ID: id, User: user, Created: o.FirstSeen, Updated: o.FirstSeen, Origins: []Origin{o}, Inbound: ref, ScheduledJob: job}
	// Publish the directory only once both session files exist.
	stage := sessionPath(".new-"+id, "")
	if err := s.root.Mkdir(stage, 0700); err != nil {
		return Session{}, fmt.Errorf("stage session: %w", err)
	}
	defer s.root.RemoveAll(stage)
	if err := s.root.WriteFile(filepath.Join(stage, "messages.jsonl"), nil, 0600); err != nil {
		return Session{}, fmt.Errorf("create message log: %w", err)
	}
	if err := s.writeJSON(filepath.Join(stage, "meta.json"), session); err != nil {
		return Session{}, err
	}
	if err := s.root.Rename(stage, sessionPath(id, "")); err != nil {
		return Session{}, fmt.Errorf("publish session: %w", err)
	}
	return session, nil
}

// The metadata rename commits a turn. Its previous timestamp identifies an unfinished append.
type pendingAppend struct {
	Offset         int64     `json:"offset"`
	PreviousUpdate time.Time `json:"previous_update"`
}

const pendingFile = ".pending-append.json"

func (s *Store) recoverAppend(session Session) error {
	path := sessionPath(session.ID, pendingFile)
	var pending pendingAppend
	err := readJSON(s.root, path, &pending)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read pending append: %w", err)
	}
	if session.Updated.Equal(pending.PreviousUpdate) {
		file, err := s.root.OpenFile(sessionPath(session.ID, "messages.jsonl"), os.O_WRONLY, 0)
		if err != nil {
			return fmt.Errorf("open log for recovery: %w", err)
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return err
		}
		if pending.Offset < 0 || pending.Offset > info.Size() {
			return fmt.Errorf("invalid pending append offset")
		}
		if err := file.Truncate(pending.Offset); err != nil {
			return fmt.Errorf("restore previous turn: %w", err)
		}
		if err := file.Sync(); err != nil {
			return fmt.Errorf("sync restored turn: %w", err)
		}
	}
	if err := s.root.Remove(path); err != nil {
		return fmt.Errorf("clear pending append: %w", err)
	}
	return nil
}

func (s *Store) Append(id, channel, chat, sender string, msgs ...provider.Message) error {
	return s.AppendInbound(id, channel, chat, sender, "", msgs...)
}

func (s *Store) AppendInbound(id, channel, chat, sender, inboundID string, msgs ...provider.Message) error {
	if !validOrigin(channel, chat, sender) {
		return fmt.Errorf("invalid message origin")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionLock == nil {
		return errWriterRequired
	}
	session, err := s.get(id)
	if err != nil {
		return err
	}
	if err := s.recoverAppend(session); err != nil {
		return err
	}
	if len(msgs) == 0 {
		return nil
	}
	path := sessionPath(id, "messages.jsonl")
	file, err := s.root.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open message log: %w", err)
	}
	offset, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		file.Close()
		return fmt.Errorf("locate end of message log: %w", err)
	}
	marker := sessionPath(id, pendingFile)
	if err := s.writeJSON(marker, pendingAppend{offset, session.Updated}); err != nil {
		file.Close()
		return err
	}
	now := time.Now().UTC()
	if !now.After(session.Updated) {
		now = session.Updated.Add(time.Nanosecond)
	}
	enc := json.NewEncoder(file)
	var ref InboundRef
	if inboundID != "" {
		ref = InboundRef{channel, inboundID}
	}
	for _, msg := range msgs {
		err = enc.Encode(record{Time: now, Channel: channel, Chat: chat, Sender: sender, Message: msg, Inbound: ref})
		if err != nil {
			break
		}
	}
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err == nil {
		updated := session
		updated.Updated = now
		if !slices.ContainsFunc(session.Origins, func(o Origin) bool { return o.Channel == channel && o.Chat == chat && o.Sender == sender }) {
			updated.Origins = append(session.Origins, Origin{channel, chat, sender, now})
		}
		err = s.writeJSON(sessionPath(id, "meta.json"), updated)
	}
	if err != nil {
		recoveryErr := s.recoverAppend(session)
		return fmt.Errorf("append session messages: %w", errors.Join(err, recoveryErr))
	}
	// A leftover marker is harmless once the metadata rename succeeds.
	_ = s.root.Remove(marker)
	return nil
}

// AnswerForInbound searches committed exchanges, including sessions since rotated by /new or idleness.
func (s *Store) AnswerForInbound(user string, ref InboundRef) (string, bool, error) {
	if !ValidIdentity(user) || ref.Channel == "" || ref.ID == "" {
		return "", false, fmt.Errorf("invalid inbound reference or user")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.list(user, 0)
	if err != nil {
		return "", false, err
	}
	for _, session := range list {
		if !slices.ContainsFunc(session.Origins, func(o Origin) bool { return o.Channel == ref.Channel }) {
			continue
		}
		var answer string
		var found bool
		err := s.visit(session, func(_ int, r record) bool {
			if r.Inbound == ref && r.Message.Role == "assistant" {
				answer, found = r.Message.Text(), true
			}
			// All records of an exchange were appended together.
			return !found || r.Inbound == ref
		})
		if err != nil || found {
			return answer, found, err
		}
	}
	return "", false, nil
}

// visit reads one JSON line at a time, including messages larger than a scanner buffer.
func (s *Store) visit(session Session, fn func(int, record) bool) error {
	f, err := s.root.Open(sessionPath(session.ID, "messages.jsonl"))
	if err != nil {
		return fmt.Errorf("open message log: %w", err)
	}
	defer f.Close()
	reader := bufio.NewReader(f)
	for i := 0; ; i++ {
		line, err := reader.ReadBytes('\n')
		// A committed record always ends with a newline.
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read message log: %w", err)
		}
		var r record
		if err := json.Unmarshal(line, &r); err != nil {
			return fmt.Errorf("decode session %s line %d: %w", session.ID, i, err)
		}
		if r.Time.After(session.Updated) || !fn(i, r) {
			return nil
		}
	}
}

func userText(m provider.Message) (string, bool) {
	if m.Role == "user" {
		for _, b := range m.Content {
			if b.Type == "text" {
				return b.Text, true
			}
		}
	}
	return "", false
}

func (s *Store) FirstUserLine(id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, err := s.get(id)
	if err != nil {
		return "", err
	}
	var line string
	err = s.visit(session, func(_ int, r record) bool {
		text, ok := userText(r.Message)
		if ok {
			line, _, _ = strings.Cut(text, "\n")
		}
		return !ok
	})
	return line, err
}

// VisitMessages preserves committed log indices and checks ownership before disclosing content.
func (s *Store) VisitMessages(user, id string, visit func(int, provider.Message, bool)) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, err := s.get(id)
	if err != nil {
		return Session{}, err
	}
	if session.User != user {
		return Session{}, fmt.Errorf("session does not belong to the user")
	}
	links, err := s.links()
	if err != nil {
		return Session{}, err
	}
	err = s.visit(session, func(i int, r record) bool {
		linked := ValidIdentity(r.Channel) && ValidIdentity(r.Sender) && links[r.Channel+"\x00"+r.Sender] == user
		visit(i, r.Message, linked)
		return true
	})
	return session, err
}

func (s *Store) History(id string, maxMessages int) ([]provider.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if maxMessages <= 0 {
		maxMessages = DefaultHistory
	}
	session, err := s.get(id)
	if err != nil {
		return nil, err
	}
	var window []provider.Message
	lastUser := 0
	err = s.visit(session, func(_ int, r record) bool {
		if startsTurn(r.Message) {
			lastUser = len(window)
		} else if len(window) == 0 {
			return true
		}
		window = append(window, r.Message)
		// Keep the latest turn's start even after it falls outside the window.
		if drop := min(len(window)-maxMessages, lastUser); drop > 0 {
			clear(window[:drop])
			window = window[drop:]
			lastUser -= drop
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	return HistoryWindow(window, maxMessages), nil
}

func startsTurn(msg provider.Message) bool {
	_, ok := userText(msg)
	return ok && !slices.ContainsFunc(msg.Content, func(b provider.Block) bool { return b.Type == "tool_result" })
}

func HistoryWindow(msgs []provider.Message, n int) []provider.Message {
	if n <= 0 {
		n = DefaultHistory
	}
	start := max(0, len(msgs)-n)
	for i := start; i < len(msgs); i++ {
		if startsTurn(msgs[i]) {
			return msgs[i:]
		}
	}
	for i := start - 1; i >= 0; i-- {
		if startsTurn(msgs[i]) {
			return msgs[i:]
		}
	}
	return nil
}
