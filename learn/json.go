package learn

import (
	"encoding/json"
	"fmt"

	"github.com/cristiangirlea/adanext-ada/internal/jsonobject"
)

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
