package learn

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cristiangirlea/adanext-ada/internal/jsonobject"
	"github.com/cristiangirlea/adanext-ada/provider"
)

// Local models often wrap their JSON in one json fence; nothing else around it is accepted.
func replyJSON(res provider.Response, notText string) ([]byte, error) {
	var b strings.Builder
	for _, block := range res.Content {
		if block.Type != "text" {
			return nil, fmt.Errorf("%s", notText)
		}
		b.WriteString(block.Text)
	}
	raw := strings.TrimSpace(b.String())
	if strings.HasPrefix(raw, "```json") {
		header, body, ok := strings.Cut(raw, "\n")
		body, closed := strings.CutSuffix(body, "\n```")
		if !ok || !closed || strings.TrimSuffix(header, "\r") != "```json" {
			return nil, fmt.Errorf("invalid JSON fence")
		}
		raw = body
	}
	return []byte(raw), nil
}

// The standard decoder otherwise accepts duplicate keys, case variants and null scalars.
func object(data []byte, keys ...string) (map[string]json.RawMessage, error) {
	return objectWithOptional(data, nil, keys...)
}

func objectWithOptional(data []byte, optional []string, keys ...string) (map[string]json.RawMessage, error) {
	return jsonobject.Fields(data, optional, keys...)
}

func messageIndices(raw json.RawMessage) ([]int, error) {
	var values []*int
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	indices := make([]int, len(values))
	for i, n := range values {
		if n == nil {
			return nil, fmt.Errorf("expected integer message index")
		}
		indices[i] = *n
	}
	return indices, nil
}
