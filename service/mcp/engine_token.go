package mcp

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"
)

type cachedEngineToken struct {
	token     string
	expires   time.Time
	refreshAt time.Time
}

// Engine JWTs are short-lived credentials, cached only in this process. Keys
// bind the user, engine application, source grant and relevant configuration.
type engineTokenCache struct {
	mu      sync.Mutex
	entries map[string]cachedEngineToken
	flights singleflight.Group
}

var knowledgeEngineTokens engineTokenCache

func (cache *engineTokenCache) load(key string) (cachedEngineToken, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry, ok := cache.entries[key]
	if !ok || !time.Now().Before(entry.refreshAt) {
		delete(cache.entries, key)
		return cachedEngineToken{}, false
	}
	return entry, true
}

func (cache *engineTokenCache) store(key string, entry cachedEngineToken) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.entries == nil {
		cache.entries = make(map[string]cachedEngineToken)
	}
	if len(cache.entries) >= 1024 {
		now := time.Now()
		var oldestKey string
		var oldest time.Time
		for candidate, value := range cache.entries {
			if !now.Before(value.refreshAt) {
				delete(cache.entries, candidate)
				continue
			}
			if oldestKey == "" || value.expires.Before(oldest) {
				oldestKey, oldest = candidate, value.expires
			}
		}
		if len(cache.entries) >= 1024 {
			delete(cache.entries, oldestKey)
		}
	}
	cache.entries[key] = entry
}

func (cache *engineTokenCache) invalidate(key, rejectedToken string) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.entries[key].token == rejectedToken {
		delete(cache.entries, key)
	}
}

func knowledgeEngineToken(ctx context.Context, userID int, subject, base, clientID, secret string, grant *oauth2.Token, timeout time.Duration) (cachedEngineToken, string, error) {
	if err := ctx.Err(); err != nil {
		return cachedEngineToken{}, "", err
	}
	// Public OAuth clients without an introspection secret retain the original
	// exchange path. Caching must not weaken immediate source-grant revocation.
	oauthSecret := os.Getenv("MCP_KNOWLEDGE_OAUTH_CLIENT_SECRET")
	if oauthSecret == "" {
		entry, err := exchangeKnowledgeEngineToken(ctx, base, clientID, secret, grant, timeout)
		return entry, "", err
	}
	key := Digest(strings.Join([]string{strconv.Itoa(userID), subject, base, clientID, secret, grant.AccessToken,
		os.Getenv("MCP_CASDOOR_ISSUER"), os.Getenv("MCP_CASDOOR_ORGANIZATION"), os.Getenv("MCP_CASDOOR_PROVIDER_ID"), os.Getenv("MCP_KNOWLEDGE_SCOPES"), oauthSecret, strconv.FormatInt(grant.Expiry.UnixNano(), 10)}, "\x00"))
	if entry, ok := knowledgeEngineTokens.load(key); ok {
		if err := verifyKnowledgeGrantActive(ctx, clientID, oauthSecret, subject, grant.AccessToken); err != nil {
			knowledgeEngineTokens.invalidate(key, entry.token)
			return cachedEngineToken{}, key, err
		}
		if time.Now().Before(entry.refreshAt) {
			return entry, key, nil
		}
	}
	result := knowledgeEngineTokens.flights.DoChan(key, func() (any, error) {
		// Waiters have independent cancellation. The shared exchange has its own
		// bounded lifetime and may populate the cache even if its first caller leaves.
		shared, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		defer cancel()
		if entry, ok := knowledgeEngineTokens.load(key); ok {
			if err := verifyKnowledgeGrantActive(shared, clientID, oauthSecret, subject, grant.AccessToken); err != nil {
				knowledgeEngineTokens.invalidate(key, entry.token)
				return cachedEngineToken{}, err
			}
			if time.Now().Before(entry.refreshAt) {
				return entry, nil
			}
		}
		entry, err := exchangeKnowledgeEngineToken(shared, base, clientID, secret, grant, timeout)
		if err == nil {
			knowledgeEngineTokens.store(key, entry)
		}
		return entry, err
	})
	select {
	case <-ctx.Done():
		return cachedEngineToken{}, key, ctx.Err()
	case response := <-result:
		if response.Err != nil {
			return cachedEngineToken{}, key, response.Err
		}
		return response.Val.(cachedEngineToken), key, nil
	}
}

func verifyKnowledgeGrantActive(ctx context.Context, clientID, secret, subject, raw string) error {
	issuer := strings.TrimRight(os.Getenv("MCP_CASDOOR_ISSUER"), "/")
	client, err := requestHTTPClient(ctx, issuer, 10*time.Second)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	form := url.Values{"token": {raw}, "token_type_hint": {"access_token"}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, issuer+"/api/login/oauth/introspect", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	request.SetBasicAuth(clientID, secret)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(request)
	if err != nil {
		return Failure(503, "AUTHORIZATION_UNAVAILABLE", "Knowledge authorization state cannot be verified")
	}
	defer response.Body.Close()
	var active struct {
		Active   bool             `json:"active"`
		Subject  string           `json:"sub"`
		ClientID string           `json:"client_id"`
		Issuer   string           `json:"iss"`
		Audience jwt.ClaimStrings `json:"aud"`
		Exp      int64            `json:"exp"`
	}
	if response.StatusCode != http.StatusOK || common.DecodeJson(response.Body, &active) != nil {
		return Failure(503, "AUTHORIZATION_UNAVAILABLE", "Knowledge authorization state cannot be verified")
	}
	if !active.Active || active.Subject != subject || active.ClientID != clientID || active.Issuer != issuer || len(active.Audience) != 1 || active.Audience[0] != clientID || active.Exp <= time.Now().Unix() {
		return Failure(401, "ENGINE_AUTHORIZATION_REQUIRED", "Authorize the knowledge application again")
	}
	return nil
}

func exchangeKnowledgeEngineToken(ctx context.Context, base, clientID, secret string, grant *oauth2.Token, timeout time.Duration) (cachedEngineToken, error) {
	client, err := requestHTTPClient(ctx, base, timeout)
	if err != nil {
		return cachedEngineToken{}, err
	}
	defer client.CloseIdleConnections()
	body, err := common.Marshal(map[string]string{"access_token": grant.AccessToken})
	if err != nil {
		return cachedEngineToken{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/auth/app-exchange", bytes.NewReader(body))
	if err != nil {
		return cachedEngineToken{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.SetBasicAuth(clientID, secret)
	started := time.Now()
	response, err := client.Do(request)
	if err != nil {
		return cachedEngineToken{}, Failure(503, "ENGINE_UNAVAILABLE", "Knowledge engine is temporarily unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		if response.StatusCode == 401 || response.StatusCode == 403 {
			var failure struct{ Detail string }
			_ = common.DecodeJson(response.Body, &failure)
			switch failure.Detail {
			case "Invalid application credentials", "Application authentication required", "Unknown application", "Application is not enabled", "Application is not configured", "Invalid application audience", "Delegated applications require an independent client ID":
				return cachedEngineToken{}, Failure(503, "ENGINE_APP_NOT_CONFIGURED", "Verify the trusted application registration and exchange credentials")
			}
			return cachedEngineToken{}, Failure(401, "ENGINE_AUTHORIZATION_REQUIRED", "Authorize the knowledge application again")
		}
		return cachedEngineToken{}, Failure(503, "ENGINE_UNAVAILABLE", "Knowledge engine is temporarily unavailable")
	}
	var token struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if common.DecodeJson(response.Body, &token) != nil || !strings.EqualFold(token.TokenType, "Bearer") || token.AccessToken == "" || token.ExpiresIn <= 0 || token.ExpiresIn > 3600 {
		return cachedEngineToken{}, Failure(502, "TOKEN_EXCHANGE_FAILED", "Invalid knowledge engine authorization response")
	}
	expires := started.Add(time.Duration(token.ExpiresIn) * time.Second)
	if grant.Expiry.Before(expires) {
		expires = grant.Expiry
	}
	if !expires.After(time.Now()) {
		return cachedEngineToken{}, Failure(401, "ENGINE_AUTHORIZATION_REQUIRED", "Knowledge authorization has expired")
	}
	margin := min(30*time.Second, expires.Sub(started)/10)
	return cachedEngineToken{token: token.AccessToken, expires: expires, refreshAt: expires.Add(-margin)}, nil
}
