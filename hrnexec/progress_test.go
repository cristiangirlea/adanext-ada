package hrnexec

import (
	"bytes"
	"strings"
	"testing"
)

func TestEvalProgressRedactsAcrossWrites(t *testing.T) {
	var out bytes.Buffer
	w := newProgressWriter(&out, []string{"HRN_TENANT_TOKEN=fixture-tenant", "HRN_SERVER_URL=fixture-url"})
	for _, s := range []string{"one\nfix", "ture-tenant\ntwo\nfixture-u", "rl\n"} {
		if _, err := w.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	if out.String() != "one\n[redacted]\ntwo\n[redacted]\n" {
		t.Fatal(out.String())
	}
}

func TestEvalProgressCarriageReturns(t *testing.T) {
	var out bytes.Buffer
	w := newProgressWriter(&out, nil)
	w.Write([]byte("first\rsecond\rthird\r"))
	if out.String() != "first\nsecond\nthird\n" {
		t.Fatalf("progress = %q", out.String())
	}
}

func TestEvalProgressBoundsPartialLine(t *testing.T) {
	var out bytes.Buffer
	w := newProgressWriter(&out, []string{"HRN_TENANT_TOKEN=fixture-tenant"})
	data := strings.Repeat("x", 4095) + "é" + strings.Repeat("x", (1<<20)-4097)
	for start := 0; start < len(data); start += 8191 {
		end := min(len(data), start+8191)
		if _, err := w.Write([]byte(data[start:end])); err != nil {
			t.Fatal(err)
		}
		if held := end - out.Len(); held > 4096 {
			t.Fatalf("held %d bytes", held)
		}
	}
	if err := w.flush(); err != nil || out.String() != data {
		t.Fatalf("wrote %d bytes, want %d: %v", out.Len(), len(data), err)
	}
}

func TestEvalProgressRedactsForcedFlush(t *testing.T) {
	for _, end := range []string{"fixture-tenant", "fix"} {
		var out bytes.Buffer
		w := newProgressWriter(&out, []string{"HRN_TENANT_TOKEN=fixture-tenant"})
		w.Write([]byte(strings.Repeat("x", 4096-len(end)) + end))
		if out.String() != "[redacted]" {
			t.Fatalf("forced flush with %q wrote %d bytes", end, out.Len())
		}
		w.Write([]byte(strings.TrimPrefix("fixture-tenant", end) + " done\n"))
		if out.String() != "[redacted] done\n" {
			t.Fatalf("progress = %q", out.String())
		}
	}
}

func TestEvalProgressSecretLongerThanBuffer(t *testing.T) {
	var out bytes.Buffer
	secret := strings.Repeat("ab", 5000) + "z"
	w := newProgressWriter(&out, []string{"HRN_TENANT_TOKEN=" + secret})
	for _, part := range []string{secret[:4095], secret[4095:8191], secret[8191:], " done\r", "\n"} {
		if _, err := w.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
		if len(w.line) > 4096 {
			t.Fatalf("held %d bytes", len(w.line))
		}
	}
	if got := strings.ReplaceAll(out.String(), "[redacted]", ""); got != " done\n" {
		t.Fatalf("unexpected progress after redaction (%d bytes)", len(got))
	}
}

func TestEvalProgressEscapesCompleteLines(t *testing.T) {
	var out bytes.Buffer
	w := newProgressWriter(&out, nil)
	w.Write([]byte("first\x1b"))
	if out.Len() != 0 {
		t.Errorf("partial line shown: %q", out.String())
	}
	w.Write([]byte("[2J\r\nlast\u202e"))
	if got := out.String(); got != "first\\x1b[2J\n" {
		t.Errorf("progress = %q", got)
	}
	if err := w.flush(); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "first\\x1b[2J\nlast\\u202e" {
		t.Fatal(got)
	}
}
