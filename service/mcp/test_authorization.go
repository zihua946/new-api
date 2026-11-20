package mcp

import (
	"context"
	"crypto/rand"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"golang.org/x/oauth2"
)

func testOAuthConfig(ctx context.Context) (*oauth2.Config, *http.Client, error) {
	provider, client, err := oauthProvider(ctx)
	if err != nil {
		return nil, nil, err
	}
	id, secret := os.Getenv("MCP_OAUTH_CLIENT_ID"), os.Getenv("MCP_OAUTH_CLIENT_SECRET")
	if id == "" || secret == "" || PublicBaseURL() == "" {
		return nil, nil, Failure(503, "MCP_NOT_CONFIGURED", "MCP test OAuth client is not configured")
	}
	return &oauth2.Config{ClientID: id, ClientSecret: secret, Endpoint: provider.Endpoint(), RedirectURL: PublicBaseURL() + "/api/mcp/test-authorizations/callback", Scopes: []string{"mcp:tools:read", "mcp:tools:call"}}, client, nil
}

func StartTestAuthorization(ctx context.Context, serverID string, userID int, sessionID string) (map[string]any, error) {
	if sessionID == "" {
		return nil, Failure(401, "BROWSER_SESSION_REQUIRED", "A signed-in browser session is required")
	}
	server, err := model.GetMCPServer(serverID)
	if err != nil {
		return nil, err
	}
	if server.AuthMode != "casdoor_oauth" {
		return nil, Failure(422, "OAUTH_NOT_REQUIRED", "This server does not use OAuth")
	}
	config, client, err := testOAuthConfig(ctx)
	if err != nil {
		return nil, err
	}
	defer client.CloseIdleConnections()
	state, verifier := rand.Text(), oauth2.GenerateVerifier()
	stateHash := Digest(state)
	sealed, err := Seal(verifier, "test-flow:"+stateHash)
	if err != nil {
		return nil, err
	}
	auth := model.MCPTestAuthorization{ID: Digest(state + "\x00" + strconv.Itoa(userID)), ServerID: serverID, UserID: userID, SessionHash: Digest(sessionID), StateHash: stateHash, VerifierCiphertext: sealed, ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}
	if err = model.DB.Create(&auth).Error; err != nil {
		return nil, err
	}
	authorizationURL := config.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("resource", ResourceURL(server)))
	return map[string]any{"authorization_url": authorizationURL, "expires_at": auth.ExpiresAt}, nil
}

func FinishTestAuthorization(ctx context.Context, userID int, sessionID, state, code string) (string, error) {
	if sessionID == "" || state == "" || code == "" || len(state) > 256 || len(code) > 4096 {
		return "", Failure(400, "INVALID_AUTHORIZATION_CALLBACK", "Authorization callback is invalid")
	}
	var auth model.MCPTestAuthorization
	if model.DB.Where("state_hash = ? AND user_id = ? AND session_hash = ? AND consumed = ? AND expires_at > ?", Digest(state), userID, Digest(sessionID), false, time.Now().UTC()).First(&auth).Error != nil {
		return "", Failure(400, "INVALID_AUTHORIZATION_STATE", "Authorization state expired or was already used")
	}
	result := model.DB.Model(&model.MCPTestAuthorization{}).Where("id = ? AND consumed = ? AND expires_at > ?", auth.ID, false, time.Now().UTC()).Update("consumed", true)
	if result.Error != nil {
		return "", result.Error
	}
	if result.RowsAffected != 1 {
		return "", Failure(400, "INVALID_AUTHORIZATION_STATE", "Authorization state expired or was already used")
	}
	server, err := model.GetMCPServer(auth.ServerID)
	if err != nil {
		return "", err
	}
	verifier, err := Open(auth.VerifierCiphertext, "test-flow:"+auth.StateHash)
	if err != nil {
		return "", err
	}
	config, client, err := testOAuthConfig(ctx)
	if err != nil {
		return "", err
	}
	defer client.CloseIdleConnections()
	token, err := config.Exchange(context.WithValue(ctx, oauth2.HTTPClient, client), code, oauth2.VerifierOption(verifier), oauth2.SetAuthURLParam("resource", ResourceURL(server)))
	if err != nil {
		return "", Failure(401, "AUTHORIZATION_FAILED", "The authorization code could not be exchanged")
	}
	actor, err := AuthenticateOAuth(ctx, server, token.AccessToken)
	if err != nil {
		return "", err
	}
	if actor.UserID != userID {
		return "", Failure(403, "TEST_IDENTITY_MISMATCH", "Authorize with the same account as the signed-in administrator")
	}
	sealed, err := Seal(token.AccessToken, "test-token:"+auth.ID)
	if err != nil {
		return "", err
	}
	if err = model.DB.Model(&auth).Updates(map[string]any{"token_ciphertext": sealed, "verifier_ciphertext": "", "subject": actor.Subject, "expires_at": actor.ExpiresAt}).Error; err != nil {
		return "", err
	}
	return server.ID, nil
}

func GetTestAuthorization(serverID string, userID int, sessionID string) (*model.MCPTestAuthorization, error) {
	if sessionID == "" {
		return nil, Failure(401, "BROWSER_SESSION_REQUIRED", "A signed-in browser session is required")
	}
	var auth model.MCPTestAuthorization
	err := model.DB.Where("server_id = ? AND user_id = ? AND session_hash = ? AND consumed = ? AND token_ciphertext <> ? AND expires_at > ?", serverID, userID, Digest(sessionID), true, "", time.Now().UTC()).Order("created_at DESC").First(&auth).Error
	return &auth, err
}

func TestActor(ctx context.Context, server *model.MCPServer, userID int, sessionID string) (*Actor, error) {
	if !server.Catalog && server.AuthMode == "casdoor_oauth" {
		auth, err := GetTestAuthorization(server.ID, userID, sessionID)
		if err != nil {
			return nil, Failure(401, "TEST_AUTHORIZATION_REQUIRED", "Authorize this service with your Casdoor account before testing")
		}
		token, err := Open(auth.TokenCiphertext, "test-token:"+auth.ID)
		if err != nil {
			return nil, err
		}
		return AuthenticateOAuth(ctx, server, token)
	}
	user, err := model.GetUserById(userID, false)
	if err != nil {
		return nil, err
	}
	if user.Status != common.UserStatusEnabled {
		return nil, Failure(403, "USER_DISABLED", "User is unavailable")
	}
	return &Actor{UserID: userID, Group: user.Group, CanCall: true}, nil
}
