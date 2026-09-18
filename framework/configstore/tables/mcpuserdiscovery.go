package tables

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/bytedance/sonic"
	"github.com/maximhq/bifrost/core/schemas"
	"gorm.io/gorm"
)

// TableMCPPerUserDiscovery is the discovery one caller sees on one MCP client
// with per_user_discovery on: the tools and instructions the upstream answered
// with when Bifrost connected with that caller's own credential. It is the
// persisted form of schemas.MCPDiscovery for one caller.
//
// Keyed by (auth_mode, identity, mcp_client_id) with the same identity
// columns as TableMCPOauthToken and TableMCPPerUserHeaderCredential, so a
// discovery and the credential it was made with share one binding: whatever
// revokes, orphans, or reassigns the credential can find its discovery by the
// same key. There is deliberately no auth_mode='admin' row shape — a
// per-user-discovery client stores no admin credential and does no admin
// discovery. Rows are not encrypted; tool schemas and instructions are not
// secret material.
type TableMCPPerUserDiscovery struct {
	ID           string  `gorm:"type:varchar(255);primaryKey" json:"id"`                // UUID
	SessionID    string  `gorm:"type:varchar(255);index" json:"session_id,omitempty"`   // Session-mode identity: client-asserted x-bf-mcp-session-id. Empty for vk/user mode rows.
	VirtualKeyID *string `gorm:"type:varchar(255);index" json:"virtual_key_id"`         // VK identity (vk-mode rows)
	UserID       *string `gorm:"type:varchar(255);index" json:"user_id"`                // User identity (user-mode rows)
	MCPClientID  string  `gorm:"type:varchar(255);not null;index" json:"mcp_client_id"` // Which MCP server
	AuthMode     string  `gorm:"type:varchar(20);not null" json:"auth_mode"`            // 'user' | 'vk' | 'session' — which identity column keys this row

	ToolsJSON           string    `gorm:"type:text;not null" json:"-"`                     // JSON map[string]schemas.ChatTool keyed by the upstream name, no client prefix (added from the current name on read)
	ToolNameMappingJSON string    `gorm:"type:text" json:"-"`                              // JSON map[string]string, sanitized name → upstream name
	Instructions        string    `gorm:"type:text" json:"-"`                              // Upstream initialize `instructions` from the same handshake, "" when it advertises none
	DiscoveryHash       string    `gorm:"type:varchar(64);not null" json:"discovery_hash"` // Content hash of the tools, name mapping and instructions, for change detection and grouping
	DiscoveredAt        time.Time `gorm:"not null" json:"discovered_at"`                   // When the upstream last answered tools/list for this caller
	CreatedAt           time.Time `gorm:"index;not null" json:"created_at"`
	UpdatedAt           time.Time `gorm:"index;not null" json:"updated_at"`

	// Decoded forms of the JSON columns, populated by AfterFind and read by
	// BeforeSave. Not columns.
	Tools           map[string]schemas.ChatTool `gorm:"-" json:"-"`
	ToolNameMapping map[string]string           `gorm:"-" json:"-"`
}

func (TableMCPPerUserDiscovery) TableName() string {
	return "mcp_per_user_discoveries"
}

// BeforeSave serializes the decoded maps into their JSON columns. A nil
// Tools map is written as an empty object rather than left empty, since the
// column is NOT NULL and a caller whose upstream grants it no tool has a
// valid, meaningful discovery.
func (s *TableMCPPerUserDiscovery) BeforeSave(tx *gorm.DB) error {
	tools := s.Tools
	if tools == nil {
		tools = map[string]schemas.ChatTool{}
	}
	data, err := json.Marshal(tools)
	if err != nil {
		return fmt.Errorf("marshal per-user discovery tools: %w", err)
	}
	s.ToolsJSON = string(data)

	mapping := s.ToolNameMapping
	if mapping == nil {
		mapping = map[string]string{}
	}
	data, err = json.Marshal(mapping)
	if err != nil {
		return fmt.Errorf("marshal per-user discovery name mapping: %w", err)
	}
	s.ToolNameMappingJSON = string(data)
	return nil
}

// AfterFind decodes the JSON columns back into the maps.
func (s *TableMCPPerUserDiscovery) AfterFind(tx *gorm.DB) error {
	if s.ToolsJSON != "" {
		if err := sonic.Unmarshal([]byte(s.ToolsJSON), &s.Tools); err != nil {
			return fmt.Errorf("unmarshal per-user discovery tools: %w", err)
		}
	}
	if s.ToolNameMappingJSON != "" {
		if err := sonic.Unmarshal([]byte(s.ToolNameMappingJSON), &s.ToolNameMapping); err != nil {
			return fmt.Errorf("unmarshal per-user discovery name mapping: %w", err)
		}
	}
	return nil
}

// Identity returns the value of the identity column its AuthMode selects, or
// "" when that column is empty. Mirrors how the credential tables read their
// own identity back.
func (s *TableMCPPerUserDiscovery) Identity() string {
	switch schemas.MCPAuthMode(s.AuthMode) {
	case schemas.MCPAuthModeUser:
		if s.UserID != nil {
			return *s.UserID
		}
	case schemas.MCPAuthModeVK:
		if s.VirtualKeyID != nil {
			return *s.VirtualKeyID
		}
	case schemas.MCPAuthModeSession:
		return s.SessionID
	}
	return ""
}
