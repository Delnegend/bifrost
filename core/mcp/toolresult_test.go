package mcp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Text-only results stay one string, as before; media becomes content blocks, never base64 text.
func TestToolResultContent(t *testing.T) {
	text := func(s string) *string { return &s }
	cases := []struct {
		name   string
		result *mcp.CallToolResult
		want   *schemas.ChatMessageContent
	}{
		{"no result", nil, &schemas.ChatMessageContent{ContentStr: text("MCP tool 'echo' executed successfully")}},
		{"text only stays a string", &mcp.CallToolResult{Content: []mcp.Content{mcp.NewTextContent("a"), &mcp.TextContent{Text: "b "}}},
			&schemas.ChatMessageContent{ContentStr: text("ab")}},
		{"structured content when there is no text", &mcp.CallToolResult{StructuredContent: map[string]any{"temp": 21}},
			&schemas.ChatMessageContent{ContentStr: text(`{"temp":21}`)}},
		{"text wins over structured content", &mcp.CallToolResult{Content: []mcp.Content{mcp.NewTextContent("21 degrees")}, StructuredContent: map[string]any{"temp": 21}},
			&schemas.ChatMessageContent{ContentStr: text("21 degrees")}},
		{"image and audio become blocks", &mcp.CallToolResult{Content: []mcp.Content{mcp.NewTextContent("chart"), mcp.NewImageContent("AAAA", "image/png"), mcp.NewAudioContent("BBBB", "audio/mpeg")}},
			&schemas.ChatMessageContent{ContentBlocks: []schemas.ChatContentBlock{
				{Type: schemas.ChatContentBlockTypeText, Text: text("chart")},
				{Type: schemas.ChatContentBlockTypeImage, ImageURLStruct: &schemas.ChatInputImage{URL: "data:image/png;base64,AAAA"}},
				{Type: schemas.ChatContentBlockTypeInputAudio, InputAudio: &schemas.ChatInputAudio{Data: "BBBB", Format: text("mp3")}},
			}}},
		{"embedded resources", &mcp.CallToolResult{Content: []mcp.Content{
			mcp.NewEmbeddedResource(mcp.TextResourceContents{URI: "file:///notes.md", Text: "# Notes"}),
			mcp.NewEmbeddedResource(mcp.BlobResourceContents{URI: "file:///report.pdf", MIMEType: "application/pdf", Blob: "CCCC"}),
		}}, &schemas.ChatMessageContent{ContentBlocks: []schemas.ChatContentBlock{
			{Type: schemas.ChatContentBlockTypeText, Text: text("# Notes")},
			{Type: schemas.ChatContentBlockTypeFile, File: &schemas.ChatInputFile{FileData: text("CCCC"), Filename: text("report.pdf"), FileType: text("application/pdf")}},
		}}},
		{"resource link is a line", &mcp.CallToolResult{Content: []mcp.Content{mcp.NewResourceLink("file:///a.txt", "a", "", "text/plain")}},
			&schemas.ChatMessageContent{ContentStr: text("[Resource: a file:///a.txt (text/plain)]")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, toolResultContent(tc.result, "echo"))
		})
	}
}

// The upstream's own result rides on the message for the gateway, and is withheld once a plugin has
// rewritten the content.
func TestCreateToolResponseMessage_KeepsTheUpstreamResultUntilRewritten(t *testing.T) {
	result := &mcp.CallToolResult{Content: []mcp.Content{mcp.NewTextContent("secret: 42")}, StructuredContent: map[string]any{"secret": 42}, IsError: true}
	message := createToolResponseMessage(testToolCall("call_1"), result, "echo")
	require.NotNil(t, message.ChatToolMessage.IsError)
	assert.Same(t, result, message.ChatToolMessage.MCPResult(message.Content))

	*message.Content.ContentStr = "secret: [redacted]"
	assert.Nil(t, message.ChatToolMessage.MCPResult(message.Content), "a result rewritten in place must not pass the raw one through")
	redacted := "secret: [redacted]"
	message.Content = &schemas.ChatMessageContent{ContentStr: &redacted}
	assert.Nil(t, message.ChatToolMessage.MCPResult(message.Content), "nor one whose content was replaced")
	*message.Content.ContentStr = "secret: 42"
	assert.Same(t, result, message.ChatToolMessage.MCPResult(message.Content))

	// Media results are compared field by field, in-place edits included.
	media := createToolResponseMessage(testToolCall("call_2"), &mcp.CallToolResult{Content: []mcp.Content{mcp.NewTextContent("x"), mcp.NewImageContent("AAAA", "image/png")}}, "echo")
	require.NotNil(t, media.ChatToolMessage.MCPResult(media.Content))
	*media.Content.ContentBlocks[0].Text = "y"
	assert.Nil(t, media.ChatToolMessage.MCPResult(media.Content))
}

// Discovery keeps a tool's title, icons and output schema beside its function definition.
func TestConvertMCPTool_KeepsTitleIconsAndOutputSchema(t *testing.T) {
	tool := mcp.NewTool("weather", mcp.WithOutputSchema[struct {
		Temp int `json:"temp"`
	}]())
	tool.Title = "Weather"
	tool.Icons = []mcp.Icon{{Src: "https://example.com/weather.png", MIMEType: "image/png"}}
	converted := convertMCPToolToBifrostSchema(&tool, &MockLogger{})
	require.NotNil(t, converted.MCP)
	assert.Equal(t, "Weather", converted.MCP.Title)
	assert.Equal(t, tool.Icons, converted.MCP.Icons)
	assert.Contains(t, string(converted.MCP.OutputSchema), `"temp"`)

	plain := mcp.NewTool("echo")
	assert.Nil(t, convertMCPToolToBifrostSchema(&plain, &MockLogger{}).MCP)

	// A type-less output schema is kept, and no "type" is added.
	var untyped mcp.Tool
	require.NoError(t, json.Unmarshal([]byte(`{"name":"answer","inputSchema":{"type":"object"},"outputSchema":{"properties":{"answer":{"type":"string"}}}}`), &untyped))
	converted = convertMCPToolToBifrostSchema(&untyped, &MockLogger{})
	require.NotNil(t, converted.MCP, "the output schema must not be dropped")
	var schema map[string]any
	require.NoError(t, json.Unmarshal(converted.MCP.OutputSchema, &schema))
	assert.Contains(t, schema, "properties")
	assert.NotContains(t, schema, "type")
}

var toolResultSink *schemas.ChatMessage

// BenchmarkCreateToolResponseMessage measures turning an upstream result into a tool message: the
// common text-only case, and one carrying a 1 MB image.
func BenchmarkCreateToolResponseMessage(b *testing.B) {
	for _, tc := range []struct {
		name   string
		result *mcp.CallToolResult
	}{
		{"text", &mcp.CallToolResult{Content: []mcp.Content{mcp.NewTextContent(strings.Repeat("weather is fine ", 64))}}},
		{"image-1MB", &mcp.CallToolResult{Content: []mcp.Content{mcp.NewTextContent("chart"), mcp.NewImageContent(strings.Repeat("A", 1<<20), "image/png")}}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			call := testToolCall("call_1")
			b.ReportAllocs()
			for b.Loop() {
				toolResultSink = createToolResponseMessage(call, tc.result, "echo")
			}
		})
	}
}

// An upstream tool's image and structured content reach the tool message through the real call path,
// and its title, icons and output schema reach the discovered tool, for Chat and Responses alike.
func TestExecuteTool_MultiModalResultAndToolMetadata(t *testing.T) {
	upstream := server.NewMCPServer("chart-upstream", "1.0.0", server.WithToolCapabilities(true))
	chart := mcp.NewTool("chart", mcp.WithOutputSchema[struct {
		Points int `json:"points"`
	}]())
	chart.Title = "Chart"
	chart.Icons = []mcp.Icon{{Src: "https://example.com/chart.png"}}
	upstream.AddTool(chart, func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		result := mcp.NewToolResultStructured(map[string]any{"points": 3}, "3 points")
		result.Content = append(result.Content, mcp.NewImageContent("iVBORw0KGgo=", "image/png"))
		return result, nil
	})
	ts := httptest.NewServer(server.NewStreamableHTTPServer(upstream))
	t.Cleanup(ts.Close)
	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)
	config := &schemas.MCPClientConfig{
		ID: "chart", Name: "plots", ConnectionType: schemas.MCPConnectionTypeHTTP,
		ConnectionString: schemas.NewSecretVar(ts.URL), ToolsToExecute: []string{"*"}, NeedsSessionStickiness: schemas.Ptr(true),
	}
	require.NoError(t, m.AddClient(context.Background(), config))
	t.Cleanup(func() { _ = m.RemoveClient(config.ID) })

	state, ok := snapshotClientState(m, config.ID)
	require.True(t, ok)
	discovered := state.ToolMap["plots-chart"]
	require.NotNil(t, discovered.MCP)
	assert.Equal(t, "Chart", discovered.MCP.Title)
	assert.Contains(t, string(discovered.MCP.OutputSchema), "points")

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	call := testToolCall("call_1")
	call.Function.Name = schemas.Ptr("plots-chart")
	call.Function.Arguments = "{}"
	message, bErr := m.ExecuteChatTool(ctx, &call)
	require.Nil(t, bErr)
	require.Len(t, message.Content.ContentBlocks, 2)
	assert.Equal(t, "3 points", *message.Content.ContentBlocks[0].Text)
	assert.Equal(t, "data:image/png;base64,iVBORw0KGgo=", message.Content.ContentBlocks[1].ImageURLStruct.URL)
	upstreamResult := message.ChatToolMessage.MCPResult(message.Content)
	require.NotNil(t, upstreamResult, "the gateway can pass the upstream's result through")
	assert.NotNil(t, upstreamResult.StructuredContent)

	responses, bErr := m.ExecuteResponsesTool(ctx, &schemas.ResponsesToolMessage{CallID: schemas.Ptr("call_2"), Name: schemas.Ptr("plots-chart"), Arguments: schemas.Ptr("{}")})
	require.Nil(t, bErr)
	require.NotNil(t, responses.ResponsesToolMessage.Output)
	blocks := responses.ResponsesToolMessage.Output.ResponsesFunctionToolCallOutputBlocks
	require.Len(t, blocks, 2, "the Responses function output carries the image too")
	require.NotNil(t, blocks[1].ResponsesInputMessageContentBlockImage)
}
