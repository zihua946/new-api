package mcp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const mcpDelegationScope = "mcp:tools:read mcp:tools:call"

// Public claims describe only the MCP delegation. Sensitive upstream state is
// authenticated and encrypted separately; it must never become readable claims.
type mcpDelegationClaims struct {
	TokenUse       string `json:"token_use"`
	ClientID       string `json:"azp"`
	Scope          string `json:"scope"`
	EncryptedGrant string `json:"grant"`
	jwt.RegisteredClaims
}

func mcpDelegationSigningKey() ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(os.Getenv("MCP_ENCRYPTION_KEY"))
	if err != nil || len(key) != 32 {
		return nil, Failure(503, "MCP_NOT_CONFIGURED", "MCP encryption key is not configured")
	}
	// Domain separation keeps MCP signatures independent of both grant encryption
	// and dashboard JWTs, which use a different root key and audience.
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("new-api/mcp-delegation-jwt/v1"))
	return mac.Sum(nil), nil
}

// Trusted applications may use only the intersection of their server allowlist
// and the user's existing installations. Registration never grants upstream access.
type trustedMCPApplication struct {
	Generation           string   `json:"generation"`
	Enabled              bool     `json:"enabled"`
	ExchangeSecretSHA256 string   `json:"exchange_secret_sha256"`
	CasdoorClientSecret  string   `json:"casdoor_client_secret"`
	ServerIDs            []string `json:"server_ids"`
	MaxChargeQuota       int      `json:"max_charge_quota"`
}

// The upstream token is encrypted, never exposed as JWT claims or persisted in
// another grant table. Live introspection makes Casdoor revocation effective on
// the next request, including after a new-api restart or on another replica.
type MCPDelegation struct {
	ClientID       string   `json:"client_id"`
	UserID         int      `json:"user_id"`
	Subject        string   `json:"sub"`
	AuthVersion    int64    `json:"auth_version"`
	Application    string   `json:"application"`
	AccessToken    string   `json:"upstream_token"`
	IssuedAt       int64    `json:"iat"`
	ExpiresAt      int64    `json:"exp"`
	MaxChargeQuota int      `json:"-"`
	ServerIDs      []string `json:"-"`
}

type MCPExchangeResult struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
	ExpiresAt   int64  `json:"expires_at"`
	Scope       string `json:"scope"`
	GatewayURL  string `json:"gateway_url"`
}

func trustedMCPApp(clientID string) (*trustedMCPApplication, string, error) {
	invalid := Failure(401, "INVALID_APPLICATION", "Application authentication failed")
	var row model.MCPTrustedApplication
	if err := model.DB.First(&row, "id = ?", clientID).Error; err != nil {
		return nil, "", invalid
	}
	raw, err := Open(string(row.Payload), "mcp-app:"+clientID)
	if err != nil {
		return nil, "", err
	}
	var app trustedMCPApplication
	if common.UnmarshalJsonStr(raw, &app) != nil || !app.Enabled {
		return nil, "", invalid
	}

	digest, err := hex.DecodeString(app.ExchangeSecretSHA256)
	if err != nil || len(digest) != sha256.Size || app.CasdoorClientSecret == "" || len(app.ServerIDs) == 0 || app.MaxChargeQuota <= 0 || app.MaxChargeQuota > common.MaxQuota {
		return nil, "", Failure(503, "MCP_NOT_CONFIGURED", "MCP trusted application configuration is invalid")
	}
	canonical, err := common.Marshal(app)
	if err != nil {
		return nil, "", err
	}
	return &app, Digest(string(canonical)), nil
}

// verifyDelegatingUser consumes an access token for the registered Casdoor
// application, not an engine JWT, ID token, API key or MCP-resource token.
// mcp:gateway must be separately consented to in Casdoor; system.login is insufficient.
func verifyDelegatingUser(ctx context.Context, clientID, raw string, app *trustedMCPApplication) (*model.User, string, time.Time, error) {
	invalid := Failure(401, "INVALID_USER_AUTHORIZATION", "MCP delegation authorization is invalid or inactive")
	if raw == "" || len(raw) > 32768 {
		return nil, "", time.Time{}, invalid
	}
	provider, client, err := oauthProvider(ctx)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	defer client.CloseIdleConnections()
	identity, err := provider.Verifier(&oidc.Config{ClientID: clientID, SupportedSigningAlgs: []string{"RS256"}}).Verify(oidc.ClientContext(ctx, client), raw)
	var claims struct {
		Owner     string `json:"owner"`
		Party     string `json:"azp"`
		Scope     string `json:"scope"`
		IssuedAt  int64  `json:"iat"`
		NotBefore int64  `json:"nbf"`
	}
	now := time.Now()
	organization := ConfigValue("casdoor_organization", ctx)
	if err != nil || identity.Claims(&claims) != nil || identity.Subject == "" || len(identity.Audience) != 1 || identity.Audience[0] != clientID || claims.Party != "" && claims.Party != clientID || organization == "" || claims.Owner != organization || claims.IssuedAt <= 0 || claims.IssuedAt > now.Unix()+30 || claims.NotBefore > now.Unix() || !slices.Contains(strings.Fields(claims.Scope), "mcp:gateway") {
		return nil, "", time.Time{}, invalid
	}
	form := url.Values{"token": {raw}, "token_type_hint": {"access_token"}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(ConfigValue("casdoor_issuer", ctx), "/")+"/api/login/oauth/introspect", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, "", time.Time{}, err
	}
	request.SetBasicAuth(clientID, app.CasdoorClientSecret)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(request)
	if err != nil {
		return nil, "", time.Time{}, Failure(503, "AUTHORIZATION_UNAVAILABLE", "Casdoor authorization state cannot be verified")
	}
	defer response.Body.Close()
	var active struct {
		Active   bool             `json:"active"`
		Subject  string           `json:"sub"`
		ClientID string           `json:"client_id"`
		Issuer   string           `json:"iss"`
		Audience jwt.ClaimStrings `json:"aud"`
		Scope    string           `json:"scope"`
		Exp      int64            `json:"exp"`
	}
	if response.StatusCode != http.StatusOK || common.DecodeJson(response.Body, &active) != nil {
		return nil, "", time.Time{}, Failure(503, "AUTHORIZATION_UNAVAILABLE", "Casdoor authorization state cannot be verified")
	}
	if !active.Active || active.Issuer != strings.TrimRight(ConfigValue("casdoor_issuer", ctx), "/") || active.Subject != identity.Subject || active.ClientID != clientID || len(active.Audience) != 1 || active.Audience[0] != clientID || active.Exp <= now.Unix() || !slices.Contains(strings.Fields(active.Scope), "mcp:gateway") {
		return nil, "", time.Time{}, invalid
	}
	user, err := casdoorUser(identity.Subject, ctx)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	expiry := identity.Expiry
	if liveExpiry := time.Unix(active.Exp, 0); liveExpiry.Before(expiry) {
		expiry = liveExpiry
	}
	return user, identity.Subject, expiry, nil
}

func delegationPurpose() string {
	return "new-api/mcp-delegation-jwt/v1:" + PublicBaseURL() + ":" + strings.TrimRight(ConfigValue("casdoor_issuer"), "/")
}

func ExchangeMCPApplication(ctx context.Context, clientID, secret, raw string) (*MCPExchangeResult, *Actor, error) {
	app, revision, err := trustedMCPApp(clientID)
	if err != nil {
		return nil, nil, err
	}
	expected, _ := hex.DecodeString(app.ExchangeSecretSHA256)
	actual := sha256.Sum256([]byte(secret))
	if secret == "" || len(secret) > 4096 || subtle.ConstantTimeCompare(expected, actual[:]) != 1 {
		return nil, nil, Failure(401, "INVALID_APPLICATION", "Application authentication failed")
	}
	if _, err = ValidateEndpoint(PublicBaseURL(ctx)); err != nil {
		return nil, nil, Failure(503, "MCP_NOT_CONFIGURED", "MCP public URL is not configured")
	}
	user, subject, upstreamExpiry, err := verifyDelegatingUser(ctx, clientID, raw, app)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now().Truncate(time.Second)
	expires := now.Add(5 * time.Minute)
	if upstreamExpiry.Before(expires) {
		expires = upstreamExpiry.Truncate(time.Second)
	}
	if !expires.After(now) {
		return nil, nil, Failure(401, "INVALID_USER_AUTHORIZATION", "MCP delegation authorization has expired")
	}
	delegation := &MCPDelegation{ClientID: clientID, UserID: user.Id, Subject: subject, AuthVersion: user.AuthVersion, Application: revision, AccessToken: raw, IssuedAt: now.Unix(), ExpiresAt: expires.Unix(), ServerIDs: app.ServerIDs, MaxChargeQuota: app.MaxChargeQuota}
	data, err := common.Marshal(delegation)
	if err != nil {
		return nil, nil, err
	}
	sealed, err := Seal(string(data), delegationPurpose())
	if err != nil {
		return nil, nil, err
	}
	key, err := mcpDelegationSigningKey()
	if err != nil {
		return nil, nil, err
	}
	claims := mcpDelegationClaims{
		TokenUse: "mcp_delegation", ClientID: clientID, Scope: mcpDelegationScope, EncryptedGrant: sealed,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: PublicBaseURL(ctx), Subject: strconv.Itoa(user.Id), Audience: jwt.ClaimStrings{PublicBaseURL(ctx) + "/mcp/gateway"},
			IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(expires), ID: uuid.NewString(),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	token.Header["typ"] = "at+jwt"
	signed, err := token.SignedString(key)
	if err != nil {
		return nil, nil, err
	}
	actor := &Actor{UserID: user.Id, Group: user.Group, Subject: subject, CanCall: true, ExpiresAt: expires, Delegation: delegation}
	return &MCPExchangeResult{AccessToken: signed, TokenType: "Bearer", ExpiresIn: expires.Unix() - now.Unix(), ExpiresAt: expires.Unix(), Scope: mcpDelegationScope, GatewayURL: PublicBaseURL(ctx) + "/mcp/gateway"}, actor, nil
}

func AuthenticateDelegation(ctx context.Context, authorization string) (*Actor, error) {
	invalid := Failure(401, "INVALID_DELEGATION", "A valid MCP delegation bearer token is required")
	scheme, raw, ok := strings.Cut(authorization, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || raw == "" || len(raw) > 65536 || strings.ContainsAny(raw, " \r\n\t") {
		return nil, invalid
	}
	key, err := mcpDelegationSigningKey()
	if err != nil {
		return nil, err
	}
	var claims mcpDelegationClaims
	parsed, err := jwt.ParseWithClaims(raw, &claims, func(*jwt.Token) (any, error) { return key, nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer(PublicBaseURL(ctx)), jwt.WithAudience(PublicBaseURL(ctx)+"/mcp/gateway"), jwt.WithExpirationRequired(), jwt.WithIssuedAt())
	if err != nil || !parsed.Valid || parsed.Header["typ"] != "at+jwt" || claims.TokenUse != "mcp_delegation" || claims.ClientID == "" || claims.Scope != mcpDelegationScope || len(claims.Audience) != 1 || claims.ID == "" || claims.IssuedAt == nil || claims.NotBefore == nil || claims.ExpiresAt == nil {
		return nil, invalid
	}
	plain, err := Open(claims.EncryptedGrant, delegationPurpose())
	if err != nil {
		return nil, invalid
	}
	var delegation MCPDelegation
	now := time.Now().Unix()
	if common.UnmarshalJsonStr(plain, &delegation) != nil || delegation.UserID <= 0 || delegation.ExpiresAt <= now || delegation.IssuedAt > now || delegation.ExpiresAt <= delegation.IssuedAt || delegation.ExpiresAt-delegation.IssuedAt > 300 {
		return nil, invalid
	}
	if claims.Subject != strconv.Itoa(delegation.UserID) || claims.ClientID != delegation.ClientID || claims.IssuedAt.Unix() != delegation.IssuedAt || claims.NotBefore.Unix() != delegation.IssuedAt || claims.ExpiresAt.Unix() != delegation.ExpiresAt {
		return nil, invalid
	}
	app, revision, err := trustedMCPApp(delegation.ClientID)
	if err != nil {
		return nil, err
	}
	if revision != delegation.Application {
		return nil, invalid
	}
	user, subject, expiry, err := verifyDelegatingUser(ctx, delegation.ClientID, delegation.AccessToken, app)
	if err != nil {
		return nil, err
	}
	if user.Id != delegation.UserID || subject != delegation.Subject || user.AuthVersion != delegation.AuthVersion {
		return nil, invalid
	}
	delegation.ServerIDs, delegation.MaxChargeQuota = app.ServerIDs, app.MaxChargeQuota
	if tokenExpiry := time.Unix(delegation.ExpiresAt, 0); tokenExpiry.Before(expiry) {
		expiry = tokenExpiry
	}
	if !expiry.After(time.Now()) {
		return nil, invalid
	}
	return &Actor{UserID: user.Id, Group: user.Group, Subject: subject, CanCall: true, ExpiresAt: expiry, Delegation: &delegation, DelegationToken: raw}, nil
}

func (delegation *MCPDelegation) AllowsServer(serverID string) bool {
	return slices.Contains(delegation.ServerIDs, serverID) || slices.Contains(delegation.ServerIDs, "*")
}
