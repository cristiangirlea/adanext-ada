package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLearningDurations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	for _, tc := range []struct {
		raw            string
		stale, archive time.Duration
	}{
		{"{}", 2160 * time.Hour, 720 * time.Hour}, {`{"stale_after":"48h","archive_after":"5m"}`, 48 * time.Hour, 5 * time.Minute},
		{`{"stale_after":"oops"}`, -1, 0}, {`{"archive_after":"-1s"}`, -1, 0},
	} {
		if err := os.WriteFile(path, fmt.Appendf(nil, `{"chat_provider":"fake","learning":%s}`, tc.raw), 0600); err != nil {
			t.Fatal(err)
		}
		s, err := Load(path)
		if tc.stale < 0 {
			if err == nil {
				t.Fatal(tc.raw)
			}
			continue
		}
		if err != nil || s.Learning.StaleAfter != tc.stale || s.Learning.ArchiveAfter != tc.archive {
			t.Fatal(tc.raw, s.Learning, err)
		}
	}
}
