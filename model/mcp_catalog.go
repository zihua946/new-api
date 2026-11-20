package model

import "time"

type MCPInstallation struct {
	ID                   string             `json:"id" gorm:"primaryKey;size:36"`
	UserID               int                `json:"-" gorm:"uniqueIndex:mcp_installation_owner,priority:1"`
	ServerID             string             `json:"server_id" gorm:"size:36;uniqueIndex:mcp_installation_owner,priority:2"`
	Admin                bool               `json:"-" gorm:"uniqueIndex:mcp_installation_owner,priority:3"`
	AuthorizationAttempt string             `json:"-" gorm:"size:64"`
	Authorized           bool               `json:"authorized"`
	ConnectionRevision   int64              `json:"-"`
	Grant                MCPEncryptedResult `json:"-" gorm:"column:grant_ciphertext"`
	RefreshClaim         string             `json:"-" gorm:"size:36"`
	RefreshUntil         *time.Time         `json:"-"`
	CreatedAt            time.Time          `json:"created_at"`
}

type MCPAuthorizationFlow struct {
	ID             string `gorm:"primaryKey;size:64"`
	InstallationID string `gorm:"size:36;index"`
	UserID         int
	SessionHash    string `gorm:"size:64"`
	Ciphertext     MCPEncryptedResult
	ExpiresAt      time.Time `gorm:"index"`
}

type MCPChatTest struct {
	ID          string `gorm:"primaryKey;size:64"`
	RequestHash string `gorm:"size:64"`
	Result      MCPEncryptedResult
	ExpiresAt   time.Time `gorm:"index"`
}

// One trusted-application grant belongs to a verified platform identity. The
// issuer and client are part of its key so that configuration changes isolate grants.
type MCPEngineGrant struct {
	ID           string             `gorm:"primaryKey;size:64"`
	UserID       int                `gorm:"index"`
	Subject      string             `gorm:"size:256"`
	Token        MCPEncryptedResult `json:"-"`
	RefreshClaim string             `gorm:"size:36"`
	RefreshUntil *time.Time
}
