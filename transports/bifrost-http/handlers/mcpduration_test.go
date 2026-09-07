package handlers

import (
	"encoding/json"
	"testing"
	"time"
)

// decodeSyncInterval runs a raw JSON value through the request field exactly
// as a handler would, so these tests exercise the real decode path rather than
// a restatement of it.
func decodeSyncInterval(t *testing.T, raw string) (*MCPSyncInterval, error) {
	t.Helper()
	var body struct {
		ToolSyncInterval *MCPSyncInterval `json:"tool_sync_interval,omitempty"`
	}
	err := json.Unmarshal([]byte(`{"tool_sync_interval":`+raw+`}`), &body)
	return body.ToolSyncInterval, err
}

func TestSyncIntervalAcceptsDurationStrings(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{`"1s"`, time.Second},
		{`"30s"`, 30 * time.Second},
		{`"5m"`, 5 * time.Minute},
		{`"1h30m"`, 90 * time.Minute},
		{`"90s"`, 90 * time.Second},
		{`"10m"`, 10 * time.Minute},
	} {
		got, err := decodeSyncInterval(t, tc.raw)
		if err != nil {
			t.Errorf("tool_sync_interval=%s should decode, got %v", tc.raw, err)
			continue
		}
		if got.Duration != tc.want {
			t.Errorf("tool_sync_interval=%s: expected %v, got %v", tc.raw, tc.want, got.Duration)
		}
		if err := got.Validate(); err != nil {
			t.Errorf("tool_sync_interval=%s should validate, got %v", tc.raw, err)
		}
	}
}

// TestSyncIntervalBareNumberStillMeansMinutes is the compatibility guarantee.
// Every deployment already sending a plain number means minutes by it, and
// reinterpreting those values would change how often each of them checks its
// MCP servers without anyone asking for it.
func TestSyncIntervalBareNumberStillMeansMinutes(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{`0`, 0},
		{`1`, time.Minute},
		{`10`, 10 * time.Minute},
		{`60`, time.Hour},
		{`1440`, 24 * time.Hour},
	} {
		got, err := decodeSyncInterval(t, tc.raw)
		if err != nil {
			t.Errorf("tool_sync_interval=%s should decode, got %v", tc.raw, err)
			continue
		}
		if got.Duration != tc.want {
			t.Errorf("tool_sync_interval=%s: expected %v, got %v", tc.raw, tc.want, got.Duration)
		}
	}
}

// TestSyncIntervalRejectsFractionalSeconds covers the trap the string form
// introduces, across its whole range rather than just the tail. The column
// stores whole seconds, so "500ms" truncates to 0 and means "follow the global
// setting", the opposite of what a caller asking for a short interval wants,
// while "90500ms" keeps its seconds and silently drops the rest. Both are the
// same loss.
func TestSyncIntervalRejectsFractionalSeconds(t *testing.T) {
	for _, raw := range []string{`"1ms"`, `"500ms"`, `"999ms"`, `"1ns"`, `"1500ms"`, `"90500ms"`} {
		got, err := decodeSyncInterval(t, raw)
		if err != nil {
			continue // rejected at decode is equally acceptable
		}
		if err := got.Validate(); err == nil {
			t.Errorf("tool_sync_interval=%s loses precision on persistence and must be rejected", raw)
		}
	}
}

func TestSyncIntervalRejectsNegatives(t *testing.T) {
	for _, raw := range []string{`-1`, `-10`, `"-5m"`, `"-1s"`} {
		got, err := decodeSyncInterval(t, raw)
		if err != nil {
			continue
		}
		if err := got.Validate(); err == nil {
			t.Errorf("tool_sync_interval=%s is negative and must be rejected", raw)
		}
	}
}

// TestSyncIntervalRejectsNanosecondEchoes keeps the protection that predates
// the string form: a caller echoing back a nanosecond count as a bare number
// must still be refused, since it would otherwise be read as that many minutes
// and park tool sync centuries out.
func TestSyncIntervalRejectsNanosecondEchoes(t *testing.T) {
	for _, override := range []time.Duration{time.Minute, 10 * time.Minute, time.Hour} {
		nanos := int64(override) // what an older GET response carried for this override
		got, err := decodeSyncInterval(t, jsonInt(nanos))
		if err != nil {
			continue
		}
		if err := got.Validate(); err == nil {
			t.Errorf("tool_sync_interval=%d (nanoseconds for a %v override) must be rejected as a bare number", nanos, override)
		}
	}
}

// TestSyncIntervalRejectsFractionalBareNumbers pins that a fractional number is
// an error rather than being truncated, so "5.5" cannot quietly become five
// minutes.
func TestSyncIntervalRejectsFractionalBareNumbers(t *testing.T) {
	for _, raw := range []string{`5.5`, `0.5`, `1e3`} {
		if _, err := decodeSyncInterval(t, raw); err == nil {
			t.Errorf("tool_sync_interval=%s must be rejected; a bare number has to be whole minutes", raw)
		}
	}
}

func TestSyncIntervalRejectsUnparseableStrings(t *testing.T) {
	for _, raw := range []string{`"soon"`, `"5 minutes"`, `"m5"`, `"5x"`} {
		if _, err := decodeSyncInterval(t, raw); err == nil {
			t.Errorf("tool_sync_interval=%s must be rejected as an invalid duration", raw)
		}
	}
}

// TestSyncIntervalSurvivesPersistence asserts an accepted value round-trips
// through the seconds column it is stored in, which is the conversion that
// makes sub-second values meaningless in the first place.
func TestSyncIntervalSurvivesPersistence(t *testing.T) {
	for _, raw := range []string{`"1s"`, `"30s"`, `"5m"`, `10`, `1440`} {
		got, err := decodeSyncInterval(t, raw)
		if err != nil {
			t.Fatalf("tool_sync_interval=%s should decode, got %v", raw, err)
		}
		if err := got.Validate(); err != nil {
			t.Fatalf("tool_sync_interval=%s should validate, got %v", raw, err)
		}
		seconds := int(got.Duration / time.Second)
		if restored := time.Duration(seconds) * time.Second; restored != got.Duration {
			t.Errorf("tool_sync_interval=%s does not round-trip through the seconds column: %v became %v",
				raw, got.Duration, restored)
		}
	}
}

// TestResolveSyncIntervalKeepsExistingWhenAbsent pins the PATCH semantics the
// update endpoint documents: a field the caller did not send must not change.
// It also pins that Resolve is nil-safe, since an absent field is exactly a
// nil pointer and every call site relies on not having to check for it.
func TestResolveSyncIntervalKeepsExistingWhenAbsent(t *testing.T) {
	existing := 7 * time.Minute
	var absent *MCPSyncInterval
	got, err := absent.Resolve(existing)
	if err != nil {
		t.Fatalf("absent field should resolve cleanly, got %v", err)
	}
	if got != existing {
		t.Errorf("expected the existing %v to survive an absent field, got %v", existing, got)
	}

	// Zero is a value, not an absence: it hands the client back to the global
	// setting and must override an existing override.
	sent := &MCPSyncInterval{Duration: 0}
	got, err = sent.Resolve(existing)
	if err != nil {
		t.Fatalf("zero should resolve cleanly, got %v", err)
	}
	if got != 0 {
		t.Errorf("expected an explicit 0 to clear the override, got %v", got)
	}
}

func TestSyncIntervalMarshalsAsDurationString(t *testing.T) {
	out, err := json.Marshal(MCPSyncInterval{Duration: 90 * time.Second})
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if string(out) != `"1m30s"` {
		t.Errorf(`expected "1m30s", got %s`, out)
	}
}

// jsonInt renders an int64 as a JSON number literal.
func jsonInt(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestCreateRequestShadowedFieldReceivesTheValue is the one that matters for
// the create path. MCPClientRequest embeds TableMCPClient, which carries its
// own tool_sync_interval column field, and the request field shadows it. Go
// resolves the shallower field, so the value must land on the request field
// and the embedded column field must stay untouched. Getting that backwards
// would silently drop the caller's interval on every create.
func TestCreateRequestShadowedFieldReceivesTheValue(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{`"30s"`, 30 * time.Second},
		{`"5m"`, 5 * time.Minute},
		{`10`, 10 * time.Minute},
	} {
		var req MCPClientRequest
		body := `{"name":"c","connection_type":"http","tool_sync_interval":` + tc.raw + `}`
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("%s should decode, got %v", body, err)
		}
		if req.ToolSyncInterval == nil {
			t.Fatalf("%s: the request field is nil, so the value was lost", body)
		}
		if req.ToolSyncInterval.Duration != tc.want {
			t.Errorf("%s: expected %v, got %v", body, tc.want, req.ToolSyncInterval.Duration)
		}
		// The embedded column field is written from the resolved duration at
		// the persistence boundary, not by the decoder, so it stays zero here.
		if req.TableMCPClient.ToolSyncInterval != 0 {
			t.Errorf("%s: the embedded column field should stay untouched, got %d",
				body, req.TableMCPClient.ToolSyncInterval)
		}
		// The value the handler resolves is what a create would persist.
		got, err := req.ToolSyncInterval.Resolve(0)
		if err != nil {
			t.Fatalf("%s should resolve, got %v", body, err)
		}
		if got != tc.want {
			t.Errorf("%s: resolved to %v, expected %v", body, got, tc.want)
		}
	}
}

// TestCreateRequestAbsentFieldResolvesToZero pins that omitting the field
// still means "use the global setting", which is what the create path relied
// on when the embedded int defaulted to 0.
func TestCreateRequestAbsentFieldResolvesToZero(t *testing.T) {
	var req MCPClientRequest
	if err := json.Unmarshal([]byte(`{"name":"c","connection_type":"http"}`), &req); err != nil {
		t.Fatalf("should decode, got %v", err)
	}
	if req.ToolSyncInterval != nil {
		t.Fatalf("expected an absent field to stay nil, got %+v", req.ToolSyncInterval)
	}
	got, err := req.ToolSyncInterval.Resolve(0)
	if err != nil || got != 0 {
		t.Errorf("expected an absent field to resolve to 0, got %v (%v)", got, err)
	}
}

// TestUpdateRequestDecodesBothForms covers the update struct, whose field was
// a plain *int before and where a nil pointer still has to mean "unchanged".
func TestUpdateRequestDecodesBothForms(t *testing.T) {
	var withValue MCPClientUpdateRequest
	if err := json.Unmarshal([]byte(`{"tool_sync_interval":"45s"}`), &withValue); err != nil {
		t.Fatalf("should decode, got %v", err)
	}
	got, err := withValue.ToolSyncInterval.Resolve(9 * time.Minute)
	if err != nil {
		t.Fatalf("should resolve, got %v", err)
	}
	if got != 45*time.Second {
		t.Errorf("expected 45s to override the existing value, got %v", got)
	}

	var absent MCPClientUpdateRequest
	if err := json.Unmarshal([]byte(`{"name":"n"}`), &absent); err != nil {
		t.Fatalf("should decode, got %v", err)
	}
	got, err = absent.ToolSyncInterval.Resolve(9 * time.Minute)
	if err != nil {
		t.Fatalf("should resolve, got %v", err)
	}
	if got != 9*time.Minute {
		t.Errorf("expected an absent field to keep the existing 9m, got %v", got)
	}
}
