// Package mcpvouch holds the mark core puts on an MCP tool call it vouches for,
// naming why (schemas.MCPToolCallKind): a provider-injected tool runs past the
// client and request tool filters and the tool permits, and a connect tool is
// allowed by access to its client.
//
// A plugin holds the request context, can lift BlockRestrictedWrites, and can read and
// copy every user value through GetUserValues. So the mark is not a user value: it
// lives in the context's trusted store under a key of an unexported type. Only code
// under core/ can import this package, and code that cannot name the key can neither
// plant the mark, read it, nor copy it to another context.
package mcpvouch

import (
	"github.com/maximhq/bifrost/core/schemas"
)

type contextKey struct{}

type vouch struct {
	kind       schemas.MCPToolCallKind
	clientName string // MCP client that owns the tool
	toolName   string // prefixed tool name, "<client>-<tool>"
}

// Set marks ctx as running toolName of clientName, vouched for as kind.
func Set(ctx *schemas.BifrostContext, kind schemas.MCPToolCallKind, clientName, toolName string) {
	ctx.SetTrustedValue(contextKey{}, vouch{kind: kind, clientName: clientName, toolName: toolName})
}

// Kind returns why ctx's mark vouches for exactly this client's tool, or
// MCPToolCallKindNone. It never vouches for a sibling tool or another client's
// tool of the same name.
func Kind(ctx *schemas.BifrostContext, clientName, toolName string) schemas.MCPToolCallKind {
	v, ok := ctx.TrustedValue(contextKey{}).(vouch)
	if !ok || v.toolName == "" || v.toolName != toolName || v.clientName != clientName {
		return schemas.MCPToolCallKindNone
	}
	return v.kind
}
