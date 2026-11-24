package mcp

import (
	"context"
	"errors"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/coreos/go-oidc/v3/oidc"
	"gorm.io/gorm"
)

var SettingsFields = []string{"public_base_url", "dashboard_base_url", "casdoor_provider_id", "casdoor_organization", "oauth_client_id", "oauth_allowed_client_ids", "knowledge_engine_url", "knowledge_client_id", "knowledge_scopes"}
var SettingsSecrets = []string{"oauth_client_secret", "knowledge_client_secret", "knowledge_oauth_client_secret"}

type SettingsView struct {
	Revision  int64             `json:"revision"`
	Values    map[string]string `json:"values"`
	Secrets   map[string]bool   `json:"secrets"`
	Effective map[string]string `json:"effective"`
}

func loadSettings() (map[string]string, int64, error) {
	if model.DB == nil {
		return nil, 0, Failure(503, "MCP_NOT_CONFIGURED", "Database is unavailable")
	}
	var row model.MCPSettings
	err := model.DB.First(&row, 1).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return map[string]string{}, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	raw, err := Open(string(row.Payload), "mcp-settings")
	if err != nil {
		return nil, 0, err
	}
	values := map[string]string{}
	err = common.UnmarshalJsonStr(raw, &values)
	return values, row.Revision, err
}

// Business configuration has one authority: the database. Old environment
// variables deliberately have no fallback here.
type settingsContextKey struct{}
type settingsSnapshot struct {
	values map[string]string
	err    error
}

func WithSettingsSnapshot(ctx context.Context) context.Context {
	values, _, err := loadSettings()
	return context.WithValue(ctx, settingsContextKey{}, settingsSnapshot{values, err})
}
func ConfigValue(key string, contexts ...context.Context) string {
	var values map[string]string
	var err error
	if len(contexts) > 0 {
		if snapshot, ok := contexts[0].Value(settingsContextKey{}).(settingsSnapshot); ok {
			values, err = snapshot.values, snapshot.err
		}
	}
	if values == nil && err == nil {
		values, _, err = loadSettings()
	}
	if err != nil {
		return ""
	}
	if strings.HasPrefix(key, "casdoor_") {
		id, _ := strconv.Atoi(values["casdoor_provider_id"])
		var provider model.CustomOAuthProvider
		if id <= 0 || model.DB.First(&provider, id).Error != nil || !provider.Enabled || provider.UserIdField != "sub" || provider.WellKnown != values["casdoor_issuer"]+"/.well-known/openid-configuration" {
			return ""
		}
	}
	return effectiveSetting(values, key)
}

func effectiveSetting(values map[string]string, key string) string {
	value := values[key]
	if value != "" {
		return value
	}
	switch key {
	case "public_base_url":
		return strings.TrimRight(system_setting.ServerAddress, "/")
	case "dashboard_base_url":
		return effectiveSetting(values, "public_base_url")
	case "knowledge_scopes":
		return "openid system.login"
	case "oauth_allowed_client_ids":
		return values["oauth_client_id"]
	}
	return ""
}

func ReadSettings() (*SettingsView, error) {
	values, revision, err := loadSettings()
	if err != nil {
		return nil, err
	}
	view := &SettingsView{Revision: revision, Values: map[string]string{}, Secrets: map[string]bool{}, Effective: map[string]string{}}
	for _, key := range SettingsFields {
		view.Values[key] = values[key]
		view.Effective[key] = effectiveSetting(values, key)
	}
	view.Effective["casdoor_issuer"] = values["casdoor_issuer"]
	for _, key := range SettingsSecrets {
		view.Secrets[key] = values[key] != ""
	}
	return view, nil
}

func SaveSettings(ctx context.Context, revision int64, patch map[string]string) (*SettingsView, error) {
	values, current, err := loadSettings()
	if err != nil {
		return nil, err
	}
	if current != revision {
		return nil, model.ErrMCPRevisionConflict
	}
	previous := maps.Clone(values)
	for key, value := range patch {
		if !slices.Contains(SettingsFields, key) && !slices.Contains(SettingsSecrets, key) {
			return nil, Failure(422, "INVALID_CONFIG", "Unknown MCP setting")
		}
		if len(value) > 4096 {
			return nil, Failure(422, "INVALID_CONFIG", "MCP setting is too long")
		}
		if !slices.Contains(SettingsSecrets, key) {
			value = strings.TrimSpace(value)
		}
		if key == "public_base_url" || key == "dashboard_base_url" || key == "knowledge_engine_url" {
			value = strings.TrimRight(value, "/")
		}
		values[key] = value
	}
	for _, key := range []string{"public_base_url", "dashboard_base_url"} {
		value := effectiveSetting(values, key)
		u, e := url.Parse(value)
		if e != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || u.Scheme != "https" && u.Scheme != "http" {
			return nil, Failure(422, "INVALID_CONFIG", "A valid HTTP(S) origin is required")
		}
	}
	if base := values["knowledge_engine_url"]; base != "" {
		if _, err := ValidateEndpoint(base); err != nil {
			return nil, err
		}
	}
	if !slices.Contains(strings.Fields(effectiveSetting(values, "knowledge_scopes")), "openid") {
		return nil, Failure(422, "INVALID_CONFIG", "Knowledge scopes must include openid")
	}
	if raw := values["casdoor_provider_id"]; raw != "" {
		id, e := strconv.Atoi(raw)
		var provider model.CustomOAuthProvider
		if e != nil || id <= 0 || model.DB.First(&provider, id).Error != nil || !provider.Enabled || provider.UserIdField != "sub" {
			return nil, Failure(422, "INVALID_CONFIG", "Select an enabled OAuth provider using sub")
		}
		issuer, ok := strings.CutSuffix(provider.WellKnown, "/.well-known/openid-configuration")
		if !ok {
			return nil, Failure(422, "INVALID_CONFIG", "The provider requires OIDC discovery")
		}
		if old := values["casdoor_issuer"]; old != "" && old != issuer {
			return nil, Failure(422, "IDENTITY_PROVIDER_CHANGED", "Use a new provider with new account bindings when changing issuer")
		}
		client, e := NewHTTPClient(issuer, 10*time.Second)
		if e != nil {
			return nil, e
		}
		defer client.CloseIdleConnections()
		if _, e = oidc.NewProvider(oidc.ClientContext(ctx, client), issuer); e != nil {
			return nil, Failure(422, "INVALID_CONFIG", "OIDC issuer discovery failed")
		}
		values["casdoor_issuer"] = issuer
	} else {
		values["casdoor_issuer"] = ""
	}
	raw, err := common.Marshal(values)
	if err != nil {
		return nil, err
	}
	sealed, err := Seal(string(raw), "mcp-settings")
	if err != nil {
		return nil, err
	}
	err = model.DB.Transaction(func(tx *gorm.DB) error {
		if revision == 0 {
			err = tx.Create(&model.MCPSettings{ID: 1, Revision: 1, Payload: model.MCPEncryptedResult(sealed)}).Error
		} else {
			result := tx.Model(&model.MCPSettings{}).Where("id = ? AND revision = ?", 1, revision).Updates(map[string]any{"revision": revision + 1, "payload": sealed})
			err = result.Error
			if err == nil && result.RowsAffected != 1 {
				err = model.ErrMCPRevisionConflict
			}
		}
		if err != nil {
			return err
		}
		identityChanged := effectiveSetting(previous, "public_base_url") != effectiveSetting(values, "public_base_url") || previous["casdoor_issuer"] != values["casdoor_issuer"] || previous["casdoor_provider_id"] != values["casdoor_provider_id"] || previous["casdoor_organization"] != values["casdoor_organization"]
		if identityChanged {
			for _, row := range []any{&model.MCPAuthorizationFlow{}, &model.MCPTestAuthorization{}} {
				if err := tx.Where("1 = 1").Delete(row).Error; err != nil {
					return err
				}
			}
			if err := tx.Model(&model.MCPInstallation{}).Where("1 = 1").Updates(map[string]any{"authorized": false, "grant_ciphertext": ""}).Error; err != nil {
				return err
			}
		}
		if identityChanged || previous["knowledge_client_id"] != values["knowledge_client_id"] || previous["knowledge_oauth_client_secret"] != values["knowledge_oauth_client_secret"] {
			if err := tx.Where("1 = 1").Delete(&model.MCPEngineGrant{}).Error; err != nil {
				return err
			}
		}
		if effectiveSetting(previous, "public_base_url") != effectiveSetting(values, "public_base_url") {
			var servers []model.MCPServer
			if err := tx.Where("catalog = ? AND kind IN ?", true, []string{"deepseek", "public_knowledge"}).Find(&servers).Error; err != nil {
				return err
			}
			oldBase, newBase := effectiveSetting(previous, "public_base_url"), effectiveSetting(values, "public_base_url")
			for _, server := range servers {
				if path, ok := strings.CutPrefix(server.Connection.URL, oldBase+"/mcp/providers/"); ok {
					server.Connection.URL = newBase + "/mcp/providers/" + path
				}
				updates := map[string]any{"connection": server.Connection, "connection_revision": gorm.Expr("connection_revision + 1"), "revision": gorm.Expr("revision + 1")}
				if server.Kind == "deepseek" {
					server.Connection.ClientID = ""
					updates["connection"] = server.Connection
					updates["credential_ciphertext"] = ""
				}
				if err := tx.Model(&model.MCPServer{}).Where("id = ?", server.ID).Updates(updates).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})

	if err != nil {
		return nil, err
	}
	return ReadSettings()
}
