// Package jsonobject checks the exact fields of JSON objects at trust boundaries.
package jsonobject

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"unicode/utf8"
)

// Fields rejects missing, unknown, duplicate and null fields. Without a field list it accepts arbitrary names.
func Fields(data []byte, optional []string, keys ...string) (map[string]json.RawMessage, error) {
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
		if !ok || (keys != nil && !slices.Contains(optional, key) && !slices.Contains(keys, key)) {
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
