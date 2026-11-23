package schedule

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/cristiangirlea/adanext-ada/memory"
)

func TestConcurrentEditsAndInterruptedRunAppend(t *testing.T) {
	dir := t.TempDir()
	var group sync.WaitGroup
	for i := range 8 {
		m, err := memory.NewStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { m.Close() })
		group.Go(func() {
			_, err := (Store{m}).Add(Job{Name: fmt.Sprint(i), Kind: "prompt", Spec: "every 5m", Prompt: "report"}, time.Now())
			if err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	m, err := memory.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	store := Store{m}
	jobs, err := store.List()
	if err != nil || len(jobs) != 8 {
		t.Fatal(jobs, err)
	}
	for i := range 5 {
		if i == 3 {
			err = m.WithScheduleFiles(func(root *os.Root) error {
				f, err := root.OpenFile("runs.jsonl", os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					return err
				}
				defer f.Close()
				_, err = f.WriteString("{")
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		if err = store.record(Run{Job: jobs[i].ID, Output: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := store.Runs(3)
	if err != nil || len(runs) != 3 {
		t.Fatal(runs, err)
	}
	for i, r := range runs {
		if r.Output != fmt.Sprint(4-i) {
			t.Fatal(runs)
		}
	}
}
