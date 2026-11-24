package mcp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"golang.org/x/sync/singleflight"
)

var oauthRegistrations singleflight.Group

// Registration belongs to the service, not to an individual user's grant.
// Re-read under singleflight so concurrent first authorizations reuse the saved
// client. Optimistic revisions prevent stale registrations overwriting edits.
func registeredOAuthClient(ctx context.Context, server *model.MCPServer, meta oauthDiscovery) (*model.MCPServer, error) {
	if server.Connection.ClientID != "" {
		return server, nil
	}
	key := server.ID + ":" + strconv.FormatInt(server.ConnectionRevision, 10)
	result := oauthRegistrations.DoChan(key, func() (any, error) {
		shared, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		current, err := model.GetMCPServer(server.ID)
		if err != nil {
			return nil, err
		}
		if current.ConnectionRevision != server.ConnectionRevision || current.Connection.URL != server.Connection.URL {
			return nil, model.ErrMCPRevisionConflict
		}
		if current.Connection.ClientID != "" {
			return current, nil
		}
		if meta.RegistrationURL == "" {
			return nil, Failure(422, "OAUTH_CLIENT_REGISTRATION_REQUIRED", "The authorization server does not support dynamic client registration")
		}
		endpoint, err := ValidateEndpoint(meta.RegistrationURL)
		if err != nil {
			return nil, err
		}
		method := "none"
		local := current.Catalog && current.Kind == "deepseek"
		if local {
			issuer := strings.TrimRight(ConfigValue("casdoor_issuer", ctx), "/")
			organization := strings.TrimSpace(ConfigValue("casdoor_organization", ctx))
			trusted, err := url.Parse(issuer)
			if err != nil || issuer == "" || meta.Issuer != issuer || organization == "" ||
				endpoint.Scheme != trusted.Scheme || !strings.EqualFold(endpoint.Host, trusted.Host) {
				return nil, Failure(503, "MCP_NOT_CONFIGURED", "Configure the trusted MCP OAuth issuer and organization")
			}
			query := endpoint.Query()
			query.Set("organization", organization)
			endpoint.RawQuery = query.Encode()
			// The same confidential client performs live token introspection.
			switch {
			case len(meta.Methods) == 0 || slices.Contains(meta.Methods, "client_secret_basic"):
				method = "client_secret_basic"
			case slices.Contains(meta.Methods, "client_secret_post"):
				method = "client_secret_post"
			default:
				return nil, Failure(422, "INVALID_OAUTH_CONFIG", "A confidential OAuth client is required")
			}
		} else if !slices.Contains(meta.Methods, "none") && len(meta.Methods) > 0 {
			method = meta.Methods[0]
			if method != "client_secret_post" && method != "client_secret_basic" {
				return nil, Failure(422, "INVALID_OAUTH_CONFIG", "Unsupported OAuth client authentication method")
			}
		}
		scopes := strings.Fields(current.Connection.Scopes)
		if len(scopes) == 0 {
			scopes = meta.Scopes
		}
		body, err := common.Marshal(map[string]any{
			"client_name": "New API MCP", "redirect_uris": []string{PublicBaseURL(ctx) + "/api/mcp/oauth/callback"},
			"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
			"token_endpoint_auth_method": method, "scope": strings.Join(scopes, " "),
		})
		if err != nil {
			return nil, err
		}
		client, err := NewHTTPClient(meta.RegistrationURL, 10*time.Second)
		if err != nil {
			return nil, err
		}
		defer client.CloseIdleConnections()
		req, err := http.NewRequestWithContext(shared, http.MethodPost, endpoint.String(), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, Failure(502, "OAUTH_REGISTRATION_FAILED", "OAuth client registration failed")
		}
		defer resp.Body.Close()
		var registered struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
			Method       string `json:"token_endpoint_auth_method"`
		}
		if resp.StatusCode != 200 && resp.StatusCode != 201 ||
			common.DecodeJson(io.LimitReader(resp.Body, 128<<10), &registered) != nil ||
			strings.TrimSpace(registered.ClientID) == "" || len(registered.ClientID) > 256 || len(registered.ClientSecret) > 4096 ||
			method != "none" && registered.ClientSecret == "" || registered.Method != "" && registered.Method != method {
			return nil, Failure(502, "OAUTH_REGISTRATION_FAILED", "OAuth client registration failed")
		}
		current.Connection.ClientID = registered.ClientID
		if registered.ClientSecret != "" {
			current.CredentialCiphertext, err = Seal(registered.ClientSecret, "oauth-client:"+current.ID)
			if err != nil {
				return nil, err
			}
		}
		err = model.UpdateMCPServer(current.ID, current.Revision, map[string]any{
			"connection": current.Connection, "credential_ciphertext": current.CredentialCiphertext,
		})
		if err != nil && !errors.Is(err, model.ErrMCPRevisionConflict) {
			return nil, err
		}
		// Another replica may have won the optimistic write. Use its client only
		// when the endpoint/configuration still matches this authorization flow.
		saved, err := model.GetMCPServer(current.ID)
		if err != nil {
			return nil, err
		}
		if saved.ConnectionRevision != current.ConnectionRevision || saved.Connection.URL != current.Connection.URL || saved.Connection.ClientID == "" {
			return nil, model.ErrMCPRevisionConflict
		}
		return saved, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case response := <-result:
		if response.Err != nil {
			return nil, response.Err
		}
		copy := *response.Val.(*model.MCPServer)
		return &copy, nil
	}
}
