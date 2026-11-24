package model

import (
	"database/sql/driver"
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

var (
	ErrMCPRevisionConflict    = errors.New("MCP configuration revision conflict")
	ErrMCPIdempotencyConflict = errors.New("MCP idempotency key has different arguments")
	ErrMCPServerEnabled       = errors.New("disable the MCP server before archiving")
)

// MCP configuration uses TEXT-backed codecs, including on databases without
// native JSON columns. All host JSON encoding goes through common.
type MCPGroups []string

func (v MCPGroups) Value() (driver.Value, error) { b, err := common.Marshal(v); return string(b), err }
func (v *MCPGroups) Scan(value any) error {
	if value == nil {
		*v = nil
		return nil
	}
	return common.Unmarshal(jsonScanBytes(value), v)
}

type MCPJSON map[string]any

func (v MCPJSON) Value() (driver.Value, error) { b, err := common.Marshal(v); return string(b), err }
func (v *MCPJSON) Scan(value any) error {
	if value == nil {
		*v = nil
		return nil
	}
	return common.Unmarshal(jsonScanBytes(value), v)
}

type MCPConnection struct {
	Command          string `json:"command,omitempty"`
	Package          string `json:"package,omitempty"`
	PackageDigest    string `json:"package_digest,omitempty"`
	Owner            string `json:"owner,omitempty"`
	KBID             string `json:"kb_id,omitempty"`
	Scopes           string `json:"scopes,omitempty"`
	ChannelID        int    `json:"channel_id,omitempty"`
	ConfirmedBaseURL string `json:"confirmed_base_url,omitempty"`
	EngineBaseURL    string `json:"engine_base_url,omitempty"`
	ClientID         string `json:"client_id,omitempty"`
	URL              string `json:"url,omitempty"`
	Transport        string `json:"transport,omitempty"`
}

func (v MCPConnection) Value() (driver.Value, error) {
	b, err := common.Marshal(v)
	return string(b), err
}
func (v *MCPConnection) Scan(value any) error {
	if value == nil {
		*v = MCPConnection{}
		return nil
	}
	return common.Unmarshal(jsonScanBytes(value), v)
}

type MCPLimits struct {
	TimeoutSeconds int `json:"timeout_seconds"`
	MaxConcurrency int `json:"max_concurrency"`
}

func (v MCPLimits) Value() (driver.Value, error) { b, err := common.Marshal(v); return string(b), err }
func (v *MCPLimits) Scan(value any) error {
	if value == nil {
		*v = MCPLimits{}
		return nil
	}
	return common.Unmarshal(jsonScanBytes(value), v)
}

// MCPEncryptedResult accommodates bounded tool output without MySQL TEXT's
// 64 KiB truncation. It is never exposed by model serialization.
type MCPEncryptedResult string

func (MCPEncryptedResult) GormDataType() string { return "string" }
func (MCPEncryptedResult) GormDBDataType(db *gorm.DB, _ *schema.Field) string {
	if db.Dialector.Name() == "mysql" {
		return "longtext"
	}
	return "text"
}

type MCPServer struct {
	Icon                  MCPEncryptedResult `json:"icon,omitempty"`
	Readme                MCPEncryptedResult `json:"readme,omitempty"`
	MetadataRevision      int64              `json:"metadata_revision"`
	RuntimeSecrets        MCPEncryptedResult `json:"-"`
	OAuthCallbackURL      string             `json:"oauth_callback_url,omitempty" gorm:"-"`
	Version               string             `json:"version" gorm:"size:64"`
	Catalog               bool               `json:"-"`
	ConnectionRevision    int64              `json:"-"`
	ID                    string             `json:"id" gorm:"primaryKey;size:36"`
	Slug                  string             `json:"slug" gorm:"size:64;uniqueIndex"`
	Name                  string             `json:"name" gorm:"size:128"`
	Description           string             `json:"description" gorm:"type:text"`
	Kind                  string             `json:"kind" gorm:"size:24;index"`
	Enabled               bool               `json:"enabled"`
	Archived              bool               `json:"-" gorm:"index"`
	AuthMode              string             `json:"auth_mode" gorm:"-"`
	AllowedUserGroups     MCPGroups          `json:"allowed_user_groups" gorm:"type:text"`
	Connection            MCPConnection      `json:"connection" gorm:"type:text"`
	Limits                MCPLimits          `json:"limits" gorm:"type:text"`
	ConnectionHealth      MCPJSON            `json:"connection_health" gorm:"type:text"`
	CredentialCiphertext  string             `json:"-" gorm:"type:text"`
	CredentialRevision    int64              `json:"credential_revision"`
	CredentialsConfigured bool               `json:"credentials_configured" gorm:"-"`
	Revision              int64              `json:"revision"`
	CreatedAt             time.Time          `json:"created_at"`
	UpdatedAt             time.Time          `json:"updated_at"`
}

type MCPTool struct {
	UnitPrice         string    `json:"-" gorm:"size:64"`
	Currency          string    `json:"-" gorm:"size:3"`
	ID                string    `json:"id" gorm:"primaryKey;size:36"`
	ServerID          string    `json:"server_id" gorm:"size:36;uniqueIndex:mcp_tool_name,priority:1;index"`
	Name              string    `json:"name" gorm:"size:128;uniqueIndex:mcp_tool_name,priority:2"`
	Description       string    `json:"description" gorm:"type:text"`
	InputSchema       MCPJSON   `json:"input_schema" gorm:"type:text"`
	OutputSchema      MCPJSON   `json:"output_schema" gorm:"type:text"`
	SchemaHash        string    `json:"schema_hash" gorm:"size:64"`
	Enabled           bool      `json:"enabled"`
	ReviewRequired    bool      `json:"review_required"`
	Removed           bool      `json:"removed"`
	AllowedUserGroups MCPGroups `json:"allowed_user_groups" gorm:"type:text"`
	PriceMode         string    `json:"-" gorm:"size:24"`
	UnitPriceUSD      string    `json:"-" gorm:"size:64"`
	PriceRevision     int64     `json:"-"`
	Revision          int64     `json:"revision"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type MCPDiscovery struct {
	ID             string             `json:"id" gorm:"primaryKey;size:36"`
	ServerID       string             `json:"server_id" gorm:"size:36;index"`
	UserID         int                `json:"-"`
	ServerRevision int64              `json:"server_revision"`
	ToolsJSON      MCPEncryptedResult `json:"-"`
	ExpiresAt      time.Time          `json:"expires_at"`
	CreatedAt      time.Time          `json:"created_at"`
}

type MCPInvocation struct {
	UnitPrice         string `json:"unit_price" gorm:"size:64"`
	Currency          string `json:"currency" gorm:"size:3"`
	ExchangeRate      string `json:"exchange_rate" gorm:"size:64"`
	FundingPreference string `json:"-" gorm:"size:24"`
	FundingSource     string `json:"funding_source" gorm:"size:24"`
	SubscriptionID    int    `json:"subscription_id,omitempty"`
	ReceiptState      string `json:"-" gorm:"size:12;index"`

	ID                  string             `json:"id" gorm:"primaryKey;size:36"`
	ServerID            string             `json:"server_id" gorm:"size:36;index"`
	ToolID              string             `json:"tool_id" gorm:"size:36;index"`
	ToolName            string             `json:"tool_name" gorm:"size:128"`
	UserID              int                `json:"user_id" gorm:"index"`
	PayerUserID         int                `json:"payer_user_id" gorm:"index"`
	BillingUnitID       int                `json:"billing_unit_id"`
	FundingAttempt      int                `json:"-"`
	TokenID             int                `json:"token_id"`
	ChannelID           int                `json:"channel_id"`
	IsTest              bool               `json:"is_test"`
	IdempotencyHash     string             `json:"-" gorm:"size:64;uniqueIndex"`
	ArgumentsHash       string             `json:"-" gorm:"size:64"`
	ArgumentsCiphertext MCPEncryptedResult `json:"-"`
	Status              string             `json:"status" gorm:"size:32;index"`
	BillingStatus       string             `json:"billing_status" gorm:"size:32;index"`
	ErrorCode           string             `json:"error_code,omitempty" gorm:"size:64"`
	UnitPriceUSD        string             `json:"unit_price_usd" gorm:"size:64"`
	QuotaPerUnit        string             `json:"quota_per_unit" gorm:"size:64"`
	PriceRevision       int64              `json:"price_revision"`
	ChargeQuota         int                `json:"charge_quota"`
	ChargedQuota        int                `json:"charged_quota"`
	Billable            bool               `json:"billable"`
	ResultCiphertext    MCPEncryptedResult `json:"-"`
	ResultExpiresAt     *time.Time         `json:"result_expires_at,omitempty"`
	ReconcileAt         *time.Time         `json:"reconcile_at,omitempty" gorm:"index"`
	LeaseUntil          *time.Time         `json:"-" gorm:"index"`
	CreatedAt           time.Time          `json:"created_at" gorm:"index"`
	UpdatedAt           time.Time          `json:"updated_at"`
}

type MCPBillingOperation struct {
	ID           string    `json:"id" gorm:"primaryKey;size:64"`
	InvocationID string    `json:"invocation_id" gorm:"size:36;index"`
	Kind         string    `json:"kind" gorm:"size:16"`
	Amount       int       `json:"amount"`
	PayerUserID  int       `json:"payer_user_id"`
	TokenID      int       `json:"token_id"`
	State        string    `json:"state" gorm:"size:24;index"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type MCPTaskBinding struct {
	ID            string    `json:"-" gorm:"primaryKey;size:64"`
	ServerID      string    `json:"server_id" gorm:"size:36;index"`
	UserID        int       `json:"user_id" gorm:"index"`
	TaskID        string    `json:"task_id" gorm:"size:128;index"`
	SessionID     string    `json:"session_id" gorm:"size:128;index"`
	KnowledgeBase MCPJSON   `json:"knowledge_base" gorm:"type:text"`
	CreatedAt     time.Time `json:"created_at"`
}

type MCPTestAuthorization struct {
	ID                 string    `json:"-" gorm:"primaryKey;size:64"`
	ServerID           string    `json:"server_id" gorm:"size:36;index"`
	UserID             int       `json:"user_id" gorm:"index"`
	SessionHash        string    `json:"-" gorm:"size:64"`
	StateHash          string    `json:"-" gorm:"size:64;uniqueIndex"`
	VerifierCiphertext string    `json:"-" gorm:"type:text"`
	TokenCiphertext    string    `json:"-" gorm:"type:text"`
	Subject            string    `json:"subject" gorm:"size:256"`
	Consumed           bool      `json:"-"`
	ExpiresAt          time.Time `json:"expires_at"`
	CreatedAt          time.Time `json:"created_at"`
}

// Authentication is derived from the protocol configuration, never an administrator-selected mode.
func (server *MCPServer) AfterFind(*gorm.DB) error {
	server.AuthMode = "none"
	if server.Catalog {
		if server.Kind == "public_knowledge" {
			server.AuthMode = "engine_app"
		} else if server.Kind == "deepseek" || server.Connection.ClientID != "" {
			server.AuthMode = "oauth2"
		}
	} else if server.Kind == "deepseek" {
		server.AuthMode = "api_key"
	} else {
		server.AuthMode = "casdoor_oauth"
	}
	return nil
}

func MigrateMCP(db *gorm.DB) error {
	if db.Migrator().HasColumn("mcp_servers", "auth_mode") {
		var err error
		if db.Dialector.Name() == "sqlite" {
			// The embedded SQLite supports DROP COLUMN. The dialect migrator cannot
			// remove historical columns absent from the current GORM schema.
			err = db.Exec("ALTER TABLE mcp_servers DROP COLUMN auth_mode").Error
		} else {
			err = db.Migrator().DropColumn("mcp_servers", "auth_mode")
		}
		if err != nil {
			return err
		}
	}
	if err := db.AutoMigrate(&MCPServer{}, &MCPTool{}, &MCPDiscovery{}, &MCPInvocation{}, &MCPBillingOperation{}, &MCPTaskBinding{}, &MCPTestAuthorization{}, &MCPInstallation{}, &MCPAuthorizationFlow{}, &MCPChatTest{}, &MCPEngineGrant{}, &MCPSettings{}, &MCPTrustedApplication{}); err != nil {
		return err
	}
	// Before multi-currency pricing, both the tool price and its invocation
	// snapshot were denominated in USD. The historical CNY rate is unknown.
	for _, record := range []any{&MCPTool{}, &MCPInvocation{}} {
		if err := db.Model(record).Where("(currency IS NULL OR currency = '') AND unit_price_usd <> ''").
			UpdateColumns(map[string]any{"currency": "USD", "unit_price": gorm.Expr("unit_price_usd")}).Error; err != nil {
			return err
		}
	}
	var servers []MCPServer
	if err := db.Where("enabled = ?", true).Find(&servers).Error; err != nil {
		return err
	}
	for _, server := range servers {
		if server.Connection.Transport == "stdio" {
			if err := db.Model(&MCPServer{}).Where("id = ?", server.ID).Update("enabled", false).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

func GetMCPServer(id string) (*MCPServer, error) {
	var server MCPServer
	err := DB.Where("id = ? AND archived = ?", id, false).First(&server).Error
	server.CredentialsConfigured = server.CredentialCiphertext != "" || server.RuntimeSecrets != "" || server.Kind == "deepseek" && server.Connection.ChannelID > 0
	return &server, err
}

func GetMCPServerBySlug(slug string) (*MCPServer, error) {
	var server MCPServer
	err := DB.Where("slug = ? AND archived = ?", slug, false).First(&server).Error
	// MySQL collations must not turn distinct OAuth resources into aliases.
	if err == nil && server.Slug != slug {
		return nil, gorm.ErrRecordNotFound
	}
	return &server, err
}

func UpdateMCPServer(id string, revision int64, updates map[string]any) error {
	updates["revision"] = gorm.Expr("revision + 1")
	result := DB.Model(&MCPServer{}).Where("id = ? AND revision = ? AND archived = ?", id, revision, false).Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrMCPRevisionConflict
	}
	return nil
}

func ArchiveMCPServer(id string, revision int64) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		var server MCPServer
		if err := lockForUpdate(tx).Where("id = ? AND archived = ?", id, false).First(&server).Error; err != nil {
			return err
		}
		if server.Revision != revision {
			return ErrMCPRevisionConflict
		}
		if server.Enabled {
			return ErrMCPServerEnabled
		}
		result := tx.Model(&MCPServer{}).Where("id = ? AND revision = ? AND enabled = ?", id, revision, false).
			Updates(map[string]any{"archived": true, "revision": revision + 1})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrMCPRevisionConflict
		}
		return nil
	})
}

func UpdateMCPTool(serverID, toolID string, revision int64, updates map[string]any) error {
	updates["revision"] = gorm.Expr("revision + 1")
	result := DB.Model(&MCPTool{}).Where("id = ? AND server_id = ? AND revision = ?", toolID, serverID, revision).Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrMCPRevisionConflict
	}
	return nil
}

func BeginMCPInvocation(call *MCPInvocation) (*MCPInvocation, bool, error) {
	if err := DB.Clauses(clause.OnConflict{DoNothing: true}).Create(call).Error; err != nil {
		return nil, false, err
	}
	var prior MCPInvocation
	if err := DB.Where("idempotency_hash = ?", call.IdempotencyHash).First(&prior).Error; err != nil {
		return nil, false, err
	}
	if prior.ArgumentsHash != call.ArgumentsHash || prior.UserID != call.UserID || prior.ServerID != call.ServerID || prior.ToolID != call.ToolID {
		return nil, false, ErrMCPIdempotencyConflict
	}
	return &prior, prior.ID == call.ID, nil
}
