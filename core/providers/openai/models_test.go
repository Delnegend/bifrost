package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestListModelsByKeyResponseShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		body        string
		wantID      string
		wantOwnedBy string
		wantContext int
	}{
		{
			name:        "OpenAI envelope",
			body:        `{"object":"list","data":[{"id":"gpt-5","owned_by":"openai","context_window":128000}]}`,
			wantID:      "test/gpt-5",
			wantOwnedBy: "openai",
			wantContext: 128000,
		},
		{
			name:        "Together array",
			body:        `[{"id":"zai-org/GLM-5.2","organization":"Z.ai","context_length":131072}]`,
			wantID:      "test/zai-org/GLM-5.2",
			wantOwnedBy: "Z.ai",
			wantContext: 131072,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()

			response, bifrostErr := ListModelsByKey(
				schemas.NewBifrostContext(context.Background(), schemas.NoDeadline),
				&fasthttp.Client{},
				server.URL,
				schemas.Key{Models: schemas.WhiteList{"*"}},
				false,
				nil,
				schemas.ModelProvider("test"),
				false,
				false,
			)

			require.Nil(t, bifrostErr)
			require.Len(t, response.Data, 1)
			require.Equal(t, test.wantID, response.Data[0].ID)
			require.Equal(t, schemas.Ptr(test.wantOwnedBy), response.Data[0].OwnedBy)
			require.Equal(t, schemas.Ptr(test.wantContext), response.Data[0].ContextLength)
		})
	}
}

func TestListModelsMetadataConsumerJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		body        string
		native      string
		integration string
	}{
		{
			name:        "zero-g envelope",
			body:        `{"object":"list","data":[{"id":"codex-model","name":"Codex Model","owned_by":"openai","context_window":400000,"max_output_tokens":128000,"reasoning":true,"supported_reasoning_efforts":["low","medium","high"],"default_reasoning_effort":"medium"}]}`,
			native:      `{"id":"test/codex-model","name":"Codex Model","owned_by":"openai","context_length":400000,"max_output_tokens":128000,"reasoning":{"supported_efforts":["low","medium","high"],"default_effort":"medium"}}`,
			integration: `{"id":"test/codex-model","object":"model","owned_by":"openai","name":"Codex Model","context_window":400000,"max_output_tokens":128000,"supported_reasoning_efforts":["low","medium","high"],"default_reasoning_effort":"medium"}`,
		},
		{
			name:        "compatible array with context_length",
			body:        `[{"id":"model","name":"Display","organization":"vendor","context_length":32000,"max_output_tokens":0,"default_reasoning_effort":"none"}]`,
			native:      `{"id":"test/model","name":"Display","owned_by":"vendor","context_length":32000,"max_output_tokens":0,"reasoning":{"default_effort":"none"}}`,
			integration: `{"id":"test/model","object":"model","owned_by":"vendor","name":"Display","context_window":32000,"max_output_tokens":0,"default_reasoning_effort":"none"}`,
		},
		{
			name:        "ordinary model without optional metadata",
			body:        `{"data":[{"id":"model","owned_by":"openai"}]}`,
			native:      `{"id":"test/model","owned_by":"openai"}`,
			integration: `{"id":"test/model","object":"model","owned_by":"openai"}`,
		},
		{
			name:        "envelope context length and supported efforts",
			body:        `{"data":[{"id":"model","owned_by":"vendor","context_length":16000,"supported_reasoning_efforts":["low"]}]}`,
			native:      `{"id":"test/model","owned_by":"vendor","context_length":16000,"reasoning":{"supported_efforts":["low"]}}`,
			integration: `{"id":"test/model","object":"model","owned_by":"vendor","context_window":16000,"supported_reasoning_efforts":["low"]}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()

			response, bifrostErr := ListModelsByKey(
				schemas.NewBifrostContext(context.Background(), schemas.NoDeadline),
				&fasthttp.Client{}, server.URL, schemas.Key{Models: schemas.WhiteList{"*"}},
				false, nil, schemas.ModelProvider("test"), false, false,
			)
			require.Nil(t, bifrostErr)
			require.Len(t, response.Data, 1)

			native, err := json.Marshal(response.Data[0])
			require.NoError(t, err)
			require.JSONEq(t, test.native, string(native))

			integration := ToOpenAIListModelsResponse(response)
			require.Len(t, integration.Data, 1)
			converted, err := json.Marshal(integration.Data[0])
			require.NoError(t, err)
			require.JSONEq(t, test.integration, string(converted))
		})
	}
}

func TestModelRetrieveResponseShape(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"gpt-6-astra","object":"model","created":1686935002,"owned_by":"openai","shutdown_date":"2026-10-23","name":"Astra","context_window":400000,"max_output_tokens":64000,"supported_reasoning_efforts":["low","high"],"default_reasoning_effort":"high"}`))
	}))
	defer server.Close()

	response, bifrostErr := HandleOpenAIModelRetrieveRequest(
		schemas.NewBifrostContext(context.Background(), schemas.NoDeadline),
		&fasthttp.Client{},
		server.URL+"/v1/models/gpt-6-astra",
		schemas.Key{},
		nil,
		schemas.ModelProvider("test"),
		false,
		false,
	)

	require.Nil(t, bifrostErr)
	require.Equal(t, "test/gpt-6-astra", response.ID)
	require.Equal(t, schemas.Ptr("openai"), response.OwnedBy)
	require.Equal(t, schemas.Ptr(int64(1686935002)), response.Created)
	require.Equal(t, schemas.Ptr("2026-10-23"), response.ShutdownDate)
	require.Equal(t, schemas.Ptr("Astra"), response.Name)
	require.Equal(t, schemas.Ptr(400000), response.ContextLength)
	require.Equal(t, schemas.Ptr(64000), response.MaxOutputTokens)
	require.Equal(t, &schemas.ModelReasoning{SupportedEfforts: []string{"low", "high"}, DefaultEffort: schemas.Ptr("high")}, response.Reasoning)
	converted, err := json.Marshal(ToOpenAIModelRetrieveResponse(response))
	require.NoError(t, err)
	require.JSONEq(t, `{"id":"test/gpt-6-astra","object":"model","created":1686935002,"owned_by":"openai","shutdown_date":"2026-10-23","name":"Astra","context_window":400000,"max_output_tokens":64000,"supported_reasoning_efforts":["low","high"],"default_reasoning_effort":"high"}`, string(converted))
}

func TestModelRetrieveUpstreamErrorIsPropagated(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"The model 'nope' does not exist","type":"invalid_request_error","code":"model_not_found"}}`))
	}))
	defer server.Close()

	response, bifrostErr := HandleOpenAIModelRetrieveRequest(
		schemas.NewBifrostContext(context.Background(), schemas.NoDeadline),
		&fasthttp.Client{},
		server.URL+"/v1/models/nope",
		schemas.Key{},
		nil,
		schemas.ModelProvider("test"),
		false,
		false,
	)

	require.Nil(t, response)
	require.NotNil(t, bifrostErr)
	require.Equal(t, schemas.Ptr(http.StatusNotFound), bifrostErr.StatusCode)
	require.Contains(t, bifrostErr.Error.Message, "does not exist")
}
