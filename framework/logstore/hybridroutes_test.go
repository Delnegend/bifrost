package logstore

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestHybridWithRoutes builds a hybrid store whose mirror has one
// in-memory store per route, keyed by route name.
func newTestHybridWithRoutes(t *testing.T, excludeFields []string, routes map[string]ObjectStorageRouteMatch) (*HybridLogStore, LogStore, *objectstore.InMemoryObjectStore, map[string]*objectstore.InMemoryObjectStore) {
	t.Helper()
	inner, err := newSqliteLogStore(context.Background(), &SQLiteConfig{Path: filepath.Join(t.TempDir(), "hybrid.db")}, hybridTestLogger{})
	require.NoError(t, err)
	primary := objectstore.NewInMemoryObjectStore()
	hybrid := newHybridLogStore(inner, primary, "test", hybridTestLogger{}, excludeFields, nil)
	stores := make(map[string]*objectstore.InMemoryObjectStore, len(routes))
	var mirrorRoutes []*mirrorRoute
	for name, match := range routes {
		stores[name] = objectstore.NewInMemoryObjectStore()
		mirrorRoutes = append(mirrorRoutes, testMirrorRoute(name, match, stores[name], ""))
	}
	hybrid.mirror = newObjectMirror(mirrorRoutes, hybridTestLogger{})
	// Tests close the store themselves to drain the mirror; Close is not idempotent.
	t.Cleanup(func() {
		if !hybrid.closed.Load() {
			_ = hybrid.Close(context.Background())
		}
	})
	return hybrid, inner, primary, stores
}

// routeCopy waits for the copy of id in a route's store that satisfies ready
// and returns it decoded.
func routeCopy(t *testing.T, store *objectstore.InMemoryObjectStore, route string, at time.Time, id string, ready func(map[string]string) bool) map[string]string {
	t.Helper()
	key := objectKey(route, "logs", at, id, jsonObjectExt)
	var payload map[string]string
	waitForUploads(t, func() bool {
		data, err := store.Get(context.Background(), key)
		if err != nil {
			return false
		}
		payload = nil
		return sonic.Unmarshal(data, &payload) == nil && ready(payload)
	})
	return payload
}

// chatEntry returns a chat log with one user message.
func chatEntry(id string, at time.Time, status, input string) *Log {
	return &Log{
		ID: id, Timestamp: at, CreatedAt: at, Object: "chat.completion", Provider: "openai", Model: "gpt-4o", Status: status,
		InputHistoryParsed: []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &input}}},
	}
}

// inputBlock returns the error and guardrail metadata of an input-phase block.
func inputBlock(l *Log) *Log {
	l.ErrorDetailsParsed = &schemas.BifrostError{StatusCode: new(400), Error: &schemas.ErrorField{Type: new(guardrailInterventionErrorType), Message: "PII detected"}}
	l.GuardrailDebugParsed = &schemas.BifrostGuardrailMetadata{Interventions: []schemas.BifrostGuardrailIntervention{
		{Phase: "input", RuleName: "pii", GuardrailProvider: "bedrock", Reason: "PII detected"},
	}}
	return l
}

// TestHybridRoutes_MirrorsOnlyMatchingCreates checks that a blocked log is
// copied complete, DB-resident and excluded fields included, while a clean
// log reaches only the primary store.
func TestHybridRoutes_MirrorsOnlyMatchingCreates(t *testing.T) {
	hybrid, inner, primary, stores := newTestHybridWithRoutes(t, []string{"params"}, map[string]ObjectStorageRouteMatch{
		"flagged": {GuardrailOutcomes: []string{GuardrailOutcomeBlocked}},
	})
	ctx := context.Background()
	at := time.Now().UTC()

	blocked := inputBlock(chatEntry("blocked", at, "error", "my ssn is 123-45-6789"))
	blocked.ParamsParsed = map[string]any{"temperature": 0.2}
	require.NoError(t, hybrid.BatchCreateIfNotExists(ctx, []*Log{blocked, chatEntry("clean", at, "success", "hello")}))
	waitForOffload(t, inner, "blocked")
	waitForOffload(t, inner, "clean")

	copied := routeCopy(t, stores["flagged"], "flagged", at, "blocked", func(map[string]string) bool { return true })
	assert.Equal(t, "blocked", copied["id"])
	assert.Equal(t, "chat.completion", copied["object_type"])
	assert.Contains(t, copied["input_history"], "123-45-6789")
	assert.Contains(t, copied["guardrail_debug"], `"rule_name":"pii"`, "DB-resident guardrail_debug is in the copy")
	assert.Contains(t, copied["error_details"], guardrailInterventionErrorType, "DB-resident error_details is in the copy")
	assert.Contains(t, copied["params"], "0.2", "a field excluded from offload is in the copy")

	hybrid.Close(ctx)
	assert.Equal(t, 1, stores["flagged"].Len(), "the clean log is not copied")
	assert.Equal(t, 2, primary.Len(), "primary offload is unchanged")
}

// TestHybridRoutes_LateCompletionMirrorsMergedLog checks that a log which only
// matches once its completion arrives is copied with its input and output.
func TestHybridRoutes_LateCompletionMirrorsMergedLog(t *testing.T) {
	hybrid, inner, _, stores := newTestHybridWithRoutes(t, nil, map[string]ObjectStorageRouteMatch{
		"redacted": {GuardrailOutcomes: []string{GuardrailOutcomeRedacted}, GuardrailPhases: []string{"output"}},
	})
	ctx := context.Background()
	at := time.Now().UTC()

	require.NoError(t, hybrid.CreateIfNotExists(ctx, chatEntry("late", at, "processing", "the question")))
	waitForOffload(t, inner, "late")
	assert.Zero(t, stores["redacted"].Len(), "the partial row does not match yet")

	output := "the [REDACTED] answer"
	require.NoError(t, hybrid.BatchUpdate(ctx, []*Log{{
		ID: "late", Status: "success",
		OutputMessageParsed: &schemas.ChatMessage{Role: schemas.ChatMessageRoleAssistant, Content: &schemas.ChatMessageContent{ContentStr: &output}},
		GuardrailDebugParsed: &schemas.BifrostGuardrailMetadata{Detections: []schemas.BifrostGuardrailDetection{
			{Phase: "output", RuleName: "pii", GuardrailProvider: "presidio", Action: schemas.GuardrailDetectionActionRedact},
		}},
	}}))

	copied := routeCopy(t, stores["redacted"], "redacted", at, "late", func(p map[string]string) bool {
		return strings.Contains(p["output_message"], "REDACTED")
	})
	assert.Contains(t, copied["input_history"], "the question", "the stored input is in the copy")
	assert.Contains(t, copied["guardrail_debug"], "presidio")
	assert.Equal(t, "success", copied["status"])
}

// TestHybridRoutes_HiddenContentIsMirrored checks a content-hidden row: it
// matches on guardrail metadata that only the object holds, is copied with
// its content, and its DB row stays content-free.
func TestHybridRoutes_HiddenContentIsMirrored(t *testing.T) {
	hybrid, inner, _, stores := newTestHybridWithRoutes(t, nil, map[string]ObjectStorageRouteMatch{
		"flagged": {GuardrailOutcomes: []string{GuardrailOutcomeBlocked}, GuardrailPhases: []string{"input"}},
	})
	ctx := context.Background()
	at := time.Now().UTC()

	entry := inputBlock(chatEntry("hidden", at, "error", "sensitive question"))
	entry.ContentHidden = true
	require.NoError(t, hybrid.Create(ctx, entry))
	waitForOffload(t, inner, "hidden")

	copied := routeCopy(t, stores["flagged"], "flagged", at, "hidden", func(map[string]string) bool { return true })
	assert.Contains(t, copied["input_history"], "sensitive question")
	assert.Contains(t, copied["guardrail_debug"], "bedrock")

	row, err := inner.FindByID(ctx, "hidden")
	require.NoError(t, err)
	assert.Empty(t, row.InputHistory)
	assert.Empty(t, row.GuardrailDebug)
}

// TestHybridRoutes_LogMatchingSeveralRoutes checks that every matching route
// receives its own copy.
func TestHybridRoutes_LogMatchingSeveralRoutes(t *testing.T) {
	hybrid, inner, _, stores := newTestHybridWithRoutes(t, nil, map[string]ObjectStorageRouteMatch{
		"flagged":     {GuardrailOutcomes: []string{GuardrailOutcomeBlocked}},
		"bad-request": {StatusCodes: []int{400}},
		"rate-limit":  {StatusCodes: []int{429}},
	})
	ctx := context.Background()
	at := time.Now().UTC()

	require.NoError(t, hybrid.Create(ctx, inputBlock(chatEntry("multi", at, "error", "q"))))
	waitForOffload(t, inner, "multi")
	routeCopy(t, stores["flagged"], "flagged", at, "multi", func(map[string]string) bool { return true })
	routeCopy(t, stores["bad-request"], "bad-request", at, "multi", func(map[string]string) bool { return true })
	hybrid.Close(ctx)
	assert.Zero(t, stores["rate-limit"].Len())
	stats := map[string]ObjectStorageRouteStats{}
	for _, s := range hybrid.ObjectStorageRouteStats() {
		stats[s.Name] = s
	}
	assert.EqualValues(t, 1, stats["flagged"].Written)
	assert.EqualValues(t, 1, stats["bad-request"].Written)
	assert.Zero(t, stats["rate-limit"].Written)
}

// TestNewLogStoreRoutesRequireObjectStorage checks that routes without a
// primary object store are rejected rather than silently ignored.
func TestNewLogStoreRoutesRequireObjectStorage(t *testing.T) {
	cfg := &Config{
		Enabled: true,
		Type:    LogStoreTypeSQLite,
		Config:  &SQLiteConfig{Path: filepath.Join(t.TempDir(), "logs.db")},
		ObjectStorageRoutes: []ObjectStorageRoute{{
			Name:          "flagged",
			Match:         ObjectStorageRouteMatch{GuardrailOutcomes: []string{GuardrailOutcomeBlocked}},
			ObjectStorage: &objectstore.Config{Type: objectstore.StoreTypeS3, Bucket: *schemas.NewSecretVar("b")},
		}},
	}
	_, err := NewLogStore(context.Background(), cfg, hybridTestLogger{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "require object_storage")
}

// TestHybridRoutes_ExcludedRequestTypeIsMirrored checks that a request type
// kept whole in the DB is still copied to a matching route, both when it is
// created matching and when an update makes it match.
func TestHybridRoutes_ExcludedRequestTypeIsMirrored(t *testing.T) {
	hybrid, inner, primary, stores := newTestHybridWithRoutes(t, nil, map[string]ObjectStorageRouteMatch{
		"flagged": {GuardrailOutcomes: []string{GuardrailOutcomeBlocked, GuardrailOutcomeDetected}},
	})
	hybrid.excludedRequestTypes = map[string]struct{}{"chat.completion": {}}
	ctx := context.Background()
	at := time.Now().UTC()

	require.NoError(t, hybrid.Create(ctx, inputBlock(chatEntry("excluded-blocked", at, "error", "blocked question"))))
	copied := routeCopy(t, stores["flagged"], "flagged", at, "excluded-blocked", func(map[string]string) bool { return true })
	assert.Contains(t, copied["input_history"], "blocked question")
	assert.Contains(t, copied["guardrail_debug"], "bedrock")

	require.NoError(t, hybrid.BatchCreateIfNotExists(ctx, []*Log{chatEntry("excluded-late", at, "processing", "late question")}))
	output := "flagged answer"
	require.NoError(t, hybrid.BatchUpdate(ctx, []*Log{{
		ID: "excluded-late", Status: "success",
		OutputMessageParsed: &schemas.ChatMessage{Role: schemas.ChatMessageRoleAssistant, Content: &schemas.ChatMessageContent{ContentStr: &output}},
		GuardrailDebugParsed: &schemas.BifrostGuardrailMetadata{Detections: []schemas.BifrostGuardrailDetection{
			{Phase: "output", RuleName: "toxicity", GuardrailProvider: "azure", Action: schemas.GuardrailDetectionActionDetect},
		}},
	}}))
	copied = routeCopy(t, stores["flagged"], "flagged", at, "excluded-late", func(map[string]string) bool { return true })
	assert.Contains(t, copied["input_history"], "late question", "the stored row's input is in the copy")
	assert.Contains(t, copied["output_message"], output)
	assert.Equal(t, "success", copied["status"])

	row, err := inner.FindByID(ctx, "excluded-late")
	require.NoError(t, err)
	assert.Contains(t, row.OutputMessage, output, "the excluded row stays whole in the DB")
	hybrid.Close(ctx)
	assert.Zero(t, primary.Len(), "excluded rows are still never offloaded")
}
