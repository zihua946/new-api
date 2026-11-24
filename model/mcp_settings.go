package model

// MCPSettings keeps the deployment's business configuration encrypted at rest.
// The deployment encryption key never resides in this table.
type MCPSettings struct {
	ID       int `gorm:"primaryKey"`
	Revision int64
	Payload  MCPEncryptedResult `json:"-"`
}

type MCPTrustedApplication struct {
	ID       string             `json:"id" gorm:"primaryKey;size:256"`
	Revision int64              `json:"revision"`
	Payload  MCPEncryptedResult `json:"-"`
}
