package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Provider is a local inference engine.
type Provider interface {
	// Chat runs one completion. It must honour ctx cancellation.
	Chat(ctx context.Context, req Request) (Response, error)
	// Health returns nil when the engine can serve requests.
	Health(ctx context.Context) error
}

// DecodeJSON parses a schema-constrained response into v. Grammar-constrained
// output is normally valid, but a response cut off by max_tokens is not, and a
// model may wrap JSON in prose or code fences when no grammar was applied;
// both are handled here so callers get a single ErrMalformed signal.
func DecodeJSON(resp Response, v any) error {
	if resp.FinishReason == "length" {
		return fmt.Errorf("%w: output truncated at max_tokens", ErrMalformed)
	}
	s := strings.TrimSpace(resp.Content)
	if s == "" {
		return fmt.Errorf("%w: empty output", ErrMalformed)
	}
	if err := json.Unmarshal([]byte(s), v); err == nil {
		return nil
	}
	// Fallback: first balanced {...} object in the text.
	if obj, ok := firstJSONObject(s); ok {
		if err := json.Unmarshal([]byte(obj), v); err == nil {
			return nil
		}
	}
	return fmt.Errorf("%w: not valid JSON: %.120q", ErrMalformed, s)
}

// firstJSONObject extracts the first balanced JSON object, respecting strings.
func firstJSONObject(s string) (string, bool) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "", false
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case inStr && c == '\\':
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}
