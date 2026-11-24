package tool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/adanext-ada/hrnexec"
)

func TestAutoDelegateRequiresExactApproval(t *testing.T) {
	dir, other := t.TempDir(), t.TempDir()
	d := Delegate{Mode: "auto", Roots: []string{dir, other}, Backend: "configured", Options: hrnexec.Options{HRNPath: os.Args[0]}}
	task := func(dir string) json.RawMessage {
		raw, err := json.Marshal(map[string]string{"prompt": "change the code", "purpose": "implement", "dir": dir})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	summary, err := d.Summary(task(other))
	if err != nil {
		t.Fatal(err)
	}
	withSkills := d
	withSkills.Skills = true
	skillSummary, err := withSkills.Summary(task(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADA_TEST_DELEGATE", "auto")
	for name, ctx := range map[string]context.Context{
		"no approval":         t.Context(),
		"other skill setting": context.WithValue(t.Context(), approvedSummaryKey{}, skillSummary),
		"other directory":     context.WithValue(t.Context(), approvedSummaryKey{}, summary),
	} {
		t.Run(name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "started")
			t.Setenv("ADA_TEST_DELEGATE_STARTED", marker)
			if _, err := d.Run(ctx, task(dir)); err == nil || !strings.Contains(err.Error(), "requires approval for this exact task") {
				t.Errorf("Run = %v", err)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("fake hrn start marker: stat = %v", err)
			}
		})
	}
}
