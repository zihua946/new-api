package mcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func Install(serverID string, userID int, admin bool) (*model.MCPInstallation, error) {
	server, err := model.GetMCPServer(serverID)
	if err != nil {
		return nil, err
	}
	if !server.Catalog || !admin && !server.Enabled {
		return nil, gorm.ErrRecordNotFound
	}
	row := &model.MCPInstallation{ID: uuid.NewString(), UserID: userID, ServerID: serverID, Admin: admin, ConnectionRevision: server.ConnectionRevision, Authorized: admin && server.AuthMode == "none"}
	if err = model.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(row).Error; err != nil {
		return nil, err
	}
	return FindInstallation(serverID, userID, admin)
}

func FindInstallation(serverID string, userID int, admin bool) (*model.MCPInstallation, error) {
	var row model.MCPInstallation
	err := model.DB.Where("server_id = ? AND user_id = ? AND admin = ?", serverID, userID, admin).First(&row).Error
	return &row, err
}

func DisableInstallation(id string, userID int) error {
	return model.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.MCPInstallation{}).Where("id = ? AND user_id = ? AND admin = ?", id, userID, false).
			Updates(map[string]any{"authorized": false, "authorization_attempt": uuid.NewString()})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		return tx.Where("installation_id = ?", id).Delete(&model.MCPAuthorizationFlow{}).Error
	})
}

func DisconnectInstallation(id string, userID int) error {
	return model.DB.Transaction(func(tx *gorm.DB) error {
		var row model.MCPInstallation
		if err := tx.Where("id = ? AND user_id = ? AND admin = ?", id, userID, false).First(&row).Error; err != nil {
			return err
		}
		var ids []string
		if err := tx.Model(&model.MCPInstallation{}).Where("server_id = ? AND user_id = ?", row.ServerID, userID).Pluck("id", &ids).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.MCPInstallation{}).Where("id IN ?", ids).Updates(map[string]any{
			"authorized": false, "grant_ciphertext": "", "refresh_claim": "", "refresh_until": nil, "authorization_attempt": uuid.NewString(),
		}).Error; err != nil {
			return err
		}
		return tx.Where("installation_id IN ?", ids).Delete(&model.MCPAuthorizationFlow{}).Error
	})
}

// The management installation is the shared credential owner for this user and
// service. Personal installations retain their independent enable state.
func ReusableInstallationToken(ctx context.Context, server *model.MCPServer, userID int) (string, error) {
	shared, err := Install(server.ID, userID, true)
	if err != nil {
		return "", err
	}
	if shared.Grant == "" {
		var legacy model.MCPInstallation
		err := model.DB.Where("server_id = ? AND user_id = ? AND admin = ? AND grant_ciphertext <> ? AND connection_revision = ?", server.ID, userID, false, "", server.ConnectionRevision).First(&legacy).Error
		if err == nil {
			raw, err := Open(string(legacy.Grant), "oauth-grant:"+legacy.ID)
			var grant oauthGrant
			if err != nil || common.UnmarshalJsonStr(raw, &grant) != nil {
				return "", Failure(401, "REAUTHORIZATION_REQUIRED", "Authorize this MCP installation again")
			}
			if err := saveGrant(shared, server.ConnectionRevision, grant); err != nil {
				return "", err
			}
			shared, err = FindInstallation(server.ID, userID, true)
			if err != nil {
				return "", err
			}
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return "", err
		}
	}
	return installationGrantToken(ctx, server, shared)
}

type oauthDiscovery struct {
	Issuer          string   `json:"issuer"`
	AuthURL         string   `json:"authorization_endpoint"`
	TokenURL        string   `json:"token_endpoint"`
	Methods         []string `json:"token_endpoint_auth_methods_supported"`
	Challenges      []string `json:"code_challenge_methods_supported"`
	RegistrationURL string   `json:"registration_endpoint"`
	Scopes          []string `json:"scopes_supported"`
	ResponseIssuer  bool     `json:"authorization_response_iss_parameter_supported"`
}
type oauthFlow struct {
	Metadata oauthDiscovery
	Verifier string
	Resource string
	Revision int64
	Nonce    string
	AppID    string
}

func oauthMetadata(ctx context.Context, endpoint string, out any) error {
	client, err := NewHTTPClient(endpoint, 10*time.Second)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return errors.New("authorization metadata is unavailable")
	}
	return common.DecodeJson(io.LimitReader(resp.Body, 128<<10), out)
}

func discoverOAuth(ctx context.Context, endpoint string) (oauthDiscovery, error) {
	fail := Failure(502, "OAUTH_DISCOVERY_FAILED", "MCP OAuth metadata is unavailable or incompatible")
	var meta oauthDiscovery
	u, err := ValidateEndpoint(endpoint)
	if err != nil {
		return meta, err
	}
	origin := u.Scheme + "://" + u.Host
	resourceURL := origin + "/.well-known/oauth-protected-resource" + u.EscapedPath()
	client, err := NewHTTPClient(endpoint, 10*time.Second)
	if err != nil {
		return meta, err
	}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"new-api","version":"1"}}}`))
	if err != nil {
		return meta, err
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return meta, fail
	}
	resp.Body.Close()
	var scopes []string
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		challenges, err := oauthex.ParseWWWAuthenticate(resp.Header.Values("WWW-Authenticate"))
		if err != nil {
			return meta, fail
		}
		for _, challenge := range challenges {
			if challenge.Scheme != "bearer" {
				continue
			}
			if metadataURL := challenge.Params["resource_metadata"]; metadataURL != "" {
				parsed, err := ValidateEndpoint(metadataURL)
				if err != nil || !strings.EqualFold(parsed.Scheme+"://"+parsed.Host, origin) {
					return meta, fail
				}
				resourceURL = metadataURL
			}
			scopes = strings.Fields(challenge.Params["scope"])
			break
		}
	}
	var resource struct {
		Resource string   `json:"resource"`
		Issuers  []string `json:"authorization_servers"`
		Scopes   []string `json:"scopes_supported"`
	}
	if oauthMetadata(ctx, resourceURL, &resource) != nil {
		if resourceURL == origin+"/.well-known/oauth-protected-resource" || oauthMetadata(ctx, origin+"/.well-known/oauth-protected-resource", &resource) != nil {
			return meta, fail
		}
	}
	if resource.Resource != endpoint || len(resource.Issuers) != 1 {
		return meta, fail
	}
	if len(scopes) == 0 {
		scopes = resource.Scopes
	}
	if len(strings.Join(scopes, " ")) > 2048 {
		return meta, fail
	}
	for _, scope := range scopes {
		if scope == "" {
			return meta, fail
		}
		for _, c := range scope {
			// OAuth scope-token syntax from RFC 6749 section 3.3.
			if c < 0x21 || c > 0x7e || c == '"' || c == '\\' {
				return meta, fail
			}
		}
	}
	issuer, err := ValidateEndpoint(resource.Issuers[0])
	if err != nil {
		return meta, fail
	}
	issuerOrigin := issuer.Scheme + "://" + issuer.Host
	if oauthMetadata(ctx, issuerOrigin+"/.well-known/oauth-authorization-server"+strings.TrimRight(issuer.EscapedPath(), "/"), &meta) != nil {
		meta = oauthDiscovery{}
		if oauthMetadata(ctx, strings.TrimRight(resource.Issuers[0], "/")+"/.well-known/openid-configuration", &meta) != nil {
			return meta, fail
		}
	}
	if meta.Issuer != resource.Issuers[0] || !slices.Contains(meta.Challenges, "S256") {
		return meta, fail
	}
	for _, v := range []string{meta.AuthURL, meta.TokenURL} {
		if _, err := ValidateEndpoint(v); err != nil {
			return meta, fail
		}
	}
	// An authorization server's scopes may cover unrelated resources. Only
	// scopes advertised by this MCP resource belong in its authorization flow.
	meta.Scopes = scopes
	return meta, nil
}

func oauthClient(server *model.MCPServer, meta oauthDiscovery) (*oauth2.Config, error) {
	secret := ""
	if server.CredentialCiphertext != "" {
		var err error
		secret, err = Open(server.CredentialCiphertext, "oauth-client:"+server.ID)
		if err != nil {
			return nil, err
		}
	}
	if server.Kind == "deepseek" && server.Catalog {
		issuer := strings.TrimRight(os.Getenv("MCP_CASDOOR_ISSUER"), "/")
		if issuer == "" || issuer != meta.Issuer {
			return nil, Failure(503, "MCP_NOT_CONFIGURED", "Configure the trusted MCP OAuth issuer")
		}
		if server.Connection.ClientID == "" || secret == "" {
			return nil, Failure(401, "REAUTHORIZATION_REQUIRED", "Register and authorize this MCP client again")
		}
	}
	style := oauth2.AuthStyleInParams
	if secret != "" && (len(meta.Methods) == 0 || slices.Contains(meta.Methods, "client_secret_basic")) {
		style = oauth2.AuthStyleInHeader
	} else if secret != "" && !slices.Contains(meta.Methods, "client_secret_post") {
		return nil, Failure(422, "INVALID_OAUTH_CONFIG", "Unsupported OAuth client authentication method")
	}
	scopes := strings.Fields(server.Connection.Scopes)
	if len(scopes) == 0 {
		scopes = meta.Scopes
	}
	return &oauth2.Config{ClientID: server.Connection.ClientID, ClientSecret: secret, Endpoint: oauth2.Endpoint{AuthURL: meta.AuthURL, TokenURL: meta.TokenURL, AuthStyle: style}, Scopes: scopes, RedirectURL: PublicBaseURL() + "/api/mcp/oauth/callback"}, nil
}

func BeginAuthorization(ctx context.Context, row *model.MCPInstallation, sessionID string) (string, error) {
	server, err := model.GetMCPServer(row.ServerID)
	if err != nil {
		return "", err
	}
	if !server.Catalog || server.AuthMode != "oauth2" && server.Kind != "remote_mcp" && server.Kind != "public_knowledge" || sessionID == "" || !row.Admin && !server.Enabled {
		return "", Failure(403, "AUTHORIZATION_UNAVAILABLE", "OAuth authorization is unavailable")
	}
	if _, err := ValidateEndpoint(PublicBaseURL()); err != nil {
		return "", Failure(503, "MCP_NOT_CONFIGURED", "Configure the public MCP base URL")
	}
	var cfg *oauth2.Config
	flow := oauthFlow{Verifier: oauth2.GenerateVerifier(), Resource: server.Connection.URL, Revision: server.ConnectionRevision}
	opts := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(flow.Verifier)}
	if server.Kind == "public_knowledge" {
		cfg, flow.Metadata, err = applicationOAuth(ctx)
		if err != nil {
			return "", err
		}
		flow.Nonce, flow.AppID = rand.Text(), cfg.ClientID
		opts = append(opts, oidc.Nonce(flow.Nonce))
	} else {
		flow.Metadata, err = discoverOAuth(ctx, server.Connection.URL)
		if err != nil {
			return "", err
		}
		server, err = registeredOAuthClient(ctx, server, flow.Metadata)
		if err != nil {
			return "", err
		}
		flow.AppID = server.Connection.ClientID
		cfg, err = oauthClient(server, flow.Metadata)
		if err != nil {
			return "", err
		}
		opts = append(opts, oauth2.SetAuthURLParam("resource", flow.Resource))
	}
	state := rand.Text()
	id := Digest(state)
	encoded, err := common.Marshal(flow)
	if err != nil {
		return "", err
	}
	sealed, err := Seal(string(encoded), "oauth-flow:"+id)
	if err != nil {
		return "", err
	}
	if err = model.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(row).Where("COALESCE(authorization_attempt, '') = ?", row.AuthorizationAttempt).Update("authorization_attempt", id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return model.ErrMCPRevisionConflict
		}
		if err := tx.Where("installation_id = ?", row.ID).Delete(&model.MCPAuthorizationFlow{}).Error; err != nil {
			return err
		}
		return tx.Create(&model.MCPAuthorizationFlow{ID: id, InstallationID: row.ID, UserID: row.UserID, SessionHash: Digest(sessionID), Ciphertext: model.MCPEncryptedResult(sealed), ExpiresAt: time.Now().Add(10 * time.Minute)}).Error
	}); err != nil {
		return "", err
	}
	return cfg.AuthCodeURL(state, opts...), nil
}

type oauthGrant struct {
	Token    oauth2.Token
	Metadata oauthDiscovery
}

func FinishAuthorization(ctx context.Context, userID int, sessionID, state, code, issuer string) (bool, error) {
	fail := Failure(400, "INVALID_AUTHORIZATION_STATE", "Authorization expired or could not be verified")
	if sessionID == "" || state == "" || len(state) > 256 || len(code) > 4096 {
		return false, fail
	}
	var pending model.MCPAuthorizationFlow
	q := model.DB.Where("id = ? AND user_id = ? AND session_hash = ? AND expires_at > ?", Digest(state), userID, Digest(sessionID), time.Now())
	if q.First(&pending).Error != nil {
		return false, fail
	}
	deleted := q.Delete(&model.MCPAuthorizationFlow{})
	if deleted.Error != nil || deleted.RowsAffected != 1 {
		return false, fail
	}
	var row model.MCPInstallation
	if model.DB.Where("id = ? AND user_id = ?", pending.InstallationID, userID).First(&row).Error != nil {
		return false, fail
	}
	if code == "" {
		return row.Admin, Failure(400, "AUTHORIZATION_CANCELLED", "Authorization was cancelled")
	}
	server, err := model.GetMCPServer(row.ServerID)
	if err != nil {
		return row.Admin, err
	}
	var flow oauthFlow
	data, err := Open(string(pending.Ciphertext), "oauth-flow:"+pending.ID)
	if err != nil || common.UnmarshalJsonStr(data, &flow) != nil || flow.Revision != server.ConnectionRevision || flow.Resource != server.Connection.URL ||
		(flow.Metadata.ResponseIssuer || issuer != "") && issuer != flow.Metadata.Issuer || !row.Admin && !server.Enabled {
		return row.Admin, fail
	}
	var cfg *oauth2.Config
	opts := []oauth2.AuthCodeOption{oauth2.VerifierOption(flow.Verifier)}
	if server.Kind == "public_knowledge" {
		var current oauthDiscovery
		cfg, current, err = applicationOAuth(ctx)
		if err == nil && (flow.Nonce == "" || cfg.ClientID != flow.AppID || current.Issuer != flow.Metadata.Issuer || current.TokenURL != flow.Metadata.TokenURL) {
			return row.Admin, fail
		}
	} else {
		cfg, err = oauthClient(server, flow.Metadata)
		if err == nil && flow.AppID != "" && cfg.ClientID != flow.AppID {
			return row.Admin, fail
		}
		opts = append(opts, oauth2.SetAuthURLParam("resource", flow.Resource))
	}
	if err != nil {
		return row.Admin, err
	}
	client, err := NewHTTPClient(flow.Metadata.TokenURL, 15*time.Second)
	if err != nil {
		return row.Admin, err
	}
	defer client.CloseIdleConnections()
	token, err := cfg.Exchange(context.WithValue(ctx, oauth2.HTTPClient, client), code, opts...)
	if err != nil || !token.Valid() || !strings.EqualFold(token.Type(), "Bearer") {
		return row.Admin, Failure(401, "AUTHORIZATION_FAILED", "OAuth authorization failed")
	}
	if server.Kind == "public_knowledge" {
		raw, _ := token.Extra("id_token").(string)
		provider, httpClient, err := oauthProvider(ctx)
		if err != nil {
			return row.Admin, err
		}
		defer httpClient.CloseIdleConnections()
		identity, err := provider.Verifier(&oidc.Config{ClientID: cfg.ClientID, SupportedSigningAlgs: []string{"RS256"}}).Verify(oidc.ClientContext(ctx, httpClient), raw)
		if err != nil || identity.Nonce != flow.Nonce || len(identity.Audience) != 1 {
			return row.Admin, fail
		}
		access, err := verifiedAppIdentity(ctx, userID, token.AccessToken)
		if err != nil {
			return row.Admin, err
		}
		if access.Subject != identity.Subject {
			return row.Admin, fail
		}
		return row.Admin, model.DB.Transaction(func(tx *gorm.DB) error {
			result := tx.Model(&row).Where("authorization_attempt = ?", pending.ID).Updates(map[string]any{"authorized": true, "connection_revision": server.ConnectionRevision})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return fail
			}
			return writeEngineGrant(tx, userID, *token, "", access)
		})
	}
	if server.Kind != "remote_mcp" {
		actor, err := AuthenticateOAuth(ctx, server, token.AccessToken)
		if err != nil || actor.UserID != userID {
			return row.Admin, Failure(403, "IDENTITY_MISMATCH", "Authorize with your New API account")
		}
	}
	row.AuthorizationAttempt = pending.ID
	return row.Admin, saveGrant(&row, server.ConnectionRevision, oauthGrant{Token: *token, Metadata: flow.Metadata})
}

func saveGrant(row *model.MCPInstallation, revision int64, grant oauthGrant) error {
	shared, err := Install(row.ServerID, row.UserID, true)
	if err != nil {
		return err
	}
	encoded, err := common.Marshal(grant)
	if err != nil {
		return err
	}
	sealed, err := Seal(string(encoded), "oauth-grant:"+shared.ID)
	if err != nil {
		return err
	}
	return model.DB.Transaction(func(tx *gorm.DB) error {
		// Serialize writes to the shared credential, including refresh completion.
		if err := tx.Model(&model.MCPInstallation{}).Where("id = ?", shared.ID).
			UpdateColumn("refresh_claim", gorm.Expr("refresh_claim")).Error; err != nil {
			return err
		}
		result := tx.Model(&model.MCPInstallation{}).Where("id = ? AND grant_ciphertext = ? AND refresh_claim = ? AND COALESCE(authorization_attempt, '') = ?", row.ID, row.Grant, row.RefreshClaim, row.AuthorizationAttempt).
			Updates(map[string]any{"authorized": true, "connection_revision": revision, "authorization_attempt": uuid.NewString()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return model.ErrMCPRevisionConflict
		}
		if err := tx.Model(&model.MCPInstallation{}).Where("id = ?", shared.ID).Updates(map[string]any{
			"grant_ciphertext": sealed, "authorized": true, "connection_revision": revision, "refresh_claim": "", "refresh_until": nil,
		}).Error; err != nil {
			return err
		}
		return tx.Model(&model.MCPInstallation{}).Where("server_id = ? AND user_id = ? AND id <> ?", row.ServerID, row.UserID, shared.ID).
			Updates(map[string]any{"grant_ciphertext": "", "refresh_claim": "", "refresh_until": nil}).Error
	})
}

func InstallationToken(ctx context.Context, server *model.MCPServer, row *model.MCPInstallation) (string, error) {
	if !row.Admin && !row.Authorized {
		return "", Failure(403, "INSTALLATION_DISCONNECTED", "Enable this MCP installation before use")
	}
	return ReusableInstallationToken(ctx, server, row.UserID)
}

func installationGrantToken(ctx context.Context, server *model.MCPServer, row *model.MCPInstallation) (string, error) {
	if server.AuthMode == "none" {
		return "", nil
	}
	fail := Failure(401, "REAUTHORIZATION_REQUIRED", "Authorize this MCP installation again")
	if !row.Authorized || row.ConnectionRevision != server.ConnectionRevision || row.Grant == "" {
		return "", fail
	}
	raw, err := Open(string(row.Grant), "oauth-grant:"+row.ID)
	var grant oauthGrant
	if err != nil || common.UnmarshalJsonStr(raw, &grant) != nil {
		return "", fail
	}
	if grant.Token.Valid() {
		return grant.Token.AccessToken, nil
	}
	if grant.Token.RefreshToken == "" {
		return "", fail
	}
	claim := uuid.NewString()
	claimed := model.DB.Model(row).Where("grant_ciphertext = ? AND (refresh_until IS NULL OR refresh_until < ?)", row.Grant, time.Now()).Updates(map[string]any{"refresh_claim": claim, "refresh_until": time.Now().Add(time.Minute)})
	if claimed.Error != nil {
		return "", claimed.Error
	}
	if claimed.RowsAffected != 1 {
		return "", Failure(409, "AUTHORIZATION_REFRESH_BUSY", "Authorization is being refreshed")
	}
	row.RefreshClaim = claim
	defer model.DB.Model(row).Where("refresh_claim = ?", claim).Updates(map[string]any{"refresh_claim": "", "refresh_until": nil})
	cfg, err := oauthClient(server, grant.Metadata)
	if err != nil {
		return "", err
	}
	// Force the refresh and retain the resource on the refresh grant.
	client, err := NewHTTPClient(grant.Metadata.TokenURL, 15*time.Second)
	if err != nil {
		return "", err
	}
	defer client.CloseIdleConnections()
	client.Transport = &oauthRefreshTransport{base: client.Transport, resource: server.Connection.URL}
	refreshed, err := cfg.TokenSource(context.WithValue(ctx, oauth2.HTTPClient, client), &grant.Token).Token()
	if err != nil {
		var retrieve *oauth2.RetrieveError
		if errors.As(err, &retrieve) && retrieve.ErrorCode == "invalid_grant" {
			model.DB.Model(row).Where("refresh_claim = ?", claim).Updates(map[string]any{"authorized": false, "grant_ciphertext": ""})
			return "", fail
		}
		return "", Failure(503, "AUTHORIZATION_UNAVAILABLE", "OAuth refresh is temporarily unavailable")
	}
	if !refreshed.Valid() || !strings.EqualFold(refreshed.Type(), "Bearer") {
		return "", fail
	}
	grant.Token = *refreshed
	if err = saveGrant(row, server.ConnectionRevision, grant); err != nil {
		return "", err
	}
	return refreshed.AccessToken, nil
}

// oauthRefreshTransport adds the protected resource only to the pinned token request.
type oauthRefreshTransport struct {
	base     http.RoundTripper
	resource string
}

func (transport *oauthRefreshTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodPost || r.Body == nil {
		return nil, errors.New("invalid OAuth refresh request")
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 128<<10))
	r.Body.Close()
	if err != nil {
		return nil, err
	}
	form, err := url.ParseQuery(string(raw))
	if err != nil || form.Get("grant_type") != "refresh_token" {
		return nil, errors.New("invalid OAuth refresh form")
	}
	form.Set("resource", transport.resource)
	body := form.Encode()
	request := r.Clone(r.Context())
	request.Body = io.NopCloser(strings.NewReader(body))
	request.ContentLength = int64(len(body))
	return transport.base.RoundTrip(request)
}

func (transport *oauthRefreshTransport) CloseIdleConnections() {
	if base, ok := transport.base.(interface{ CloseIdleConnections() }); ok {
		base.CloseIdleConnections()
	}
}
