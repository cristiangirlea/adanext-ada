package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cristiangirlea/adanext-ada/hrnexec"
	"github.com/cristiangirlea/adanext-ada/memory"
	"github.com/cristiangirlea/adanext-ada/provider"
)

type SearchMemory struct {
	Store   *memory.Store
	HRNRuns string
}

func (SearchMemory) Spec() provider.ToolSpec {
	return provider.ToolSpec{Name: "search_memory", Description: "Search past session messages and local hrn run records.", InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}},"required":["query"],"additionalProperties":false}`)}
}

func (SearchMemory) Effect() Effect { return Read }

func (t SearchMemory) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	var input struct {
		Query string
		Limit int
	}
	if err := decodeInput(raw, &input); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	user, ok := memory.UserFrom(ctx)
	if !ok {
		return "", fmt.Errorf("search_memory requires a user in the context")
	}
	hits, err := t.Store.SearchContext(ctx, user, input.Query, input.Limit, t.HRNRuns)
	if err != nil {
		return "", err
	}
	return FormatHits(hits), nil
}

func FormatHits(hits []memory.Hit) string {
	var blocks []string
	for _, h := range hits {
		blocks = append(blocks, fmt.Sprintf("%s  %s  %s\n%s", h.Ref, h.Kind, h.Time.Format(time.RFC3339Nano), h.Excerpt))
	}
	return strings.Join(blocks, "\n\n")
}

type SearchKnowledge struct {
	Options hrnexec.Options
	Request hrnexec.KnowledgeRequest
}

func (SearchKnowledge) Spec() provider.ToolSpec {
	return provider.ToolSpec{Name: "search_knowledge", Description: "Search project knowledge through hrn.", InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`)}
}

func (SearchKnowledge) Effect() Effect { return Read }

func (t SearchKnowledge) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	var input struct{ Query string }
	if err := decodeInput(raw, &input); err != nil {
		return "", err
	}
	if strings.TrimSpace(input.Query) == "" {
		return "", fmt.Errorf("knowledge query is empty")
	}
	req := t.Request
	req.Query = input.Query
	packet, err := hrnexec.SearchKnowledge(ctx, t.Options, req)
	if err != nil {
		return "", err
	}
	if len(packet.Entries) == 0 {
		return packet.EmptyReason, nil
	}
	var blocks []string
	for _, e := range packet.Entries {
		label := e.Key
		if e.Excerpt {
			label += " (excerpt)"
		}
		blocks = append(blocks, fmt.Sprintf("%s  %s\n%s", label, e.Source, e.Content))
	}
	return strings.Join(blocks, "\n\n"), nil
}
