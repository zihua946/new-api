package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image/png"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/google/uuid"
	"golang.org/x/net/http/httpguts"
	"gorm.io/gorm"
)

var iconKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,79}$`)
var variableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
var slugPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{2,63}$`)

type ServerInput struct {
	Icon              string              `json:"icon"`
	Readme            string              `json:"readme"`
	Headers           map[string]string   `json:"headers"`
	Env               map[string]string   `json:"env"`
	Version           string              `json:"version"`
	ClientSecret      string              `json:"client_secret"`
	Slug              string              `json:"slug"`
	Name              string              `json:"name"`
	Description       string              `json:"description"`
	Kind              string              `json:"kind"`
	AuthMode          string              `json:"auth_mode"`
	AllowedUserGroups model.MCPGroups     `json:"allowed_user_groups"`
	Connection        model.MCPConnection `json:"connection"`
	Limits            model.MCPLimits     `json:"limits"`
}

type ServerPatch struct {
	Icon              *string              `json:"icon"`
	Readme            *string              `json:"readme"`
	Headers           map[string]string    `json:"headers"`
	Env               map[string]string    `json:"env"`
	Version           *string              `json:"version"`
	AuthMode          *string              `json:"auth_mode"`
	ClientSecret      *string              `json:"client_secret"`
	ExpectedRevision  int64                `json:"expected_revision"`
	Name              *string              `json:"name"`
	Description       *string              `json:"description"`
	AllowedUserGroups *model.MCPGroups     `json:"allowed_user_groups"`
	Connection        *model.MCPConnection `json:"connection"`
	Limits            *model.MCPLimits     `json:"limits"`
}

func CreateServer(input ServerInput) (*model.MCPServer, error) {
	input.Connection.URL = strings.TrimSpace(input.Connection.URL)
	catalog := input.Kind == ""
	if catalog {
		input.Slug = "mcp-" + uuid.NewString()
		input.Kind = "remote_mcp"
		if kind := LocalProvider(input.Connection.URL); kind != "" {
			input.Slug, input.Kind = LocalProviderSlug(input.Connection.URL), kind

		}
	}
	if !slugPattern.MatchString(input.Slug) {
		return nil, Failure(422, "INVALID_SLUG", "Use 3–64 lowercase letters, digits or hyphens for the server slug")
	}
	server := &model.MCPServer{ID: uuid.NewString(), Slug: input.Slug, Name: input.Name, Description: input.Description, Kind: input.Kind,
		Catalog: catalog, Version: input.Version, ConnectionRevision: 1, AuthMode: input.AuthMode, AllowedUserGroups: input.AllowedUserGroups, Connection: input.Connection, Limits: input.Limits, Revision: 1}
	server.Icon, server.Readme = model.MCPEncryptedResult(input.Icon), model.MCPEncryptedResult(input.Readme)
	if catalog {
		server.MetadataRevision = 1
	}
	if input.Connection.PackageDigest != "" {
		return nil, Failure(422, "INVALID_PACKAGE", "Install the configured package to resolve its digest")
	}
	if catalog && server.Kind == "public_knowledge" {
		binding, _ := publicLibraryConnection(input.Connection.URL)
		if input.Connection.Owner != "" && input.Connection.Owner != binding.Owner || input.Connection.KBID != "" && input.Connection.KBID != binding.KBID {
			return nil, Failure(422, "INVALID_LIBRARY", "The library must match the MCP URL")
		}
		server.AuthMode = "engine_app"
		server.Connection.Owner, server.Connection.KBID = binding.Owner, binding.KBID
	} else if catalog && (input.Connection.Owner != "" || input.Connection.KBID != "") {
		return nil, Failure(422, "INVALID_LIBRARY", "Configure the public library using its MCP URL")
	}
	if catalog && server.Kind == "remote_mcp" && PublicBaseURL() != "" && strings.HasPrefix(server.Connection.URL, PublicBaseURL()+"/mcp/providers/knowledge/") {
		return nil, Failure(422, "INVALID_LIBRARY", "Use a public library MCP URL containing its owner and knowledge base ID")
	}
	if err := NormalizeServer(server); err != nil {
		return nil, err
	}
	if catalog && input.ClientSecret != "" {
		if len(input.ClientSecret) > 4096 || server.AuthMode != "oauth2" {
			return nil, Failure(422, "INVALID_CREDENTIAL", "Invalid OAuth client secret")
		}
		ciphertext, err := Seal(input.ClientSecret, "oauth-client:"+server.ID)
		if err != nil {
			return nil, err
		}
		server.CredentialCiphertext = ciphertext
		server.CredentialsConfigured = true
	}
	if input.Headers != nil || input.Env != nil {
		sealed, err := encodeRuntimeSecrets(server, input.Headers, input.Env)
		if err != nil {
			return nil, err
		}
		server.RuntimeSecrets = model.MCPEncryptedResult(sealed)
		server.CredentialsConfigured = server.CredentialsConfigured || sealed != ""
	}
	err := model.DB.Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&model.MCPServer{}).Where("slug = ?", server.Slug).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return Failure(409, "SLUG_EXISTS", "Server slug is already in use")
		}
		if err := tx.Create(server).Error; err != nil {
			return err
		}
		if catalog {
			return nil
		}
		for _, tool := range BuiltinTools(server.Kind) {
			tool.ID, tool.ServerID = uuid.NewString(), server.ID
			if err := tx.Create(&tool).Error; err != nil {
				return err
			}
		}
		return nil
	})
	return server, err
}

func NormalizeServer(server *model.MCPServer) error {
	if server.Catalog {
		if err := server.AfterFind(nil); err != nil {
			return err
		}
	}
	if server.Kind == "knowledge" || PublicBaseURL() != "" && strings.TrimRight(server.Connection.URL, "/") == PublicBaseURL()+"/mcp/providers/knowledge" {
		return Failure(422, "INVALID_PROVIDER", "Unsupported MCP service type")
	}
	server.Name = strings.TrimSpace(server.Name)
	if server.Name == "" || len(server.Name) > 128 || len(server.Description) > 4096 || len(server.Version) > 64 {
		return Failure(422, "INVALID_SERVER", "Server name or description is invalid")
	}
	if len(server.Readme) > 128<<10 {
		return Failure(422, "INVALID_README", "README exceeds 128 KiB")
	}
	if icon := string(server.Icon); icon != "" && !iconKeyPattern.MatchString(icon) {
		raw, ok := strings.CutPrefix(icon, "data:image/png;base64,")
		if !ok || len(raw) > base64.StdEncoding.EncodedLen(128<<10) {
			return Failure(422, "INVALID_ICON", "Select an icon or upload a PNG up to 128 KiB")
		}
		data, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return Failure(422, "INVALID_ICON", "Invalid PNG icon")
		}
		size, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil || size.Width > 1024 || size.Height > 1024 {
			return Failure(422, "INVALID_ICON", "Invalid PNG icon dimensions")
		}
		if _, err = png.Decode(bytes.NewReader(data)); err != nil {
			return Failure(422, "INVALID_ICON", "Invalid PNG icon")
		}
	}
	if server.Limits.TimeoutSeconds == 0 {
		server.Limits.TimeoutSeconds = 60
	}
	if server.Limits.MaxConcurrency == 0 {
		server.Limits.MaxConcurrency = 10
	}
	if server.Limits.TimeoutSeconds < 1 || server.Limits.TimeoutSeconds > 600 || server.Limits.MaxConcurrency < 1 || server.Limits.MaxConcurrency > 1000 {
		return Failure(422, "INVALID_LIMITS", "Server runtime limits are invalid")
	}
	if len(server.AllowedUserGroups) > 100 {
		return Failure(422, "INVALID_GROUPS", "Too many allowed groups")
	}
	for _, group := range server.AllowedUserGroups {
		if group == "" || len(group) > 64 {
			return Failure(422, "INVALID_GROUPS", "Allowed group is invalid")
		}
	}
	if server.AllowedUserGroups == nil {
		server.AllowedUserGroups = model.MCPGroups{}
	}
	if server.Catalog {
		if server.Kind == "public_knowledge" {
			c := server.Connection
			if server.AuthMode != "engine_app" || c.Transport != "streamable_http" || !libraryIdentifier(c.Owner) || !libraryIdentifier(c.KBID) || c.URL != PublicLibraryURL(c.Owner, c.KBID) || c.Command != "" || c.Package != "" || c.ClientID != "" || c.EngineBaseURL != "" || c.Scopes != "" {
				return Failure(422, "INVALID_LIBRARY", "A public library requires a fixed owner and library ID")
			}
			server.Connection = model.MCPConnection{URL: c.URL, Transport: c.Transport, Owner: c.Owner, KBID: c.KBID}
			return nil
		}
		if server.Connection.Owner != "" || server.Connection.KBID != "" {
			return Failure(422, "INVALID_PROVIDER", "Create a new instance to bind a library")
		}
		if server.Connection.Transport == "stdio" {
			return Failure(410, "TRANSPORT_RETIRED", "stdio container execution has been retired")
		}

		if server.Connection.Command != "" || server.Connection.Package != "" || server.Connection.PackageDigest != "" {
			return Failure(422, "INVALID_TRANSPORT", "Commands and packages require stdio")
		}
		channelID, confirmed := server.Connection.ChannelID, server.Connection.ConfirmedBaseURL
		if server.Kind == "deepseek" && channelID > 0 {
			channel, err := model.GetChannelById(channelID, false)
			if err != nil || channel.Type != constant.ChannelTypeDeepSeek {
				return Failure(422, "INVALID_CHANNEL", "A DeepSeek channel is required")
			}
			base := strings.TrimSuffix(strings.TrimRight(channel.GetBaseURL(), "/"), "/v1")
			if _, err := ValidateEndpoint(base); err != nil {
				return err
			}
			if confirmed != "" && confirmed != base {
				return Failure(409, "UPSTREAM_ADDRESS_CHANGED", "Confirm the channel's current upstream address")
			}
			confirmed = base
		}
		server.Connection.URL = strings.TrimSpace(server.Connection.URL)
		if _, err := ValidateEndpoint(server.Connection.URL); err != nil {
			return err
		}
		if !slices.Contains([]string{"streamable_http", "http_sse"}, server.Connection.Transport) {
			return Failure(422, "INVALID_TRANSPORT", "Unsupported MCP transport")
		}
		if server.AuthMode == "" {
			server.AuthMode = "none"
		}
		if len(server.Connection.Scopes) > 2048 {
			return Failure(422, "INVALID_OAUTH_CONFIG", "OAuth scopes are too long")
		}
		scopes := strings.Join(strings.Fields(server.Connection.Scopes), " ")
		if server.AuthMode == "none" && server.Kind == "remote_mcp" {
			server.Connection = model.MCPConnection{URL: server.Connection.URL, Transport: server.Connection.Transport, Scopes: scopes}
			return nil
		}
		if server.AuthMode != "oauth2" || server.Kind != "deepseek" && strings.TrimSpace(server.Connection.ClientID) == "" || len(server.Connection.ClientID) > 256 {
			return Failure(422, "INVALID_OAUTH_CONFIG", "OAuth2 requires a registered client ID")
		}
		server.Connection = model.MCPConnection{URL: server.Connection.URL, Transport: server.Connection.Transport, ClientID: strings.TrimSpace(server.Connection.ClientID), Scopes: scopes, ChannelID: channelID, ConfirmedBaseURL: confirmed}
		return nil
	}
	switch server.Kind {
	case "deepseek":
		if server.AuthMode != "" && server.AuthMode != "api_key" && server.AuthMode != "engine_api_key" {
			return Failure(422, "INVALID_AUTH_MODE", "DeepSeek requires the new-api API key authentication mode")
		}
		server.AuthMode = "api_key"
		channel, err := model.GetChannelById(server.Connection.ChannelID, false)
		if err != nil || channel.Type != constant.ChannelTypeDeepSeek {
			return Failure(422, "INVALID_CHANNEL", "A DeepSeek channel is required")
		}
		base := strings.TrimRight(channel.GetBaseURL(), "/")
		base = strings.TrimSuffix(base, "/v1")
		if _, err := ValidateEndpoint(base); err != nil {
			return err
		}
		if server.Connection.ConfirmedBaseURL != "" && server.Connection.ConfirmedBaseURL != base {
			return Failure(409, "UPSTREAM_ADDRESS_CHANGED", "Confirm the channel's current upstream address")
		}
		server.Connection = model.MCPConnection{ChannelID: channel.Id, ConfirmedBaseURL: base}
	case "remote_mcp":
		if server.AuthMode != "" && server.AuthMode != "casdoor_oauth" {
			return Failure(422, "INVALID_AUTH_MODE", "Remote services require Casdoor OAuth")
		}
		server.AuthMode = "casdoor_oauth"
		if _, err := ValidateEndpoint(server.Connection.URL); err != nil {
			return err
		}
		if !slices.Contains([]string{"streamable_http", "http_sse"}, server.Connection.Transport) {
			return Failure(422, "INVALID_TRANSPORT", "Unsupported MCP transport")
		}
		server.Connection = model.MCPConnection{URL: server.Connection.URL, Transport: server.Connection.Transport}
	default:
		return Failure(422, "INVALID_SERVER_KIND", "Unsupported MCP server type")
	}
	return nil
}

func PatchServer(id string, patch ServerPatch, sensitive bool) (*model.MCPServer, error) {
	server, err := model.GetMCPServer(id)
	if err != nil {
		return nil, err
	}
	if !sensitive && (patch.Headers != nil || patch.Env != nil || patch.AuthMode != nil || patch.ClientSecret != nil || patch.AllowedUserGroups != nil || patch.Connection != nil || patch.Limits != nil) {
		return nil, Failure(403, "PERMISSION_DENIED", "Sensitive MCP configuration permission is required")
	}
	if patch.ExpectedRevision <= 0 {
		return nil, Failure(422, "REVISION_REQUIRED", "Expected revision is required")
	}
	if patch.Icon != nil {
		server.Icon = model.MCPEncryptedResult(*patch.Icon)
	}
	if patch.Readme != nil {
		server.Readme = model.MCPEncryptedResult(*patch.Readme)
	}
	if patch.Name != nil {
		server.Name = *patch.Name
	}
	if patch.Description != nil {
		server.Description = *patch.Description
	}
	if patch.AllowedUserGroups != nil {
		server.AllowedUserGroups = *patch.AllowedUserGroups
	}
	oldConnection, oldAuth := server.Connection, server.AuthMode
	if patch.Version != nil {
		server.Version = *patch.Version
	}
	if patch.AuthMode != nil {
		server.AuthMode = *patch.AuthMode
	}
	if patch.Connection != nil {
		if server.Kind == "public_knowledge" && patch.Connection.Owner == "" && patch.Connection.KBID == "" {
			// URL-only forms retain the immutable library binding.
			patch.Connection.Owner, patch.Connection.KBID = oldConnection.Owner, oldConnection.KBID
		}
		if server.Kind == "public_knowledge" && (patch.Connection.Owner != oldConnection.Owner || patch.Connection.KBID != oldConnection.KBID || patch.Connection.URL != oldConnection.URL) {
			return nil, Failure(422, "IMMUTABLE_LIBRARY", "Create a new instance to change the library")
		}
		provider := LocalProvider(patch.Connection.URL)
		if server.Catalog && ((server.Kind == "remote_mcp" && provider != "") || (server.Kind != "public_knowledge" && server.Kind != "remote_mcp" && provider != server.Kind)) {
			return nil, Failure(422, "INVALID_PROVIDER", "Create a new service to change the local provider")
		}
		if patch.Connection.PackageDigest != "" && patch.Connection.PackageDigest != oldConnection.PackageDigest {
			return nil, Failure(422, "INVALID_PACKAGE", "Install the configured package to resolve its digest")
		}
		server.Connection = *patch.Connection
		server.Connection.PackageDigest = ""
		if oldConnection.Package == server.Connection.Package {
			server.Connection.PackageDigest = oldConnection.PackageDigest
		}
	}
	if patch.Limits != nil {
		server.Limits = *patch.Limits
	}
	if err = NormalizeServer(server); err != nil {
		return nil, err
	}
	updates := map[string]any{"version": server.Version, "name": server.Name, "description": server.Description, "allowed_user_groups": server.AllowedUserGroups, "connection": server.Connection, "limits": server.Limits}
	if patch.Icon != nil || patch.Readme != nil || patch.Name != nil || patch.Description != nil || patch.Version != nil {
		updates["metadata_revision"] = gorm.Expr("COALESCE(metadata_revision, 0) + 1")
		updates["icon"], updates["readme"] = server.Icon, server.Readme
	}
	// Changing a credential destination requires a fresh enable decision.
	if server.Catalog {
		destinationChanged := oldConnection.URL != server.Connection.URL || oldConnection.ClientID != server.Connection.ClientID || oldAuth != server.AuthMode
		runtimeChanged := patch.Env != nil || patch.Headers != nil
		if destinationChanged || oldConnection.Package != server.Connection.Package || oldConnection.Transport != server.Connection.Transport {
			updates["runtime_secrets"] = ""
			server.RuntimeSecrets = ""
		}
		if runtimeChanged {
			current, err := serverCredentials(server)
			if err != nil {
				return nil, err
			}
			if patch.Headers != nil {
				current.Headers = patch.Headers
			}
			if patch.Env != nil {
				current.Env = patch.Env
			}
			encoded, err := encodeRuntimeSecrets(server, current.Headers, current.Env)
			if err != nil {
				return nil, err
			}
			updates["runtime_secrets"] = encoded
		}
		if destinationChanged || patch.ClientSecret != nil {
			var cipher string
			if patch.ClientSecret != nil && *patch.ClientSecret != "" {
				if len(*patch.ClientSecret) > 4096 || server.AuthMode != "oauth2" {
					return nil, Failure(422, "INVALID_CREDENTIAL", "Invalid OAuth client secret")
				}
				cipher, err = Seal(*patch.ClientSecret, "oauth-client:"+id)
				if err != nil {
					return nil, err
				}
			}
			updates["credential_ciphertext"] = cipher
		}
		if oldConnection != server.Connection || oldAuth != server.AuthMode || patch.ClientSecret != nil || runtimeChanged {
			updates["connection_revision"] = gorm.Expr("connection_revision + 1")
			updates["connection_health"] = nil
			updates["enabled"] = false
			// Require rediscovery before a different endpoint can use the old tool prices.
			return updateCatalogConnection(server.ID, patch.ExpectedRevision, updates)
		}
	} else if patch.Connection != nil {
		updates["enabled"] = false
	}
	if err = model.UpdateMCPServer(id, patch.ExpectedRevision, updates); err != nil {
		return nil, err
	}
	return model.GetMCPServer(id)
}

func SetServerStatus(id string, revision int64, enabled bool) (*model.MCPServer, error) {
	return SetServerStatusForUser(context.Background(), id, revision, enabled, 0)
}
func SetServerStatusForUser(ctx context.Context, id string, revision int64, enabled bool, userID int) (*model.MCPServer, error) {
	server, err := model.GetMCPServer(id)
	if err != nil {
		return nil, err
	}
	if enabled {
		if server.Catalog && server.MetadataRevision > 0 && (server.Icon == "" || strings.TrimSpace(string(server.Readme)) == "") {
			return nil, Failure(422, "DISPLAY_REQUIRED", "Add an icon and README before publishing")
		}
		// Publishing is an administrative catalog operation, not a business call.
		// Public-library access is checked with the invoking user's grant at runtime.
		if server.Connection.Transport == "stdio" {
			return nil, Failure(410, "TRANSPORT_RETIRED", "stdio container execution has been retired")
		}

		if server.Catalog {
			var count int64
			if err := model.DB.Model(&model.MCPTool{}).Where("server_id = ? AND enabled = ? AND removed = ? AND review_required = ? AND price_mode IN ?", id, true, false, false, []string{"free", "per_success"}).Count(&count).Error; err != nil {
				return nil, err
			}
			if count == 0 {
				return nil, Failure(422, "NO_ENABLED_TOOLS", "Enable and price at least one tool")
			}
		}
		if !server.Catalog && len(server.AllowedUserGroups) == 0 {
			return nil, Failure(422, "NO_ALLOWED_GROUPS", "Select at least one allowed user group")
		}
		if _, err = encryptionKey(); err != nil {
			return nil, err
		}
		if _, err = ValidateEndpoint(PublicBaseURL(ctx)); err != nil {
			return nil, Failure(503, "MCP_NOT_CONFIGURED", "Configure the public MCP base URL")
		}
		if !server.Catalog && server.Kind == "knowledge" && server.CredentialCiphertext == "" {
			return nil, Failure(422, "CREDENTIAL_REQUIRED", "Configure the engine exchange credential")
		}
	}
	if err = model.UpdateMCPServer(id, revision, map[string]any{"enabled": enabled}); err != nil {
		return nil, err
	}
	return model.GetMCPServer(id)
}

type CredentialsInput struct {
	Env              map[string]string `json:"env,omitempty"`
	ExpectedRevision int64             `json:"expected_revision"`
	ClientSecret     string            `json:"client_secret,omitempty"`
	Headers          map[string]string `json:"headers,omitempty"`
}

func SetCredentials(id string, input CredentialsInput) (*model.MCPServer, error) {
	server, err := model.GetMCPServer(id)
	if err != nil {
		return nil, err
	}
	if input.ExpectedRevision <= 0 || input.ExpectedRevision != server.Revision {
		return nil, model.ErrMCPRevisionConflict
	}
	if server.Catalog {
		if input.ClientSecret != "" || input.Headers == nil && input.Env == nil {
			return nil, Failure(422, "INVALID_CREDENTIAL", "Supply headers or an environment mapping")
		}
		return PatchServer(id, ServerPatch{ExpectedRevision: input.ExpectedRevision, Headers: input.Headers, Env: input.Env}, true)
	}
	if server.Kind == "deepseek" {
		return nil, Failure(422, "CHANNEL_CREDENTIAL_MANAGED", "Manage DeepSeek credentials on the referenced channel")
	}
	if server.Kind == "knowledge" && (input.ClientSecret == "" || len(input.ClientSecret) > 4096 || len(input.Headers) != 0) {
		return nil, Failure(422, "INVALID_CREDENTIAL", "An engine exchange secret is required")
	}
	if server.Kind == "remote_mcp" {
		if input.ClientSecret != "" || len(input.Headers) > 16 {
			return nil, Failure(422, "INVALID_CREDENTIAL", "Remote MCP credentials must be service headers")
		}
		for key, value := range input.Headers {
			canonical := http.CanonicalHeaderKey(key)
			if canonical == "" || strings.ContainsAny(key+value, "\r\n") || len(value) > 8192 ||
				slices.Contains([]string{"Host", "Connection", "Content-Length", "Transfer-Encoding", "Content-Type", "Accept", "Origin", "Cookie"}, canonical) || strings.HasPrefix(strings.ToLower(key), "mcp-") || strings.HasPrefix(strings.ToLower(key), "sec-") {
				return nil, Failure(422, "INVALID_CREDENTIAL", "Credential header is not allowed")
			}
		}
	}
	input.ExpectedRevision = 0
	encoded, err := common.Marshal(input)
	if err != nil || len(encoded) > 32<<10 {
		return nil, Failure(422, "INVALID_CREDENTIAL", "Credential payload is too large")
	}
	ciphertext, err := Seal(string(encoded), "server:"+id)
	if err != nil {
		return nil, err
	}
	// Check the same expected revision again at the database mutation boundary.
	if err = model.UpdateMCPServer(id, server.Revision, map[string]any{"credential_ciphertext": ciphertext, "credential_revision": server.CredentialRevision + 1}); err != nil {
		return nil, err
	}
	return model.GetMCPServer(id)
}

func ErrorResponse(err error) *Error {
	var typed *Error
	if errors.As(err, &typed) {
		return typed
	}
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return Failure(404, "RESOURCE_NOT_FOUND", "Resource is unavailable")
	case errors.Is(err, model.ErrMCPRevisionConflict):
		return Failure(409, "REVISION_CONFLICT", "Configuration was changed by another request")
	case errors.Is(err, model.ErrMCPIdempotencyConflict):
		return Failure(409, "IDEMPOTENCY_CONFLICT", "This idempotency key was used with different arguments")
	case errors.Is(err, model.ErrMCPServerEnabled):
		return Failure(409, "SERVER_ENABLED", "Disable the server before archiving")
	default:
		return Failure(503, "MCP_UNAVAILABLE", "MCP service is temporarily unavailable")
	}
}

func LocalProvider(endpoint string) string {
	base := PublicBaseURL()
	if base == "" {
		return ""
	}
	if strings.TrimSpace(endpoint) == base+"/mcp/providers/deepseek" {
		return "deepseek"
	}
	if _, ok := publicLibraryConnection(strings.TrimSpace(endpoint)); ok {
		return "public_knowledge"
	}
	return ""
}

// LocalProviderSlug gives each built-in endpoint a unique catalog registration.
func LocalProviderSlug(endpoint string) string {
	kind := LocalProvider(endpoint)
	if kind == "public_knowledge" {
		return "provider-knowledge-" + Digest(strings.TrimSpace(endpoint))[:40]
	}
	if kind == "" {
		return ""
	}
	return "provider-" + kind
}

func updateCatalogConnection(id string, revision int64, updates map[string]any) (*model.MCPServer, error) {
	err := model.DB.Transaction(func(tx *gorm.DB) error {
		updates["revision"] = gorm.Expr("revision + 1")
		result := tx.Model(&model.MCPServer{}).Where("id = ? AND revision = ? AND archived = ?", id, revision, false).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return model.ErrMCPRevisionConflict
		}
		return tx.Model(&model.MCPTool{}).Where("server_id = ?", id).Updates(map[string]any{"enabled": false, "review_required": true, "revision": gorm.Expr("revision + 1")}).Error
	})
	if err != nil {
		return nil, err
	}
	return model.GetMCPServer(id)
}

func providerConfig(server *model.MCPServer) (*model.MCPServer, error) {
	if !server.Catalog || server.Kind == "remote_mcp" || server.Kind == "deepseek" && server.Connection.ChannelID != 0 {
		return server, nil
	}
	configured := *server
	switch server.Kind {
	case "deepseek":
		return nil, Failure(503, "MCP_NOT_CONFIGURED", "Select a DeepSeek channel in the service settings")
	case "knowledge":
		configured.Connection.EngineBaseURL = strings.TrimRight(ConfigValue("knowledge_engine_url"), "/")
		configured.Connection.ClientID = ConfigValue("knowledge_client_id")
		if configured.Connection.EngineBaseURL == "" || configured.Connection.ClientID == "" || ConfigValue("knowledge_client_secret") == "" {
			return nil, Failure(503, "MCP_NOT_CONFIGURED", "Knowledge provider is not configured")
		}
	}
	return &configured, nil
}

func encodeRuntimeSecrets(server *model.MCPServer, headers, env map[string]string) (string, error) {
	if len(headers)+len(env) == 0 {
		return "", nil
	}
	if server.Kind != "remote_mcp" || len(headers) > 32 || len(env) > 0 || len(headers) > 0 && server.Connection.Transport == "stdio" || len(env) > 0 && server.Connection.Transport != "stdio" {
		return "", Failure(422, "INVALID_CREDENTIAL", "Credentials do not match the selected transport")
	}
	canonical := make(map[string]string, len(headers))
	for key, value := range headers {
		name := http.CanonicalHeaderKey(key)
		if !httpguts.ValidHeaderFieldName(key) || !httpguts.ValidHeaderFieldValue(value) || len(value) > 8192 || slices.Contains([]string{"Connection", "Proxy-Authorization", "Host", "Content-Length", "Transfer-Encoding", "Content-Type", "Accept", "Origin", "Cookie", "Upgrade", "Trailer", "Te", "Forwarded"}, name) || strings.HasPrefix(name, "Mcp-") || strings.HasPrefix(name, "Sec-") || strings.HasPrefix(name, "X-Forwarded-") || name == "Authorization" && server.AuthMode == "oauth2" {
			return "", Failure(422, "INVALID_CREDENTIAL", "Header is not allowed")
		}
		if _, exists := canonical[name]; exists {
			return "", Failure(422, "INVALID_CREDENTIAL", "Duplicate header")
		}
		canonical[name] = value
	}
	for key, value := range env {
		if !variableName.MatchString(key) || strings.ContainsAny(value, "\x00\r\n") || len(value) > 8192 {
			return "", Failure(422, "INVALID_CREDENTIAL", "Invalid environment variable")
		}
	}
	data, err := common.Marshal(CredentialsInput{Headers: canonical, Env: env})
	if err != nil || len(data) > 32<<10 {
		return "", Failure(422, "INVALID_CREDENTIAL", "Credential payload exceeds 32 KiB")
	}
	return Seal(string(data), "runtime:"+server.ID)
}
