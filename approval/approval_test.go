package approval

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestLogAppendsPrivateOwnerRecords(t *testing.T) {
	dir := t.TempDir()
	log := Log(dir)
	now := time.Now().UTC()
	for i, user := range []string{"owner", "someone", "owner"} {
		r := Request{Session: "abc-123", User: user, Summary: "line one\nline two"}
		d := Decision{By: "cli", At: now.Add(time.Duration(i) * time.Second), Reason: "denied"}
		if err := log.Record(r, d); err != nil {
			t.Fatal(err)
		}
	}
	for _, session := range []string{"../else", "/escape", "a.jsonl", "."} {
		if err := log.Record(Request{Session: session}, Decision{}); err == nil {
			t.Fatalf("accepted %q", session)
		}
	}
	records, err := log.Latest("owner", []string{"abc-123"}, 1)
	if err != nil || len(records) != 1 || !records[0].Decision.At.Equal(now.Add(2*time.Second)) || records[0].Request.Summary != "line one\nline two" {
		t.Fatalf("latest = %+v, %v", records, err)
	}
	if records, err = log.Latest("owner", []string{"abc-123"}, 20); err != nil || len(records) != 2 {
		t.Fatalf("records = %+v, %v", records, err)
	}
	if err := log.Record(Request{User: "owner"}, Decision{Reason: "no approver"}); err != nil {
		t.Fatal(err)
	}
	for name, perm := range map[string]os.FileMode{"approvals": 0700, "approvals/none.jsonl": 0600, "approvals/abc-123.jsonl": 0600} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != perm {
			t.Fatalf("%s permissions %v", name, info.Mode())
		}
	}
}

func TestLogRecoversInterruptedAppend(t *testing.T) {
	dir := t.TempDir()
	log := Log(dir)
	req := Request{Session: "s1", User: "owner"}
	d := Decision{Reason: "denied", At: time.Now().UTC()}
	if err := log.Record(req, d); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "approvals", "s1.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"request":`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := log.Record(req, d); err != nil {
		t.Fatal(err)
	}
	records, err := log.Latest("owner", []string{"s1"}, 3)
	if err != nil || len(records) != 2 {
		t.Fatalf("records = %+v, %v", records, err)
	}
}
