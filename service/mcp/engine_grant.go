package mcp

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func engineGrantID(userID int) string {
	return Digest(strings.Join([]string{ConfigValue("casdoor_issuer"), ConfigValue("knowledge_client_id"), strconv.Itoa(userID)}, "\x00"))
}

// HasEngineAuthorization reports locally usable credentials for the catalog.
// Discovery and execution verify the signed identity and current engine access.
func HasEngineAuthorization(userID int) (bool, error) {
	var grant model.MCPEngineGrant
	result := model.DB.Select("id", "token").Where("id = ? AND user_id = ?", engineGrantID(userID), userID).Limit(1).Find(&grant)
	if result.Error != nil || result.RowsAffected == 0 {
		return false, result.Error
	}
	raw, err := Open(string(grant.Token), "engine-grant:"+grant.ID)
	if err != nil {
		return false, err
	}
	var token oauth2.Token
	if err = common.UnmarshalJsonStr(raw, &token); err != nil {
		return false, err
	}
	return token.Valid() || token.RefreshToken != "", nil
}

func applicationOAuth(ctx context.Context) (*oauth2.Config, oauthDiscovery, error) {
	var meta oauthDiscovery
	cfgErr := Failure(503, "ENGINE_APP_NOT_CONFIGURED", "Configure the trusted knowledge application")
	issuer := strings.TrimRight(ConfigValue("casdoor_issuer", ctx), "/")
	id := ConfigValue("knowledge_client_id", ctx)
	if id == "" || issuer == "" || ConfigValue("knowledge_client_secret", ctx) == "" {
		return nil, meta, cfgErr
	}
	if err := oauthMetadata(ctx, issuer+"/.well-known/openid-configuration", &meta); err != nil {
		return nil, meta, Failure(503, "AUTHORIZATION_UNAVAILABLE", "Casdoor discovery is unavailable")
	}
	if meta.Issuer != issuer || (len(meta.Challenges) > 0 && !slices.Contains(meta.Challenges, "S256")) {
		return nil, meta, cfgErr
	}
	for _, endpoint := range []string{meta.AuthURL, meta.TokenURL} {
		if _, err := ValidateEndpoint(endpoint); err != nil {
			return nil, meta, cfgErr
		}
	}
	scope := ConfigValue("knowledge_scopes", ctx)
	if scope == "" {
		scope = "openid system.login"
	}
	if !slices.Contains(strings.Fields(scope), "openid") {
		return nil, meta, cfgErr
	}
	secret := ConfigValue("knowledge_oauth_client_secret", ctx)
	style := oauth2.AuthStyleInHeader
	if secret == "" {
		style = oauth2.AuthStyleInParams
	}
	return &oauth2.Config{ClientID: id, ClientSecret: secret, RedirectURL: PublicBaseURL(ctx) + "/api/mcp/oauth/callback", Scopes: strings.Fields(scope), Endpoint: oauth2.Endpoint{AuthURL: meta.AuthURL, TokenURL: meta.TokenURL, AuthStyle: style}}, meta, nil
}

func verifiedAppIdentity(ctx context.Context, userID int, raw string) (*oidc.IDToken, error) {
	provider, client, err := oauthProvider(ctx)
	if err != nil {
		return nil, err
	}
	defer client.CloseIdleConnections()
	id := ConfigValue("knowledge_client_id", ctx)
	if id == "" {
		return nil, Failure(503, "ENGINE_APP_NOT_CONFIGURED", "Configure the trusted knowledge application")
	}
	claims, err := provider.Verifier(&oidc.Config{ClientID: id, SupportedSigningAlgs: []string{"RS256"}}).Verify(oidc.ClientContext(ctx, client), raw)
	var extra struct {
		Owner string `json:"owner"`
		Party string `json:"azp"`
	}
	if err != nil || len(claims.Audience) != 1 || claims.Audience[0] != id || claims.Subject == "" || claims.Claims(&extra) != nil || extra.Owner == "" || extra.Owner != ConfigValue("casdoor_organization", ctx) || extra.Party != "" && extra.Party != id {
		return nil, Failure(401, "ENGINE_AUTHORIZATION_REQUIRED", "Reconnect the trusted knowledge application")
	}
	user, err := casdoorUser(claims.Subject, ctx)
	if err != nil {
		return nil, err
	}
	if user.Id != userID {
		return nil, Failure(403, "IDENTITY_MISMATCH", "Connect the same Casdoor identity as your platform account")
	}
	return claims, nil
}

func storeEngineGrant(ctx context.Context, userID int, token oauth2.Token) error {
	return persistEngineGrant(ctx, userID, token, "")
}
func persistEngineGrant(ctx context.Context, userID int, token oauth2.Token, claim string) error {
	identity, err := verifiedAppIdentity(ctx, userID, token.AccessToken)
	if err != nil {
		return err
	}
	return writeEngineGrant(model.DB, userID, token, claim, identity)
}
func writeEngineGrant(db *gorm.DB, userID int, token oauth2.Token, claim string, identity *oidc.IDToken) error {
	if token.Expiry.IsZero() || identity.Expiry.Before(token.Expiry) {
		token.Expiry = identity.Expiry
	}
	data, err := common.Marshal(token)
	if err != nil {
		return err
	}
	id := engineGrantID(userID)
	cipher, err := Seal(string(data), "engine-grant:"+id)
	if err != nil {
		return err
	}
	row := model.MCPEngineGrant{ID: id, UserID: userID, Subject: identity.Subject, Token: model.MCPEncryptedResult(cipher)}
	if claim != "" {
		res := db.Model(&row).Where("refresh_claim = ?", claim).Updates(map[string]any{"token": cipher, "refresh_claim": "", "refresh_until": nil})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return model.ErrMCPRevisionConflict
		}
		return nil
	}
	return db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoUpdates: clause.AssignmentColumns([]string{"subject", "token", "refresh_claim", "refresh_until"})}).Create(&row).Error
}

func loadEngineGrant(ctx context.Context, userID int) (*oauth2.Token, string, error) {
	required := Failure(401, "ENGINE_AUTHORIZATION_REQUIRED", "Connect your Casdoor account to the knowledge application")
	var row model.MCPEngineGrant
	err := model.DB.Where("id = ? AND user_id = ?", engineGrantID(userID), userID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, "", required
	}
	if err != nil {
		return nil, "", err
	}
	raw, err := Open(string(row.Token), "engine-grant:"+row.ID)
	if err != nil {
		return nil, "", err
	}
	var token oauth2.Token
	if common.UnmarshalJsonStr(raw, &token) != nil {
		return nil, "", required
	}
	if !token.Valid() {
		if token.RefreshToken == "" {
			return nil, "", required
		}
		cfg, _, err := applicationOAuth(ctx)
		if err != nil {
			return nil, "", err
		}
		claim := uuid.NewString()
		result := model.DB.Model(&row).Where("token = ? AND (refresh_until IS NULL OR refresh_until < ?)", row.Token, time.Now()).Updates(map[string]any{"refresh_claim": claim, "refresh_until": time.Now().Add(time.Minute)})
		if result.Error != nil {
			return nil, "", result.Error
		}
		if result.RowsAffected != 1 {
			return nil, "", Failure(409, "AUTHORIZATION_REFRESH_BUSY", "Authorization is being refreshed")
		}
		defer model.DB.Model(&row).Where("refresh_claim = ?", claim).Updates(map[string]any{"refresh_claim": "", "refresh_until": nil})
		client, err := requestHTTPClient(ctx, cfg.Endpoint.TokenURL, 15*time.Second)
		if err != nil {
			return nil, "", err
		}
		defer client.CloseIdleConnections()
		fresh, err := cfg.TokenSource(context.WithValue(ctx, oauth2.HTTPClient, client), &token).Token()
		if err != nil {
			var retrieve *oauth2.RetrieveError
			if errors.As(err, &retrieve) && retrieve.ErrorCode == "invalid_grant" {
				model.DB.Where("id = ? AND refresh_claim = ?", row.ID, claim).Delete(&model.MCPEngineGrant{})
				return nil, "", required
			}
			return nil, "", Failure(503, "AUTHORIZATION_UNAVAILABLE", "Knowledge application authorization is temporarily unavailable")
		}
		if !fresh.Valid() || !strings.EqualFold(fresh.Type(), "Bearer") {
			return nil, "", required
		}
		if err = persistEngineGrant(ctx, userID, *fresh, claim); err != nil {
			return nil, "", err
		}
		token = *fresh
	}
	identity, err := verifiedAppIdentity(ctx, userID, token.AccessToken)
	if err != nil {
		return nil, "", err
	}
	if identity.Subject != row.Subject {
		return nil, "", required
	}
	if token.Expiry.IsZero() || identity.Expiry.Before(token.Expiry) {
		token.Expiry = identity.Expiry
	}
	return &token, identity.Subject, nil
}
