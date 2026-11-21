package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/adanext-ada/hrnexec"
	"github.com/cristiangirlea/adanext-ada/memory"
	"github.com/cristiangirlea/adanext-ada/provider"
)

func init() {
	if len(os.Args) != 4 || os.Args[1] != "knowledge" || os.Args[2] != "search" || os.Getenv("ADA_TEST_TOOL_KNOWLEDGE") == "" {
		return
	}
	data, err := os.ReadFile(os.Args[3])
	var req hrnexec.KnowledgeRequest
	if err != nil || json.Unmarshal(data, &req) != nil {
		os.Exit(2)
	}
	switch req.Query {
	case "present":
		fmt.Print(`{"entries":[{"key":"one","source":"notes","content":"first","excerpt":true},{"key":"two","source":"manual","content":"second"}]}`)
	case "absent":
		fmt.Print(`{"empty_reason":"no matching entries"}`)
	default:
		os.Exit(3)
	}
	os.Exit(0)
}

func TestMemoryTools(t *testing.T) {
	store, err := memory.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	unlock, err := store.LockSessions()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	session, err := store.NewSession("owner", "cli", "local", "owner")
	if err != nil {
		t.Fatal(err)
	}
	msg := provider.Message{Role: "user", Content: []provider.Block{{Type: "text", Text: "apple pear"}}}
	if err = store.Append(session.ID, "cli", "local", "owner", msg); err != nil {
		t.Fatal(err)
	}
	search := SearchMemory{Store: store, HRNRuns: t.TempDir()}

	if _, err := search.Run(t.Context(), []byte(`{"query":"pear"}`)); err == nil {
		t.Fatal("anonymous search allowed")
	}
	other, err := search.Run(memory.WithUser(t.Context(), "other"), []byte(`{"query":"pear"}`))
	if err != nil || other != "" {
		t.Fatal(other, err)
	}
	got, err := search.Run(memory.WithUser(t.Context(), "owner"), []byte(`{"query":"pear","limit":1}`))
	meta, metaErr := store.Get(session.ID)
	if err != nil || metaErr != nil || !strings.Contains(got, "session:"+session.ID+"#0") || !strings.Contains(got, meta.Updated.Format(time.RFC3339Nano)) || !strings.Contains(got, "apple pear") {
		t.Fatal(got, err, metaErr)
	}
	t.Setenv("ADA_TEST_TOOL_KNOWLEDGE", "1")
	knowledge := SearchKnowledge{Options: hrnexec.Options{HRNPath: os.Args[0]}}
	for _, tc := range []struct{ q, want string }{{"present", "one (excerpt)  notes\nfirst\n\ntwo  manual\nsecond"}, {"absent", "no matching entries"}} {
		raw, _ := json.Marshal(map[string]string{"query": tc.q})
		got, err := knowledge.Run(t.Context(), raw)
		if err != nil || got != tc.want {
			t.Fatal(got, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = store.SearchContext(ctx, "owner", "pear", 0, ""); err != context.Canceled {
		t.Fatal(err)
	}
}
