package schemas

import (
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func toolMediaContent() *ChatMessageContent {
	text, data := "chart below", strings.Repeat("A", 4096)
	format, pdf, name := "mp3", "application/pdf", "report.pdf"
	return &ChatMessageContent{ContentBlocks: []ChatContentBlock{
		{Type: ChatContentBlockTypeText, Text: &text},
		{Type: ChatContentBlockTypeImage, ImageURLStruct: &ChatInputImage{URL: "data:image/png;base64," + data}},
		{Type: ChatContentBlockTypeInputAudio, InputAudio: &ChatInputAudio{Data: data, Format: &format}},
		{Type: ChatContentBlockTypeFile, File: &ChatInputFile{FileData: &data, FileType: &pdf, Filename: &name}},
	}}
}

// With no media kept, a tool result becomes one string with each medium named, never its base64.
func TestFlattenToolResultMedia_TextOnly(t *testing.T) {
	original := toolMediaContent()
	flattened := FlattenToolResultMedia(original, nil)
	require.NotNil(t, flattened.ContentStr)
	assert.Equal(t, "chart below\n[image/png, 3 KB returned by the tool, not shown]\n[audio/mp3, 3 KB returned by the tool, not shown]\n[application/pdf report.pdf, 3 KB returned by the tool, not shown]", *flattened.ContentStr)
	assert.Len(t, original.ContentBlocks, 4, "the caller's content is not mutated")
}

// Kept media stays a block; the rest becomes placeholders beside it.
func TestFlattenToolResultMedia_KeepsImages(t *testing.T) {
	flattened := FlattenToolResultMedia(toolMediaContent(), IsImageBlock)
	require.Len(t, flattened.ContentBlocks, 4)
	assert.Equal(t, ChatContentBlockTypeImage, flattened.ContentBlocks[1].Type)
	assert.Equal(t, ChatContentBlockTypeText, flattened.ContentBlocks[2].Type)
	assert.Contains(t, *flattened.ContentBlocks[2].Text, "audio/mp3")
}

// A cache marker survives flattening: on its text block, or on the placeholder of the media it was on,
// and a marked result stays blocks so providers can place the marker.
func TestFlattenToolResultMedia_KeepsCacheMarkers(t *testing.T) {
	marker := &CacheControl{Type: CacheControlTypeEphemeral}
	onText := toolMediaContent()
	onText.ContentBlocks[0].CacheControl = marker
	flattened := FlattenToolResultMedia(onText, nil)
	require.Len(t, flattened.ContentBlocks, 4, "a marked result is not joined into a string")
	assert.Same(t, marker, flattened.ContentBlocks[0].CacheControl)

	onMedia := toolMediaContent()
	onMedia.ContentBlocks[3].CacheControl = marker
	flattened = FlattenToolResultMedia(onMedia, IsImageBlock)
	require.Len(t, flattened.ContentBlocks, 4)
	assert.Equal(t, ChatContentBlockTypeText, flattened.ContentBlocks[3].Type)
	assert.Same(t, marker, flattened.ContentBlocks[3].CacheControl, "the placeholder carries the replaced block's marker")
}

// Content that needs nothing removed is returned as is, so text results cost nothing.
func TestFlattenToolResultMedia_Unchanged(t *testing.T) {
	text := "plain"
	str := &ChatMessageContent{ContentStr: &text}
	assert.Same(t, str, FlattenToolResultMedia(str, nil))
	assert.Nil(t, FlattenToolResultMedia(nil, nil))
	onlyImages := &ChatMessageContent{ContentBlocks: toolMediaContent().ContentBlocks[:2]}
	assert.Same(t, onlyImages, FlattenToolResultMedia(onlyImages, IsImageBlock))
}

// The Responses wire takes images and files in a function output, not audio.
func TestToolResultForResponses(t *testing.T) {
	message := &ChatMessage{Role: ChatMessageRoleTool, Content: toolMediaContent()}
	adapted := ToolResultForResponses(message)
	require.Len(t, adapted.Content.ContentBlocks, 4)
	assert.Equal(t, ChatContentBlockTypeImage, adapted.Content.ContentBlocks[1].Type)
	assert.Equal(t, ChatContentBlockTypeText, adapted.Content.ContentBlocks[2].Type)
	assert.Equal(t, ChatContentBlockTypeFile, adapted.Content.ContentBlocks[3].Type)
	assert.Equal(t, ChatContentBlockTypeInputAudio, message.Content.ContentBlocks[2].Type, "the original message is untouched")
	assert.Nil(t, ToolResultForResponses(nil))
}

// Logged prompt and resource reads name their media instead of carrying its base64, without
// touching the result served to the caller.
func TestPromptAndResourceForLog(t *testing.T) {
	data := strings.Repeat("A", 4096)
	resource := &mcp.ReadResourceResult{Contents: []mcp.ResourceContents{
		mcp.TextResourceContents{URI: "file:///a.md", Text: "# A"},
		mcp.BlobResourceContents{URI: "file:///a.png", MIMEType: "image/png", Blob: data},
	}}
	logged := ResourceContentsForLog(resource)
	require.Len(t, logged, 2)
	assert.Equal(t, "# A", logged[0].(mcp.TextResourceContents).Text)
	assert.Equal(t, "[image/png, 3 KB returned by the tool, not shown]", logged[1].(mcp.BlobResourceContents).Blob)
	assert.Equal(t, data, resource.Contents[1].(mcp.BlobResourceContents).Blob, "the served result is untouched")

	prompt := &mcp.GetPromptResult{Messages: []mcp.PromptMessage{
		mcp.NewPromptMessage(mcp.RoleUser, mcp.NewTextContent("look")),
		mcp.NewPromptMessage(mcp.RoleUser, mcp.NewImageContent(data, "image/png")),
	}}
	loggedPrompt := PromptForLog(prompt)
	assert.Equal(t, "look", loggedPrompt.Messages[0].Content.(mcp.TextContent).Text)
	assert.Equal(t, "[image/png, 3 KB returned by the tool, not shown]", loggedPrompt.Messages[1].Content.(mcp.ImageContent).Data)
	assert.Equal(t, data, prompt.Messages[1].Content.(mcp.ImageContent).Data, "the served result is untouched")
	assert.Nil(t, PromptForLog(nil))
	assert.Nil(t, ResourceContentsForLog(nil))
}
