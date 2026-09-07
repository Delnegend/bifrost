package handlers

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// MCPSyncInterval is the wire form of an MCP client's tool_sync_interval on
// create and update requests.
//
// It accepts two forms. A duration string ("30s", "5m", "1h30m") is parsed as
// written. A bare integer keeps the meaning it has always had on this API,
// a number of minutes, so an existing caller sending 10 still gets ten
// minutes rather than ten of something else. Redefining the integer was the
// alternative and is not worth it: every deployment already storing a plain
// number would silently start checking sixty times more often.
//
// The string form is not a new convention on this API either. GET responses
// already render both per-client duration fields as strings, so accepting one
// here makes the endpoint symmetric with the shape it hands back.
type MCPSyncInterval struct {
	// Duration is the resolved value.
	Duration time.Duration
	// FromString records which form the caller used, so a bounds error can be
	// phrased in the unit they actually wrote rather than one they did not.
	FromString bool
}

// UnmarshalJSON reads either form. Validation of range lives in Validate
// rather than here so a handler reports it as a request error with the field
// named, instead of a decoder error that says only that the body was bad.
func (m *MCPSyncInterval) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "null" || trimmed == "" {
		return nil
	}

	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return fmt.Errorf("tool_sync_interval must be a duration string or a number of minutes: %w", err)
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid tool_sync_interval %q: expected a duration such as \"30s\" or \"5m\"", s)
		}
		m.Duration = d
		m.FromString = true
		return nil
	}

	// The bare-number form. Rejecting a fractional value rather than
	// truncating it keeps "5.5" from silently becoming five minutes.
	if strings.ContainsAny(trimmed, ".eE") {
		return fmt.Errorf("invalid tool_sync_interval %s: a bare number must be a whole number of minutes, or use a duration string such as \"5m30s\"", trimmed)
	}
	var minutes int64
	if err := json.Unmarshal(data, &minutes); err != nil {
		return fmt.Errorf("tool_sync_interval must be a duration string or a number of minutes: %w", err)
	}
	// Bounds are checked before the multiply, since overflowing it here would
	// produce a plausible-looking negative duration.
	if minutes < 0 {
		m.Duration = time.Duration(minutes)
		return nil
	}
	if minutes > maxToolSyncIntervalMinutes {
		return fmt.Errorf("tool_sync_interval must be at most %d minutes", maxToolSyncIntervalMinutes)
	}
	m.Duration = time.Duration(minutes) * time.Minute
	return nil
}

// MarshalJSON renders the resolved value as a duration string, matching how
// GET responses already render this field.
func (m MCPSyncInterval) MarshalJSON() ([]byte, error) {
	return json.Marshal(m.Duration.String())
}

// Validate reports why a resolved interval cannot be used.
//
// The column stores whole seconds, and 0 there means "use the global setting"
// rather than "disabled". A value carrying a sub-second remainder loses it on
// the way to storage, and one below a second truncates to 0 and selects the
// opposite of what was asked for.
//
// The storage layer refuses both already. This repeats the check at the
// request boundary so the answer is a 400 before anything happens, rather than
// a failure after the client has been dialled; the wording is kept identical
// to the storage layer's so a caller never sees the same rejection phrased two
// ways. The ceiling on the bare-integer form is checked during decode instead,
// since that is a property of the wire form rather than of the duration.
func (m MCPSyncInterval) Validate() error {
	if m.Duration < 0 {
		return fmt.Errorf("tool_sync_interval must be 0 (use the global setting) or a positive duration, got %q", m.Duration.String())
	}
	if m.Duration%time.Second != 0 {
		return fmt.Errorf("tool_sync_interval must be a whole number of seconds, got %q", m.Duration.String())
	}
	return nil
}

// Resolve applies the field to an existing value and returns what to persist.
//
// The receiver is deliberately nil-safe, and that is the common case rather
// than an edge one: these endpoints have PATCH semantics, so an absent field
// is a nil pointer meaning "keep what is already there". Callers can therefore
// write req.ToolSyncInterval.Resolve(existing) without a nil check, and the
// distinction between absent and explicitly zero stays in one place. An
// explicit 0 is a value, not an absence, and clears the override.
func (m *MCPSyncInterval) Resolve(existing time.Duration) (time.Duration, error) {
	if m == nil {
		return existing, nil
	}
	if err := m.Validate(); err != nil {
		return 0, err
	}
	return m.Duration, nil
}
