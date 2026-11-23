package learn

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"unicode/utf8"
)

// The standard decoder otherwise accepts duplicate keys, case variants and null scalars.
func object(data []byte, keys ...string) (map[string]json.RawMessage, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("invalid UTF-8 in JSON")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	if tok, err := d.Token(); err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("expected a JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for d.More() {
		tok, err := d.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok || (keys != nil && !slices.Contains(keys, key)) {
			return nil, fmt.Errorf("unexpected JSON field %q", key)
		}
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("duplicate JSON field %q", key)
		}
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			return nil, err
		}
		if string(raw) == "null" {
			return nil, fmt.Errorf("null JSON field %q", key)
		}
		fields[key] = raw
	}
	if _, err := d.Token(); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, fmt.Errorf("unexpected content after JSON object")
	}
	for _, key := range keys {
		if _, ok := fields[key]; !ok {
			return nil, fmt.Errorf("missing JSON field %q", key)
		}
	}
	return fields, nil
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
