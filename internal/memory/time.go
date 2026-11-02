package memory

import (
	"encoding/json"
	"time"
)

// Time is a time.Time that renders as RFC 3339 in JSON and templates.
type Time time.Time

// T returns the underlying time.Time.
func (t Time) T() time.Time { return time.Time(t) }

// IsZero reports whether t is the zero time.
func (t Time) IsZero() bool { return time.Time(t).IsZero() }

// MarshalJSON renders RFC 3339.
func (t Time) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(time.Time(t).Format(time.RFC3339))
}

// In formats t in loc for display ("2006-01-02 15:04").
func (t Time) In(loc *time.Location) string {
	if t.IsZero() {
		return ""
	}
	return time.Time(t).In(loc).Format("2006-01-02 15:04")
}
