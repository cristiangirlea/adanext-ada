package tool

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cristiangirlea/adanext-ada/provider"
)

type registryTestTool struct {
	name   string
	effect Effect
}

func (r registryTestTool) Spec() provider.ToolSpec                              { return provider.ToolSpec{Name: r.name} }
func (r registryTestTool) Effect() Effect                                       { return r.effect }
func (r registryTestTool) Run(context.Context, json.RawMessage) (string, error) { return r.name, nil }

func TestRegistry(t *testing.T) {
	r := new(Registry)
	for _, item := range []registryTestTool{{"z", Read}, {"a", DelegateEffect}} {
		if err := r.Add(item); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Add(registryTestTool{"a", Read}); err == nil {
		t.Fatal("registered duplicate")
	}
	if err := r.Add(registryTestTool{"other", "unknown"}); err == nil {
		t.Fatal("registered unclassified effect")
	}
	if got := r.Specs(); len(got) != 2 || got[0].Name != "a" || got[1].Name != "z" {
		t.Fatalf("Specs = %v", got)
	}
	if got, err := r.Run(t.Context(), "a", nil); err != nil || got != "a" {
		t.Fatalf("Run = %q, %v", got, err)
	}
	if _, err := r.Run(t.Context(), "missing", nil); err == nil {
		t.Fatal("ran unknown tool")
	}
}
