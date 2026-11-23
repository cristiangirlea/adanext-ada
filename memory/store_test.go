package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cristiangirlea/adanext-ada/provider"
)

func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	unlock, err := s.LockSessions()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
	return s, dir
}

func message(role, kind, text string) provider.Message {
	return provider.Message{Role: role, Content: []provider.Block{{Type: kind, Text: text}}}
}

func TestSessionContinuity(t *testing.T) {
	s, dir := testStore(t)
	for _, c := range []string{"cli", "telegram"} {
		if err := s.Link(c, "sender", "owner"); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := s.Resolve("absent", "sender"); ok {
		t.Fatal("unknown link")
	}
	var id string
	for i, c := range []string{"cli", "telegram", "telegram"} {
		user, ok := s.Resolve(c, "sender")
		if !ok || user != "owner" {
			t.Fatal(user, ok)
		}
		session, err := s.Open(user, c, "local", "sender", 0)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			id = session.ID
		}
		if session.ID != id || len(session.Origins) != min(i+1, 2) {
			t.Fatal(session)
		}
	}
	other, err := s.Open("other", "cli", "local", "other", 0)
	if err != nil || other.ID == id {
		t.Fatal(other, err)
	}
	expired, err := s.Open("owner", "cli", "local", "sender", time.Nanosecond)
	if err != nil || expired.ID == id {
		t.Fatal(expired, err)
	}
	forced, err := s.NewSession("owner", "cli", "local", "sender")
	if err != nil || forced.ID == expired.ID {
		t.Fatal(forced, err)
	}
	list, err := s.List("owner", 2)
	if err != nil || len(list) != 2 || list[0].ID != forced.ID {
		t.Fatal(list, err)
	}
	if err := s.Link("cli\x00", "sender", "owner"); err == nil {
		t.Fatal("invalid channel")
	}
	if err := s.Link("cli", strings.Repeat("a", 129), "owner"); err == nil {
		t.Fatal("long sender")
	}
	for _, bad := range []string{"../x", "", "a/b", "."} {
		if _, err := s.Get(bad); err == nil {
			t.Fatal(bad)
		}
		if err := s.Append(bad, "cli", "local", "sender", message("user", "text", "x")); err == nil {
			t.Fatal(bad)
		}
		if _, err := s.History(bad, 1); err == nil {
			t.Fatal(bad)
		}
	}
	if runtime.GOOS != "windows" {
		for _, path := range []string{"identities.json", "sessions/" + id + "/meta.json", "sessions/" + id + "/messages.jsonl"} {
			info, err := os.Stat(filepath.Join(dir, path))
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal(path, info, err)
			}
		}
	}
}

func TestUncommittedAppendRecovery(t *testing.T) {
	s, dir := testStore(t)
	session, err := s.NewSession("owner", "cli", "local", "owner")
	if err != nil {
		t.Fatal(err)
	}
	appendText := func(text string) {
		t.Helper()
		if err := s.Append(session.ID, "cli", "local", "owner", message("user", "text", text)); err != nil {
			t.Fatal(err)
		}
	}
	appendText("kept")
	session, err = s.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "sessions", session.ID, "messages.jsonl")
	info, err := os.Stat(log)
	if err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(filepath.Dir(log), ".pending-append.json")
	data, err := json.Marshal(map[string]any{"offset": info.Size(), "previous_update": session.Updated})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pending, data, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	unfinished := record{Time: session.Updated.Add(time.Second), Message: message("user", "text", "unfinished")}
	if err := json.NewEncoder(f).Encode(unfinished); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	reader, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	before, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Get(session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.List("owner", 1); err != nil {
		t.Fatal(err)
	}
	if line, err := reader.FirstUserLine(session.ID); err != nil || line != "kept" {
		t.Fatal(line, err)
	}
	hits, err := reader.Search("owner", "unfinished", 1, "")
	if err != nil || len(hits) != 0 {
		t.Fatal(hits, err)
	}
	history, err := reader.History(session.ID, 40)
	after, readErr := os.ReadFile(log)
	if readErr != nil || string(after) != string(before) {
		t.Fatal("reader modified message log", readErr)
	}
	if _, err := os.Stat(pending); err != nil {
		t.Fatal("reader removed marker", err)
	}
	if err != nil || len(history) != 1 {
		t.Fatal(history, err)
	}
	appendText("also kept")
	// A crash after the metadata rename must preserve the committed turn.
	if err := os.WriteFile(pending, data, 0600); err != nil {
		t.Fatal(err)
	}
	history, err = s.History(session.ID, 40)
	if err != nil || len(history) != 2 {
		t.Fatal(history, err)
	}
	if _, err := s.Open("owner", "cli", "local", "owner", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pending); !os.IsNotExist(err) {
		t.Fatal("unexpected pending append", err)
	}
}

func TestProfileRejectsCorruptFile(t *testing.T) {
	s, dir := testStore(t)
	for _, content := range []string{`{"facts":[{"text":"unsafe"},4]}`, `{"facts":[{"text":"one\ntwo"}]}`, `{"facts":[{"text":"` + strings.Repeat("x", 301) + `"}]}`} {
		if err := os.WriteFile(filepath.Join(dir, "profiles", "owner.json"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if got := s.Render("owner"); got != "" {
			t.Fatal("rendered invalid profile", got)
		}
		if _, err := s.ReadFacts("owner"); err == nil {
			t.Fatal("accepted invalid profile")
		}
	}
}

func TestHistoryWindow(t *testing.T) {
	s, _ := testStore(t)
	session, err := s.NewSession("owner", "cli", "local", "sender")
	if err != nil {
		t.Fatal(err)
	}
	err = s.Append(session.ID, "cli", "local", "sender", message("user", "text", "old"), message("assistant", "text", "a"), message("user", "tool_result", "r"), message("assistant", "text", "b"), message("user", "text", "new"), message("assistant", "text", "answer"))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{1, 3, 4, 5} {
		h, err := s.History(session.ID, n)
		if err != nil || len(h) != 2 || h[0].Content[0].Text != "new" {
			t.Fatal(h, err)
		}
	}
}

func TestProfileLimits(t *testing.T) {
	s, dir := testStore(t)
	if s.Render("owner") != "" {
		t.Fatal(s.Render("owner"))
	}
	f, err := s.AddFact("owner", "uses Go", "cli")
	if err != nil || s.Render("owner") != "Facts about the user (kept by the user):\n- uses Go\n" {
		t.Fatal(f, err, s.Render("owner"))
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, "profiles", "owner.json"))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal(info, err)
		}
	}
	if err := s.RemoveFact("owner", f.ID); err != nil || len(s.Facts("owner")) != 0 {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "x\ny", "\tx", "x\x7f"} {
		if _, err := s.AddFact("owner", bad, "cli"); err == nil {
			t.Fatal(bad)
		}
	}
	check := func(text, limit string) {
		t.Helper()
		if _, err := s.AddFact("owner", text, "cli"); err == nil || !strings.Contains(err.Error(), limit) {
			t.Fatal(limit, err)
		}
	}
	check(strings.Repeat("é", 151), "300")
	for range 40 {
		if _, err := s.AddFact("owner", strings.Repeat("x", 100), "cli"); err != nil {
			t.Fatal(err)
		}
	}
	check("x", "40")
	if err := s.RemoveFact("owner", s.Facts("owner")[0].ID); err != nil {
		t.Fatal(err)
	}
	check(strings.Repeat("x", 101), "4000")
}

func TestSessionConfinement(t *testing.T) {
	s, dir := testStore(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "meta.json"), []byte(`{"id":"linked","user":"owner"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "sessions", "linked")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := s.Get("linked"); err == nil {
		t.Fatal("followed external directory")
	}
}

func TestConcurrentAppendAndRollback(t *testing.T) {
	s, _ := testStore(t)
	item, err := s.NewSession("owner", "cli", "local", "owner")
	if err != nil {
		t.Fatal(err)
	}
	failures := make(chan error, 12)
	for i := range cap(failures) {
		go func() { failures <- s.Append(item.ID, "cli", "local", "owner", message("user", "text", fmt.Sprint(i))) }()
	}
	for range cap(failures) {
		if err := <-failures; err != nil {
			t.Fatal(err)
		}
	}
	bad := provider.Message{Role: "assistant", Content: []provider.Block{{Type: "tool_use", Input: json.RawMessage("{")}}}
	if err := s.Append(item.ID, "cli", "local", "owner", message("user", "text", "rollback"), bad); err == nil {
		t.Fatal("committed malformed message")
	}
	history, err := s.History(item.ID, 40)
	if err != nil || len(history) != 12 {
		t.Fatal(history, err)
	}
}

func TestMixedToolResultCannotStartHistory(t *testing.T) {
	mixed := provider.Message{Role: "user", Content: []provider.Block{{Type: "tool_result", Text: "orphaned result"}, {Type: "text", Text: "also text"}}}
	next := message("user", "text", "valid")
	window := HistoryWindow([]provider.Message{mixed, next}, 2)
	if len(window) != 1 || window[0].Content[0].Text != "valid" {
		t.Fatal(window)
	}
}

func TestCanceledRanking(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := rankHits(ctx, []candidate{{tf: map[string]int{"match": 1}}}, map[string]int{"match": 1}, 1, 8)
	if err != context.Canceled {
		t.Fatal(err)
	}
}
func TestRunSearchBounds(t *testing.T) {
	s, _ := testStore(t)
	home := t.TempDir()
	runs := filepath.Join(home, "runs")
	t.Setenv("HRN_HOME", home)
	if HRNRunsDir(nil) != runs || HRNRunsDir(os.Getenv) != runs {
		t.Fatal("unexpected hrn runs directory")
	}
	for i := range 501 {
		d := filepath.Join(runs, fmt.Sprintf("%03d", i))
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
		if i <= 25 {
			if err := os.WriteFile(filepath.Join(d, "task.json"), []byte(`{"prompt":"find marker"}`), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, tc := range []struct{ limit, want int }{{0, 8}, {100, 20}} {
		hits, err := s.Search("owner", "find", tc.limit, runs)
		if err != nil || len(hits) != tc.want {
			t.Fatal(hits, err)
		}
	}
	if err := os.WriteFile(filepath.Join(runs, "000", "task.json"), []byte(`{"prompt":"excluded"}`), 0600); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Search("owner", "excluded", 20, runs)
	if err != nil || len(hits) != 0 {
		t.Fatal(hits, err)
	}
}

func TestSearchCorpus(t *testing.T) {
	s, _ := testStore(t)
	session, err := s.NewSession("owner", "cli", "local", "sender")
	if err != nil {
		t.Fatal(err)
	}
	txt := strings.Repeat("文", 120) + "  Écureuil  " + strings.Repeat("文", 110)
	err = s.Append(session.ID, "cli", "local", "sender", message("user", "text", "common"), message("user", "tool_result", txt), message("assistant", "text", "common"))
	if err != nil {
		t.Fatal(err)
	}
	runs := t.TempDir()
	for id, files := range map[string][]string{"001": {`{"prompt":"common acorn"}`, `{"final":"hazelnut","status":"done"}`}, "002": {`{"prompt":"broken"`, `{"final":"oversize"}` + strings.Repeat(" ", 1<<20)}} {
		d := filepath.Join(runs, id)
		if err := os.Mkdir(d, 0700); err != nil {
			t.Fatal(err)
		}
		for i, name := range []string{"task.json", "result.json"} {
			if err := os.WriteFile(filepath.Join(d, name), []byte(files[i]), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	h, err := s.Search("owner", "COMMON écureuil common", 0, runs)
	if err != nil || len(h) != 4 {
		t.Fatal(h, err)
	}
	if h[0].Ref != fmt.Sprintf("session:%s#1", session.ID) || len(h[0].Excerpt) > 300 || !utf8.ValidString(h[0].Excerpt) || !strings.Contains(h[0].Excerpt, "Écureuil") {
		t.Fatal(h[0])
	}
	for _, q := range []string{"acorn", "hazelnut", "done"} {
		h, err = s.Search("other", q, 1, runs)
		if err != nil || len(h) != 1 || h[0].Ref != "hrn:001" {
			t.Fatal(q, h, err)
		}
	}
	h, err = s.Search("owner", "broken oversize", 20, runs)
	if err != nil || len(h) != 0 {
		t.Fatal(h, err)
	}
}

func TestUnreadableSessionMetadata(t *testing.T) {
	s, dir := testStore(t)
	session, err := s.NewSession("owner", "cli", "local", "owner")
	if err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(dir, sessionPath(session.ID, "meta.json"))
	if err := os.Remove(meta); err != nil {
		t.Fatal(err)
	}
	for _, corrupt := range []bool{false, true} {
		if corrupt {
			if err := os.WriteFile(meta, []byte("{"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		_, listErr := s.List("owner", 1)
		_, openErr := s.Open("owner", "cli", "local", "owner", 0)
		_, searchErr := s.Search("owner", "needle", 1, "")
		for _, err := range []error{listErr, openErr, searchErr} {
			if err == nil || !strings.Contains(err.Error(), session.ID) || strings.ContainsAny(err.Error(), `/\`) || strings.Contains(err.Error(), "meta.json") {
				t.Errorf("metadata error = %v", err)
			}
		}
	}
}

func TestSessionDirectoryPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions")
	}
	s, dir := testStore(t)
	session, err := s.NewSession("owner", "cli", "local", "owner")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"sessions", "profiles", sessionPath(session.ID, "")} {
		info, err := os.Stat(filepath.Join(dir, path))
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatal(path, info, err)
		}
	}
}

func TestSessionWriterLock(t *testing.T) {
	dir := t.TempDir()
	writer, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	second, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	unlock, err := writer.LockSessions()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	session, err := writer.NewSession("owner", "cli", "local", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if release, err := second.LockSessions(); err == nil {
		release()
		t.Fatal("second writer acquired lock")
	} else if !strings.Contains(err.Error(), "another Ada process is using the data directory") {
		t.Fatal(err)
	}
	if _, err := second.NewSession("owner", "cli", "local", "owner"); err == nil {
		t.Fatal("unlocked create")
	}
	if _, err := second.Open("owner", "cli", "local", "owner", 0); err == nil {
		t.Fatal("unlocked open")
	}
	if err := second.Append(session.ID, "cli", "local", "owner", message("user", "text", "bad")); err == nil {
		t.Fatal("unlocked append")
	}
	unlock()
	release, err := second.LockSessions()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := writer.Open("owner", "cli", "local", "owner", 0); err == nil {
		t.Fatal("write after release")
	}
}

func TestSearchSkipsOtherUsers(t *testing.T) {
	s, _ := testStore(t)
	var own string
	for _, user := range []string{"alice", "bob"} {
		session, err := s.NewSession(user, "cli", "local", user)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Append(session.ID, "cli", "local", user, message("user", "text", "shared query")); err != nil {
			t.Fatal(err)
		}
		if user == "alice" {
			own = session.ID
		}
	}
	hits, err := s.Search("alice", "shared", 8, "")
	if err != nil || len(hits) != 1 || hits[0].Ref != "session:"+own+"#0" {
		t.Fatal(hits, err)
	}
}
func TestProfileIdentity(t *testing.T) {
	s, _ := testStore(t)
	for _, user := range []string{"", "invalid\nuser", strings.Repeat("x", 129)} {
		if _, err := s.AddFact(user, "fact", "cli"); err == nil {
			t.Fatal("accepted invalid identity")
		}
	}
	users := []string{"alice", "Alice", "../alice", "con", "a:b"}
	for _, user := range users {
		if _, err := s.AddFact(user, user, "cli"); err != nil {
			t.Fatal(err)
		}
	}
	for _, user := range users {
		if facts := s.Facts(user); len(facts) != 1 || facts[0].Text != user {
			t.Fatal(user, facts)
		}
	}
}
func TestProfileWriterLock(t *testing.T) {
	s, dir := testStore(t)
	fact, err := s.AddFact("owner", "saved", "cli")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	lock, err := s.lockFile("profile.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseLock(lock)
	if _, err := second.AddFact("owner", "new", "cli"); err == nil {
		t.Fatal("added while locked")
	}
	if err := second.RemoveFact("owner", fact.ID); err == nil {
		t.Fatal("removed while locked")
	}
	if facts := second.Facts("owner"); len(facts) != 1 {
		t.Fatal(facts)
	}
}

func TestLockFileInAMissingDirectoryStillFails(t *testing.T) {
	s, _ := testStore(t)
	if _, err := s.lockFile(filepath.Join("absent", "x.lock")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("got %v, want a missing-file error", err)
	}
}
