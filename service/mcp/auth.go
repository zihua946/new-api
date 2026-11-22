package mcp

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang-jwt/jwt/v5"
)

type Actor struct {
	Delegation      *MCPDelegation
	DelegationToken string
	Draft           bool
	InstallationID  string
	UserID          int
	TokenID         int
	Group           string
	Subject         string
	AccessToken     string
	ExpiresAt       time.Time
	CanCall         bool
}

var oidcProviders sync.Map

func oauthProvider(ctx context.Context) (*oidc.Provider, *http.Client, error) {
	issuer := strings.TrimRight(os.Getenv("MCP_CASDOOR_ISSUER"), "/")
	client, err := NewHTTPClient(issuer, 10*time.Second)
	if err != nil {
		return nil, nil, Failure(503, "MCP_NOT_CONFIGURED", "MCP authorization provider is not configured")
	}
	if cached, ok := oidcProviders.Load(issuer); ok {
		return cached.(*oidc.Provider), client, nil
	}
	// The provider retains its JWKS context beyond this request. The HTTP
	// client's timeout bounds discovery and later key refreshes.
	provider, err := oidc.NewProvider(oidc.ClientContext(context.WithoutCancel(ctx), client), issuer)
	if err != nil {
		return nil, nil, Failure(503, "AUTHORIZATION_UNAVAILABLE", "Authorization provider is unavailable")
	}
	actual, _ := oidcProviders.LoadOrStore(issuer, provider)
	return actual.(*oidc.Provider), client, nil
}

func ResourceURL(server *model.MCPServer) string {
	if server.Catalog && server.Kind != "remote_mcp" {
		return server.Connection.URL
	}
	return PublicBaseURL() + "/mcp/" + server.Slug
}

func Authenticate(ctx context.Context, server *model.MCPServer, authorization, clientIP string) (*Actor, error) {
	scheme, raw, ok := strings.Cut(authorization, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || raw == "" || len(raw) > 32768 || strings.ContainsAny(raw, " \r\n\t") {
		return nil, Failure(401, "UNAUTHORIZED", "A valid bearer credential is required")
	}
	if server.AuthMode == "casdoor_oauth" {
		return AuthenticateOAuth(ctx, server, raw)
	}
	if !server.Catalog && (server.Kind != "deepseek" || server.AuthMode != "api_key" && server.AuthMode != "engine_api_key") {
		return nil, Failure(503, "MCP_NOT_CONFIGURED", "MCP authentication mode is not configured")
	}
	key := strings.TrimPrefix(raw, "sk-")
	token, err := model.GetTokenByKey(key, true)
	if err != nil || token.Status != common.TokenStatusEnabled || token.ExpiredTime != -1 && token.ExpiredTime <= common.GetTimestamp() {
		return nil, Failure(401, "UNAUTHORIZED", "API credential is invalid or inactive")
	}
	if ips := token.GetIpLimits(); len(ips) > 0 && !common.IsIpInCIDRList(net.ParseIP(clientIP), ips) {
		return nil, Failure(403, "IP_NOT_ALLOWED", "API credential does not allow this client address")
	}
	user, err := model.GetUserById(token.UserId, false)
	if err != nil || user.Status != common.UserStatusEnabled {
		return nil, Failure(403, "USER_DISABLED", "User is unavailable")
	}
	actor := &Actor{UserID: user.Id, TokenID: token.Id, Group: user.Group, CanCall: true}
	if token.ExpiredTime != -1 {
		actor.ExpiresAt = time.Unix(token.ExpiredTime, 0)
	}
	if !server.Catalog && !slices.Contains(server.AllowedUserGroups, actor.Group) {
		return nil, Failure(403, "SERVER_ACCESS_DENIED", "MCP server is not available to this user group")
	}
	return actor, nil
}

// OAuth access tokens are verified both cryptographically and against the
// issuing authority's live grant state. No email/name-based account matching.
func AuthenticateOAuth(ctx context.Context, server *model.MCPServer, raw string) (*Actor, error) {
	provider, client, err := oauthProvider(ctx)
	if err != nil {
		return nil, err
	}
	defer client.CloseIdleConnections()
	resource := ResourceURL(server)
	verified, err := provider.Verifier(&oidc.Config{ClientID: resource, SupportedSigningAlgs: []string{"RS256"}}).Verify(oidc.ClientContext(ctx, client), raw)
	if err != nil {
		return nil, Failure(401, "UNAUTHORIZED", "Authorization token is invalid or expired")
	}
	var claims struct {
		Scope           string `json:"scope"`
		AuthorizedParty string `json:"azp"`
		Owner           string `json:"owner"`
	}
	if verified.Claims(&claims) != nil || verified.Subject == "" || claims.AuthorizedParty == "" || len(verified.Audience) != 1 || verified.Audience[0] != resource {
		return nil, Failure(401, "UNAUTHORIZED", "Authorization token is not valid for this resource")
	}
	if !slices.Contains(strings.Fields(claims.Scope), "mcp:tools:read") {
		return nil, Failure(403, "INSUFFICIENT_SCOPE", "The mcp:tools:read scope is required")
	}
	organization := os.Getenv("MCP_CASDOOR_ORGANIZATION")
	if organization == "" || claims.Owner != organization {
		return nil, Failure(403, "ORGANIZATION_NOT_ALLOWED", "Authorization organization is not allowed")
	}
	clientID, secret := os.Getenv("MCP_OAUTH_CLIENT_ID"), os.Getenv("MCP_OAUTH_CLIENT_SECRET")
	if server.Catalog && server.Kind == "deepseek" {
		// Load the authoritative persisted registration, including when the
		// protocol handler deliberately supplied only a public server view.
		registered, err := model.GetMCPServer(server.ID)
		if err != nil || !registered.Catalog || registered.Kind != "deepseek" || ResourceURL(registered) != resource {
			return nil, Failure(403, "CLIENT_NOT_ALLOWED", "OAuth client is not registered for MCP access")
		}
		clientID = registered.Connection.ClientID
		if clientID == "" || claims.AuthorizedParty != clientID || registered.CredentialCiphertext == "" {
			return nil, Failure(403, "CLIENT_NOT_ALLOWED", "OAuth client is not registered for MCP access")
		}
		secret, err = Open(registered.CredentialCiphertext, "oauth-client:"+registered.ID)
		if err != nil {
			return nil, err
		}
	} else {
		allowedClients := os.Getenv("MCP_OAUTH_ALLOWED_CLIENT_IDS")
		if allowedClients == "" {
			allowedClients = clientID
		}
		clientAllowed := false
		for id := range strings.SplitSeq(allowedClients, ",") {
			if strings.TrimSpace(id) == claims.AuthorizedParty {
				clientAllowed = true
				break
			}
		}
		if !clientAllowed {
			return nil, Failure(403, "CLIENT_NOT_ALLOWED", "OAuth client is not registered for MCP access")
		}
	}
	if clientID == "" || secret == "" {
		return nil, Failure(503, "MCP_NOT_CONFIGURED", "MCP resource verifier is not configured")
	}
	form := url.Values{"token": {raw}, "token_type_hint": {"access_token"}, "resource": {resource}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(os.Getenv("MCP_CASDOOR_ISSUER"), "/")+"/api/login/oauth/introspect", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.SetBasicAuth(clientID, secret)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(request)
	if err != nil {
		return nil, Failure(503, "AUTHORIZATION_UNAVAILABLE", "Authorization state cannot be verified")
	}
	defer response.Body.Close()
	var active struct {
		Active   bool             `json:"active"`
		Subject  string           `json:"sub"`
		ClientID string           `json:"client_id"`
		Scope    string           `json:"scope"`
		Issuer   string           `json:"iss"`
		Audience jwt.ClaimStrings `json:"aud"`
		Exp      int64            `json:"exp"`
	}
	if response.StatusCode != http.StatusOK || common.DecodeJson(response.Body, &active) != nil {
		return nil, Failure(503, "AUTHORIZATION_UNAVAILABLE", "Authorization state cannot be verified")
	}
	if !active.Active || active.Issuer != strings.TrimRight(os.Getenv("MCP_CASDOOR_ISSUER"), "/") || len(active.Audience) != 1 || active.Audience[0] != resource || active.Subject != verified.Subject || active.ClientID != claims.AuthorizedParty || active.Exp <= time.Now().Unix() || !slices.Contains(strings.Fields(active.Scope), "mcp:tools:read") {
		return nil, Failure(401, "AUTHORIZATION_REVOKED", "Authorization is no longer active")
	}
	user, err := casdoorUser(verified.Subject)
	if err != nil {
		return nil, err
	}
	if !server.Catalog && !slices.Contains(server.AllowedUserGroups, user.Group) {
		return nil, Failure(403, "SERVER_ACCESS_DENIED", "MCP server is not available to this user group")
	}
	expires := verified.Expiry
	if time.Unix(active.Exp, 0).Before(expires) {
		expires = time.Unix(active.Exp, 0)
	}
	return &Actor{UserID: user.Id, Group: user.Group, Subject: verified.Subject, AccessToken: raw, ExpiresAt: expires,
		CanCall: slices.Contains(strings.Fields(claims.Scope), "mcp:tools:call") && slices.Contains(strings.Fields(active.Scope), "mcp:tools:call")}, nil
}

func CanUseTool(server *model.MCPServer, tool *model.MCPTool, actor *Actor, test bool) bool {
	return actor != nil && (server.Catalog || slices.Contains(server.AllowedUserGroups, actor.Group)) &&
		(actor.Delegation == nil || actor.Delegation.AllowsServer(server.ID)) &&
		(tool.AllowedUserGroups == nil || slices.Contains(tool.AllowedUserGroups, actor.Group)) &&
		!server.Archived && !tool.Removed && !tool.ReviewRequired && (test || server.Enabled && tool.Enabled)
}

// InstallationActor binds upstream credentials to the platform owner.
func InstallationActor(ctx context.Context, server *model.MCPServer, actor *Actor, admin bool) (*Actor, error) {
	if actor.Delegation != nil && (admin || !actor.Delegation.AllowsServer(server.ID)) {
		return nil, Failure(403, "DELEGATION_ACCESS_DENIED", "The application cannot access this MCP service")
	}
	var row *model.MCPInstallation
	var err error
	if admin {
		row, err = Install(server.ID, actor.UserID, true)
	} else {
		row, err = FindInstallation(server.ID, actor.UserID, false)
	}
	if err != nil {
		return nil, err
	}
	if !admin && (!row.Authorized || row.ConnectionRevision != server.ConnectionRevision) {
		return nil, Failure(403, "INSTALLATION_DISCONNECTED", "Enable this MCP installation before use")
	}
	if server.Kind == "public_knowledge" {
		copy := *actor
		copy.InstallationID = row.ID
		copy.Draft = admin && !server.Enabled
		if _, _, err := loadEngineGrant(ctx, actor.UserID); err != nil {
			return nil, err
		}
		return &copy, nil
	}
	token, err := InstallationToken(ctx, server, row)
	if err != nil {
		return nil, err
	}
	copy := *actor
	if server.Kind != "remote_mcp" {
		verified, err := AuthenticateOAuth(ctx, server, token)
		if err != nil {
			return nil, err
		}
		if verified.UserID != actor.UserID {
			return nil, Failure(403, "IDENTITY_MISMATCH", "OAuth user does not match installation owner")
		}
		copy = *verified
		copy.TokenID = actor.TokenID
		copy.Delegation, copy.DelegationToken = actor.Delegation, actor.DelegationToken
		if !actor.ExpiresAt.IsZero() && actor.ExpiresAt.Before(copy.ExpiresAt) {
			copy.ExpiresAt = actor.ExpiresAt
		}
	} else {
		copy.AccessToken = token
	}
	copy.InstallationID = row.ID
	return &copy, nil
}

func casdoorUser(subject string) (*model.User, error) {
	providerID, _ := strconv.Atoi(os.Getenv("MCP_CASDOOR_PROVIDER_ID"))
	var configured model.CustomOAuthProvider
	issuer := strings.TrimRight(os.Getenv("MCP_CASDOOR_ISSUER"), "/")
	if providerID <= 0 || model.DB.First(&configured, providerID).Error != nil || !configured.Enabled || configured.UserIdField != "sub" || configured.WellKnown != issuer+"/.well-known/openid-configuration" {
		return nil, Failure(503, "MCP_NOT_CONFIGURED", "MCP identity provider binding is not configured")
	}
	var binding model.UserOAuthBinding
	if model.DB.Where("provider_id = ? AND provider_user_id = ?", providerID, subject).First(&binding).Error != nil || subtle.ConstantTimeCompare([]byte(binding.ProviderUserId), []byte(subject)) != 1 {
		return nil, Failure(403, "IDENTITY_NOT_LINKED", "Sign in to new-api with this Casdoor account first")
	}
	user, err := model.GetUserById(binding.UserId, false)
	if err != nil || user.Status != common.UserStatusEnabled {
		return nil, Failure(403, "USER_DISABLED", "User is unavailable")
	}
	return user, nil
}
