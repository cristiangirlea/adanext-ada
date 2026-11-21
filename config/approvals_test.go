package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestApprovalSettings(t *testing.T) {
	for _, tc := range []struct {
		mode, timeout string
		want          time.Duration
		ok            bool
	}{
		{"auto", "17s", 17 * time.Second, true}, {"readonly", "", 10 * time.Minute, true}, {"yolo", "1m", 0, false}, {"auto", "bad", 0, false}, {"auto", "-1m", 0, false},
	} {
		path := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"chat_provider":"local","hrn":{"mode":%q},"approvals":{"timeout":%q}}`, tc.mode, tc.timeout)), 0600); err != nil {
			t.Fatal(err)
		}
		s, err := Load(path)
		if (err == nil) != tc.ok || tc.ok && (s.ApprovalTimeout != tc.want || s.HRN.Mode != tc.mode) {
			t.Fatalf("load(%s,%s) gives timeout %v, err %v", tc.mode, tc.timeout, s.ApprovalTimeout, err)
		}
	}
}
