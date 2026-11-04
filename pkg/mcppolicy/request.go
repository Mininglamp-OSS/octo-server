package mcppolicy

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"unicode/utf8"
)

type Mutation struct {
	Mode       Mode
	Generation uint64
}

// ParseMutation enforces the closed management schema, including decoded
// duplicate keys, a 2 KiB cap, and lossless generation strings.
func ParseMutation(r io.Reader, deleteOverride bool) (Mutation, error) {
	data, err := io.ReadAll(io.LimitReader(r, 2049))
	if err != nil || len(data) > 2048 || !utf8.Valid(data) {
		return Mutation{}, ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(data))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return Mutation{}, ErrInvalid
	}
	seen := map[string]bool{}
	var result Mutation
	for d.More() {
		tok, err := d.Token()
		if err != nil {
			return Mutation{}, ErrInvalid
		}
		key, ok := tok.(string)
		if !ok || seen[key] || (key != "if_generation" && (key != "context_mode" || deleteOverride)) {
			return Mutation{}, ErrInvalid
		}
		seen[key] = true
		var raw json.RawMessage
		if d.Decode(&raw) != nil || bytes.Equal(raw, []byte("null")) {
			return Mutation{}, ErrInvalid
		}
		var value string
		if json.Unmarshal(raw, &value) != nil {
			return Mutation{}, ErrInvalid
		}
		switch key {
		case "context_mode":
			result.Mode = Mode(value)
			if !result.Mode.Valid() {
				return Mutation{}, ErrInvalid
			}
		case "if_generation":
			result.Generation, err = ParseGeneration(value)
			if err != nil {
				return Mutation{}, err
			}
		}
	}
	if tok, err = d.Token(); err != nil || tok != json.Delim('}') {
		return Mutation{}, ErrInvalid
	}
	if _, err := d.Token(); err != io.EOF {
		return Mutation{}, ErrInvalid
	}
	if !seen["if_generation"] || (!deleteOverride && !seen["context_mode"]) {
		return Mutation{}, ErrInvalid
	}
	return result, nil
}

// VerifyInclude validates only the new opt-in control. Other identity fields
// retain their legacy JSON behavior, including forward-compatible extensions.
func VerifyInclude(data []byte) (bool, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return false, ErrInvalid
	}
	seen, include := false, false
	for d.More() {
		tok, err := d.Token()
		if err != nil {
			return false, ErrInvalid
		}
		key, ok := tok.(string)
		if !ok {
			return false, ErrInvalid
		}
		var raw json.RawMessage
		if d.Decode(&raw) != nil {
			return false, ErrInvalid
		}
		if strings.EqualFold(key, "include_mcp_policy") {
			if key != "include_mcp_policy" || seen || (string(raw) != "true" && string(raw) != "false") {
				return false, ErrInvalid
			}
			seen, include = true, string(raw) == "true"
		}
	}
	if tok, err = d.Token(); err != nil || tok != json.Delim('}') {
		return false, ErrInvalid
	}
	if _, err := d.Token(); err != io.EOF {
		return false, ErrInvalid
	}
	return include, nil
}
