package provider

import (
	"strings"
	"testing"
)

func TestEndpointClassification(t *testing.T) {
	for _, tc := range []struct {
		url   string
		local bool
	}{
		{"http://localhost:1234", true}, {"https://LOCALHOST/v2", true},
		{"http://127.0.0.1", true}, {"http://127.42.0.1", true},
		{"http://[::1]", true}, {"http://10.1.2.3", true}, {"http://172.16.0.1", true},
		{"http://172.31.0.1", true}, {"http://172.32.0.1", false},
		{"http://192.168.10.1", true}, {"http://[fc00::1]", true},
		{"http://[fd00::1]", true}, {"http://[fe80::1]", false},
		{"https://example.com", false}, {"http://127.0.0.1.nip.io", false},
		{"http://guest@localhost", false}, {"http://localhost?foo=bar", false},
		{"http://localhost?", false}, {"http://localhost#", false},
		{"ftp://localhost", false}, {"http://[::ffff:127.0.0.1]", false},
	} {
		t.Run(tc.url, func(t *testing.T) {
			if got := isLocal(tc.url); got != tc.local {
				t.Errorf("isLocal = %v, want %v", got, tc.local)
			}
		})
	}
}

func TestNewBillingPolicy(t *testing.T) {
	cfg := Config{Name: "remote", Kind: "anthropic", BaseURL: "https://example.com", APIKeyEnv: "ADA_TEST_KEY"}
	lookups := 0
	getenv := func(name string) string {
		lookups++
		if name != cfg.APIKeyEnv {
			t.Fatalf("unexpected lookup: %q", name)
		}
		return "test-credential"
	}
	if _, err := New(cfg, false, getenv); err == nil || !strings.Contains(err.Error(), "remote") || !strings.Contains(err.Error(), "allow_metered") || lookups != 0 {
		t.Fatalf("refusal = %v, lookups = %d", err, lookups)
	}
	if p, err := New(cfg, true, getenv); err != nil || p.Name() != "remote" || lookups != 1 {
		t.Fatalf("New = %v, %v, lookups = %d", p, err, lookups)
	}
	if _, err := New(cfg, true, func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), cfg.APIKeyEnv) {
		t.Fatalf("empty key = %v", err)
	}
	cfg.Kind = "unknown"
	if _, err := New(cfg, true, getenv); err == nil {
		t.Fatal("unknown provider kind accepted")
	}
}
