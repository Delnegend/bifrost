package schemas

import (
	"fmt"
	"slices"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// FlattenToolResultMedia replaces every block keep rejects with a placeholder naming it, for providers
// that cannot take that media in a tool result. A nil keep keeps text only, joined into one string.
// Unchanged content is returned as is; the caller's is never mutated.
func FlattenToolResultMedia(content *ChatMessageContent, keep func(ChatContentBlock) bool) *ChatMessageContent {
	if content == nil || len(content.ContentBlocks) == 0 {
		return content
	}
	if keep == nil {
		keep = func(block ChatContentBlock) bool { return false }
	}
	kept := func(block ChatContentBlock) bool {
		return block.Type == ChatContentBlockTypeText || block.Type == ChatContentBlockTypeRefusal || keep(block)
	}
	// A cache marker needs blocks to land on, so a marked result is never joined into a string.
	changed, textOnly := false, true
	for _, block := range content.ContentBlocks {
		if !kept(block) {
			changed = true
		} else if block.Type != ChatContentBlockTypeText && block.Type != ChatContentBlockTypeRefusal {
			textOnly = false
		}
		if block.CacheControl != nil {
			textOnly = false
		}
	}
	if !changed {
		return content
	}
	blocks := make([]ChatContentBlock, 0, len(content.ContentBlocks))
	for _, block := range content.ContentBlocks {
		if kept(block) {
			blocks = append(blocks, block)
			continue
		}
		placeholder := ToolResultMediaPlaceholder(block)
		blocks = append(blocks, ChatContentBlock{Type: ChatContentBlockTypeText, Text: &placeholder, CacheControl: block.CacheControl})
	}
	if !textOnly {
		return &ChatMessageContent{ContentBlocks: blocks}
	}
	texts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Text != nil {
			texts = append(texts, *block.Text)
		} else if block.Refusal != nil {
			texts = append(texts, *block.Refusal)
		}
	}
	joined := strings.Join(texts, "\n")
	return &ChatMessageContent{ContentStr: &joined}
}

// SplitBase64DataURL splits "data:<type>;base64,<payload>" without scanning the payload, which
// ParseDataURL's regex does: tool results can carry megabytes of it.
func SplitBase64DataURL(url string) (mediaType, payload string, ok bool) {
	rest, isData := strings.CutPrefix(url, "data:")
	if !isData {
		return "", "", false
	}
	return strings.Cut(rest, ";base64,")
}

// IsImageBlock reports whether block is an image, the one medium most providers take in a tool result.
func IsImageBlock(block ChatContentBlock) bool { return block.Type == ChatContentBlockTypeImage }

// ToolResultForResponses is message with only the media a Responses function_call_output takes
// (images and files); audio becomes a placeholder.
func ToolResultForResponses(message *ChatMessage) *ChatMessage {
	if message == nil {
		return nil
	}
	content := FlattenToolResultMedia(message.Content, func(block ChatContentBlock) bool {
		return block.Type == ChatContentBlockTypeImage || block.Type == ChatContentBlockTypeFile
	})
	if content == message.Content {
		return message
	}
	flattened := *message
	flattened.Content = content
	return &flattened
}

// ToolResultMediaPlaceholder names a media block a tool returned that is not shown
func ToolResultMediaPlaceholder(block ChatContentBlock) string {
	kind, data := string(block.Type), ""
	switch {
	case block.ImageURLStruct != nil:
		kind, data = "image", block.ImageURLStruct.URL
		if mimeType, payload, ok := SplitBase64DataURL(data); ok {
			kind, data = mimeType, payload
		}
	case block.InputAudio != nil:
		kind, data = "audio", block.InputAudio.Data
		if block.InputAudio.Format != nil {
			kind = "audio/" + *block.InputAudio.Format
		}
	case block.File != nil:
		kind = "file"
		if block.File.FileType != nil {
			kind = *block.File.FileType
		}
		if block.File.Filename != nil {
			kind += " " + *block.File.Filename
		}
		if block.File.FileData != nil {
			data = *block.File.FileData
		}
	}
	return MediaPlaceholder(kind, len(data))
}

// ResponsesToolResultMediaPlaceholder names a Responses media block (image, file or audio); false if block is not media.
func ResponsesToolResultMediaPlaceholder(block ResponsesMessageContentBlock) (string, bool) {
	switch {
	case block.ResponsesInputMessageContentBlockImage != nil:
		kind, data := "image", ""
		if url := block.ResponsesInputMessageContentBlockImage.ImageURL; url != nil {
			if mimeType, payload, ok := SplitBase64DataURL(*url); ok {
				kind, data = mimeType, payload
			}
		}
		return MediaPlaceholder(kind, len(data)), true
	case block.ResponsesInputMessageContentBlockFile != nil:
		file, kind, data := block.ResponsesInputMessageContentBlockFile, "file", ""
		if file.FileType != nil {
			kind = *file.FileType
		}
		if file.Filename != nil {
			kind += " " + *file.Filename
		}
		if file.FileData != nil {
			data = *file.FileData
		}
		return MediaPlaceholder(kind, len(data)), true
	case block.Audio != nil:
		return MediaPlaceholder("audio/"+block.Audio.Format, len(block.Audio.Data)), true
	}
	return "", false
}

// FlattenResponsesToolResultMedia replaces each media block with a placeholder and reports whether any
// was replaced. blocks is not mutated.
func FlattenResponsesToolResultMedia(blocks []ResponsesMessageContentBlock) ([]ResponsesMessageContentBlock, bool) {
	var flattened []ResponsesMessageContentBlock
	for i, block := range blocks {
		placeholder, isMedia := ResponsesToolResultMediaPlaceholder(block)
		if !isMedia {
			continue
		}
		if flattened == nil {
			flattened = slices.Clone(blocks)
		}
		flattened[i] = ResponsesMessageContentBlock{Type: ResponsesInputMessageContentBlockTypeText, Text: &placeholder}
	}
	if flattened == nil {
		return blocks, false
	}
	return flattened, true
}

// MediaPlaceholder is ToolResultMediaPlaceholder for media of kind carried as base64Len base64 characters.
func MediaPlaceholder(kind string, base64Len int) string {
	if base64Len == 0 {
		return fmt.Sprintf("[%s returned by the tool, not shown]", kind)
	}
	return fmt.Sprintf("[%s, %s returned by the tool, not shown]", kind, base64Size(base64Len))
}

// base64Size is the decoded size of n base64 characters, in readable units.
func base64Size(n int) string {
	size := n / 4 * 3
	switch {
	case size >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(size)/(1<<20))
	case size >= 1<<10:
		return fmt.Sprintf("%d KB", size>>10)
	}
	return fmt.Sprintf("%d bytes", size)
}

// ResourceContentsForLog returns result's contents with every blob replaced by a placeholder.
func ResourceContentsForLog(result *mcp.ReadResourceResult) []mcp.ResourceContents {
	if result == nil {
		return nil
	}
	contents := make([]mcp.ResourceContents, len(result.Contents))
	for i, content := range result.Contents {
		contents[i] = resourceContentsForLog(content)
	}
	return contents
}

// PromptForLog returns result with image, audio and blob data replaced by placeholders; result is not mutated.
func PromptForLog(result *mcp.GetPromptResult) *mcp.GetPromptResult {
	if result == nil {
		return nil
	}
	logged := *result
	logged.Messages = make([]mcp.PromptMessage, len(result.Messages))
	for i, message := range result.Messages {
		logged.Messages[i] = message
		switch content := message.Content.(type) {
		case mcp.ImageContent:
			content.Data = MediaPlaceholder(content.MIMEType, len(content.Data))
			logged.Messages[i].Content = content
		case mcp.AudioContent:
			content.Data = MediaPlaceholder(content.MIMEType, len(content.Data))
			logged.Messages[i].Content = content
		case mcp.EmbeddedResource:
			content.Resource = resourceContentsForLog(content.Resource)
			logged.Messages[i].Content = content
		}
	}
	return &logged
}

func resourceContentsForLog(content mcp.ResourceContents) mcp.ResourceContents {
	switch blob := content.(type) {
	case mcp.BlobResourceContents:
		blob.Blob = MediaPlaceholder(blob.MIMEType, len(blob.Blob))
		return blob
	case *mcp.BlobResourceContents:
		if blob != nil {
			logged := *blob
			logged.Blob = MediaPlaceholder(blob.MIMEType, len(blob.Blob))
			return logged
		}
	}
	return content
}
