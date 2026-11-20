package mcp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"fmt"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/golang-jwt/jwt/v5"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	"gorm.io/gorm/logger"
)

func TestMCPOAuthCryptographicAndLiveAuthorization(t *testing.T) {
	setupMCPService(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	var active atomic.Bool
	active.Store(true)
	var issuer string
	introspectionPatch := map[string]any{}
	resource := "https://gateway.test/mcp/knowledge"
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var response any
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			response = map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}}
		case "/keys":
			response = map[string]any{"keys": []any{map[string]any{"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "test-key", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}}
		case "/api/login/oauth/introspect":
			id, secret, ok := r.BasicAuth()
			assert.True(t, ok)
			assert.Equal(t, "resource-verifier", id)
			assert.Equal(t, "verifier-secret", secret)
			assert.NoError(t, r.ParseForm())
			assert.Equal(t, resource, r.Form.Get("resource"))
			claims := jwt.MapClaims{}
			_, err := jwt.ParseWithClaims(r.Form.Get("token"), claims, func(*jwt.Token) (any, error) { return &key.PublicKey, nil }, jwt.WithValidMethods([]string{"RS256"}))
			if err != nil {
				http.Error(w, "invalid token", 401)
				return
			}
			status := map[string]any{"active": active.Load(), "iss": issuer, "aud": claims["aud"], "sub": claims["sub"], "client_id": claims["azp"], "exp": claims["exp"], "scope": claims["scope"]}
			maps.Copy(status, introspectionPatch)
			response = status
		default:
			http.NotFound(w, r)
			return
		}
		encoded, err := common.Marshal(response)
		assert.NoError(t, err)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(encoded)
	}))
	t.Cleanup(provider.Close)
	issuer = provider.URL
	t.Setenv("MCP_ALLOWED_UPSTREAM_ORIGINS", issuer)
	t.Setenv("MCP_CASDOOR_ISSUER", issuer)
	t.Setenv("MCP_CASDOOR_ORGANIZATION", "knowyet")
	t.Setenv("MCP_CASDOOR_PROVIDER_ID", "1")
	t.Setenv("MCP_OAUTH_CLIENT_ID", "resource-verifier")
	t.Setenv("MCP_OAUTH_CLIENT_SECRET", "verifier-secret")
	t.Setenv("MCP_OAUTH_ALLOWED_CLIENT_IDS", "resource-verifier, connector-client")
	require.NoError(t, model.DB.Create(&model.User{Id: 100, Username: "alice", Group: "default", Status: common.UserStatusEnabled}).Error)
	require.NoError(t, model.DB.Create(&model.CustomOAuthProvider{Id: 1, Name: "Casdoor", Slug: "casdoor", Enabled: true, UserIdField: "sub", WellKnown: issuer + "/.well-known/openid-configuration"}).Error)
	require.NoError(t, model.DB.Create(&model.UserOAuthBinding{UserId: 100, ProviderId: 1, ProviderUserId: "stable-alice"}).Error)
	server := &model.MCPServer{Slug: "knowledge", AllowedUserGroups: model.MCPGroups{"default"}}
	base := jwt.MapClaims{"iss": issuer, "sub": "stable-alice", "aud": []string{resource}, "azp": "connector-client", "owner": "knowyet", "scope": "mcp:tools:read mcp:tools:call", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	forged := jwt.NewWithClaims(jwt.SigningMethodRS256, base)
	forged.Header["kid"] = "test-key"
	forgedToken, err := forged.SignedString(otherKey)
	require.NoError(t, err)
	_, err = AuthenticateOAuth(context.Background(), server, forgedToken)
	require.Error(t, err)
	for _, tc := range []struct {
		name     string
		patch    jwt.MapClaims
		valid    bool
		callable bool
	}{
		{"unregistered-client", jwt.MapClaims{"azp": "another-casdoor-app"}, false, false},
		{"valid", nil, true, true}, {"read-only", jwt.MapClaims{"scope": "mcp:tools:read"}, true, false},
		{"audience", jwt.MapClaims{"aud": "other-resource"}, false, false}, {"issuer", jwt.MapClaims{"iss": "https://other.test"}, false, false},
		{"organization", jwt.MapClaims{"owner": "other"}, false, false}, {"scope", jwt.MapClaims{"scope": "openid"}, false, false},
		{"unbound", jwt.MapClaims{"sub": "other-user"}, false, false}, {"expired", jwt.MapClaims{"exp": time.Now().Add(-time.Hour).Unix()}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := maps.Clone(base)
			maps.Copy(claims, tc.patch)
			token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
			token.Header["kid"] = "test-key"
			raw, err := token.SignedString(key)
			require.NoError(t, err)
			actor, err := AuthenticateOAuth(context.Background(), server, raw)
			if !tc.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, 100, actor.UserID)
			assert.Equal(t, tc.callable, actor.CanCall)
			for _, mismatch := range []map[string]any{
				{"iss": "https://other.test"}, {"aud": "another-resource"},
				{"aud": nil}, {"client_id": "another-client"}, {"sub": "another-user"},
			} {
				introspectionPatch = mismatch
				_, err = AuthenticateOAuth(context.Background(), server, raw)
				require.Error(t, err)
			}
			introspectionPatch = map[string]any{"aud": resource}
			_, err = AuthenticateOAuth(context.Background(), server, raw)
			require.NoError(t, err, "standard introspection permits string audience")
			introspectionPatch = map[string]any{}
			t.Setenv("MCP_OAUTH_ALLOWED_CLIENT_IDS", "")
			_, err = AuthenticateOAuth(context.Background(), server, raw)
			require.Error(t, err, "unconfigured external clients must not be allowed by default")
			t.Setenv("MCP_OAUTH_ALLOWED_CLIENT_IDS", "resource-verifier, connector-client")
			active.Store(false)
			_, err = AuthenticateOAuth(context.Background(), server, raw)
			require.Error(t, err)
			assert.Equal(t, "AUTHORIZATION_REVOKED", ErrorResponse(err).Code)
			active.Store(true)
		})
	}
	t.Run("public library creation and personal result use the trusted application", func(t *testing.T) {
		ctx := t.Context()
		var public atomic.Bool
		public.Store(true)
		var chats, streams atomic.Int32
		var appToken string
		engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/auth/app-exchange":
				id, secret, ok := r.BasicAuth()
				assert.True(t, ok)
				assert.Equal(t, "business", id)
				assert.Equal(t, "engine-exchange", secret)
				var body map[string]string
				assert.NoError(t, common.DecodeJson(r.Body, &body))
				assert.Equal(t, map[string]string{"access_token": appToken}, body)
				fmt.Fprint(w, `{"token_type":"bearer","access_token":"engine-user","expires_in":3540}`)
			case "/v1/knowledge/kb/manual":
				assert.Equal(t, "library-owner", r.URL.Query().Get("owner"))
				assert.Equal(t, "Bearer engine-user", r.Header.Get("Authorization"))
				fmt.Fprintf(w, `{"id":"manual","owner":"library-owner","public_read":%t}`, public.Load())
			case "/v1/knowledge/kb/manual/chat":
				assert.Equal(t, "POST", r.Method)
				assert.Equal(t, "library-owner", r.URL.Query().Get("owner"))
				assert.NotEmpty(t, r.Header.Get("Idempotency-Key"))
				chats.Add(1)
				fmt.Fprint(w, `{"task_id":"task-one","session_id":"session-one"}`)
			case "/v1/tasks/task-one/stream":
				streams.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, `data: {"type":"connected","task_id":"task-one","status":"running"}

data: {"type":"context","messages":["private"]}

data: {"type":"text_delta",
data: "content":"answer"}

data: {"type":"completed"}

`)
			default:
				t.Errorf("unexpected engine path %s", r.URL.Path)
				http.NotFound(w, r)
			}
		}))
		defer engine.Close()
		t.Setenv("MCP_KNOWLEDGE_ENGINE_URL", engine.URL)
		t.Setenv("MCP_KNOWLEDGE_CLIENT_ID", "business")
		t.Setenv("MCP_KNOWLEDGE_CLIENT_SECRET", "engine-exchange")
		t.Setenv("MCP_KNOWLEDGE_OAUTH_CLIENT_SECRET", "casdoor-secret")
		t.Setenv("MCP_ALLOWED_UPSTREAM_ORIGINS", issuer+","+engine.URL)
		var config ServerInput
		require.NoError(t, common.UnmarshalJsonStr(`{"name":"Manual","icon":"OpenAI","readme":"Library guide","connection":{"transport":"streamable_http","owner":"library-owner","kb_id":"manual"}}`, &config))
		server, err := CreateServer(config)
		require.NoError(t, err)
		appClaims := maps.Clone(base)
		appClaims["aud"] = "business"
		appClaims["azp"] = "business"
		appClaims["scope"] = "openid system.login"
		sign := func(claims jwt.MapClaims) string {
			tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
			tok.Header["kid"] = "test-key"
			value, err := tok.SignedString(key)
			require.NoError(t, err)
			return value
		}
		appToken = sign(appClaims)
		require.NoError(t, storeEngineGrant(ctx, 100, oauth2.Token{AccessToken: appToken, Expiry: time.Now().Add(time.Hour)}))
		other := maps.Clone(appClaims)
		other["sub"] = "stable-bob"
		require.Error(t, storeEngineGrant(ctx, 100, oauth2.Token{AccessToken: sign(other)}))
		other = maps.Clone(appClaims)
		other["aud"] = ResourceURL(server)
		require.Error(t, storeEngineGrant(ctx, 100, oauth2.Token{AccessToken: sign(other)}))
		discovered, err := DiscoverTools(ctx, server.ID, 100)
		require.NoError(t, err)
		require.NoError(t, SyncTools(server.ID, discovered["discovery_id"].(string), 100, server.Revision))
		tools, err := ListTools(server.ID)
		require.NoError(t, err)
		require.Len(t, tools, 2)
		on := true
		for _, tool := range tools {
			require.NoError(t, PatchTool(server.ID, tool.ID, ToolPatch{Enabled: &on, ExpectedRevision: tool.Revision}, true))
		}
		server, err = SetServerStatusForUser(ctx, server.ID, server.Revision+1, true, 100)
		require.NoError(t, err)
		t.Run("disconnect isolates personal knowledge installations", func(t *testing.T) {
			second, err := CreateServer(config)
			require.NoError(t, err)
			require.NoError(t, model.DB.Model(second).Update("enabled", true).Error)
			second.Enabled = true
			identity := &Actor{UserID: 100, CanCall: true}
			first, err := Install(server.ID, 100, false)
			require.NoError(t, err)
			other, err := Install(second.ID, 100, false)
			require.NoError(t, err)
			_, err = InstallationActor(ctx, server, identity, false)
			require.Error(t, err)
			require.NoError(t, model.DB.Model(&model.MCPInstallation{}).Where("id IN ?", []string{first.ID, other.ID}).Update("authorized", true).Error)
			before, err := InstallationActor(ctx, server, identity, false)
			require.NoError(t, err)
			require.Error(t, DisconnectInstallation(first.ID, 101))
			require.NoError(t, DisconnectInstallation(first.ID, 100))
			_, err = publicLibraryClient(ctx, server, before)
			require.Error(t, err)
			_, err = InstallationActor(ctx, server, identity, false)
			require.Error(t, err)
			_, err = InstallationActor(ctx, second, identity, false)
			require.NoError(t, err)
			shared, err := HasEngineAuthorization(100)
			require.NoError(t, err)
			assert.True(t, shared)
		})
		actor := &Actor{UserID: 100, Group: "default", CanCall: true}
		args := map[string]any{"request_id": "one", "question": "How?"}
		for _, key := range []string{"owner", "kb_id", "knowledge_base"} {
			bad := maps.Clone(args)
			bad[key] = "different"
			_, err = PrepareCall(server.ID, "create_consultation", actor, bad, InvocationOptions{})
			require.Error(t, err)
		}
		for range 2 {
			call, err := PrepareCall(server.ID, "create_consultation", actor, args, InvocationOptions{})
			require.NoError(t, err)
			_, err = call.Run(ctx, nil)
			require.NoError(t, err)
		}
		assert.EqualValues(t, 1, chats.Load())
		call, err := PrepareCall(server.ID, "get_consultation_result", actor, map[string]any{"task_id": "task-one"}, InvocationOptions{})
		require.NoError(t, err)
		result, err := call.Run(ctx, nil)
		require.NoError(t, err)
		assert.Contains(t, fmt.Sprint(result.StructuredContent), "answer")
		assert.NotContains(t, fmt.Sprint(result.StructuredContent), "private")
		require.EqualValues(t, 1, streams.Load())
		public.Store(false)
		_, err = AuthorizedInvocationResult(ctx, call.Invocation, actor, false)
		require.Error(t, err)
		assert.EqualValues(t, 1, streams.Load())
	})
	t.Run("local provider shares pricing and idempotency with catalog", func(t *testing.T) {
		ctx := context.Background()
		resource = "https://gateway.test/mcp/providers/deepseek"
		claims := maps.Clone(base)
		claims["aud"] = []string{resource}
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		token.Header["kid"] = "test-key"
		raw, err := token.SignedString(key)
		require.NoError(t, err)
		var dispatched atomic.Int32
		fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/v1/tools/search", r.URL.Path)
			assert.Equal(t, "Bearer supplier-key", r.Header.Get("Authorization"))
			dispatched.Add(1)
			fmt.Fprint(w, `{"results":[{"title":"fixture","url":"https://example.com"}]}`)
		}))
		defer fixture.Close()
		t.Setenv("MCP_ALLOWED_UPSTREAM_ORIGINS", issuer+","+fixture.URL)
		t.Setenv("MCP_DEEPSEEK_CHANNEL_ID", "1234")
		t.Setenv("MCP_DEEPSEEK_BASE_URL", fixture.URL)
		require.NoError(t, model.DB.Create(&model.Channel{Id: 1234, Name: "local-provider", Type: constant.ChannelTypeDeepSeek, Status: common.ChannelStatusEnabled, Key: "supplier-key", BaseURL: &fixture.URL}).Error)
		require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", 100).Update("quota", 2000).Error)
		input := ServerInput{Icon: "OpenAI", Readme: "Fixture README", Name: "Search", AuthMode: "oauth2", Connection: model.MCPConnection{URL: resource, Transport: "streamable_http", ClientID: "connector-client"}}
		server, err := CreateServer(input)
		require.NoError(t, err)
		_, err = CreateServer(input)
		require.Error(t, err)
		discovery, err := DiscoverTools(ctx, server.ID, 100)
		require.NoError(t, err)
		require.NoError(t, SyncTools(server.ID, discovery["discovery_id"].(string), 100, server.Revision))
		views, err := ListTools(server.ID)
		require.NoError(t, err)
		index := slices.IndexFunc(views, func(view ToolView) bool { return view.Name == "web_search" })
		require.NotEqual(t, -1, index)
		tool := views[index]
		_, err = SetToolPrice(server.ID, tool.ID, tool.Revision, "per_success", "0.001")
		require.NoError(t, err)
		enabled := true
		require.NoError(t, PatchTool(server.ID, tool.ID, ToolPatch{ExpectedRevision: tool.Revision + 1, Enabled: &enabled}, true))
		server, err = SetServerStatus(server.ID, server.Revision+1, true)
		require.NoError(t, err)
		direct, err := AuthenticateOAuth(ctx, server, raw)
		require.NoError(t, err)
		row, err := Install(server.ID, 100, false)
		require.NoError(t, err)
		require.NoError(t, saveGrant(row, server.ConnectionRevision, oauthGrant{Token: oauth2.Token{AccessToken: raw, Expiry: time.Now().Add(time.Hour)}}))
		proxy, err := InstallationActor(ctx, server, &Actor{UserID: 100}, false)
		require.NoError(t, err)
		for _, actor := range []*Actor{direct, proxy} {
			call, err := PrepareCall(server.ID, tool.Name, actor, map[string]any{"query": "fixture"}, InvocationOptions{IdempotencyKey: "same-logical-call"})
			require.NoError(t, err)
			_, err = call.Run(ctx, nil)
			require.NoError(t, err)
			assert.Equal(t, "settled", call.Invocation.BillingStatus)
		}
		assert.EqualValues(t, 1, dispatched.Load())
		var user model.User
		require.NoError(t, model.DB.First(&user, 100).Error)
		assert.Equal(t, 1500, user.Quota)
	})

}

func setupMCPService(t *testing.T) {
	t.Helper()
	priorDB, priorLog, priorPath := model.DB, model.LOG_DB, common.SQLitePath
	priorMainType, priorLogType := common.MainDatabaseType(), common.LogDatabaseType()
	priorMaster, priorRedis, priorMemory, priorBatch, priorUnit := common.IsMasterNode, common.RedisEnabled, common.MemoryCacheEnabled, common.BatchUpdateEnabled, common.QuotaPerUnit
	testDSN := os.Getenv("MCP_SERVICE_TEST_SQL_DSN")
	if testDSN != "" {
		require.True(t, strings.HasPrefix(testDSN, "root@tcp(127.0.0.1:23306)/mcp_service_test?") || strings.HasPrefix(testDSN, "postgres://postgres:mcp-local-test@127.0.0.1:25432/mcp_service_test?"), "only the dedicated local MCP service test database is allowed")
	}
	t.Setenv("SQL_DSN", testDSN)
	t.Setenv("LOG_SQL_DSN", "")
	t.Setenv("MCP_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("MCP_PUBLIC_BASE_URL", "https://gateway.test")
	common.SQLitePath = filepath.Join(t.TempDir(), "mcp-service.db")
	common.IsMasterNode, common.RedisEnabled, common.MemoryCacheEnabled, common.BatchUpdateEnabled, common.QuotaPerUnit = true, false, false, false, 500000
	require.NoError(t, model.InitDB())
	require.NoError(t, model.InitLogDB())
	model.DB.Logger = logger.Default.LogMode(logger.Silent)
	db, err := model.DB.DB()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
		model.DB, model.LOG_DB, common.SQLitePath = priorDB, priorLog, priorPath
		common.SetMainDatabaseType(priorMainType)
		common.SetLogDatabaseType(priorLogType)
		common.IsMasterNode, common.RedisEnabled, common.MemoryCacheEnabled, common.BatchUpdateEnabled, common.QuotaPerUnit = priorMaster, priorRedis, priorMemory, priorBatch, priorUnit
	})
}

func TestMCPDeepSeekExecutionAndKeyIsolation(t *testing.T) {
	setupMCPService(t)
	var requests atomic.Int32
	var empty atomic.Bool
	var expectedKey atomic.Value
	expectedKey.Store("provider-key-one")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/tools/search", r.URL.Path)
		assert.Equal(t, "Bearer "+expectedKey.Load().(string), r.Header.Get("Authorization"))
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if empty.Load() {
			_, _ = w.Write([]byte(`{"results":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"title":"Result","url":"https://example.com"}]}`))
	}))
	t.Cleanup(upstream.Close)
	t.Setenv("MCP_ALLOWED_UPSTREAM_ORIGINS", upstream.URL)
	t.Setenv("MCP_CASDOOR_ISSUER", "")
	t.Setenv("MCP_CASDOOR_PROVIDER_ID", "")
	user := model.User{Id: 100, Username: "mcp-user", Group: "default", Status: common.UserStatusEnabled, Quota: 1000}
	key := model.Token{Id: 200, UserId: user.Id, Key: "ordinary-user-key", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 1000}
	channel := model.Channel{Id: 12, Name: "DeepSeek", Type: constant.ChannelTypeDeepSeek, Key: "provider-key-one", Status: common.ChannelStatusEnabled, BaseURL: &upstream.URL}
	require.NoError(t, model.DB.Create(&user).Error)
	require.NoError(t, model.DB.Create(&key).Error)
	require.NoError(t, model.DB.Create(&channel).Error)
	server, err := CreateServer(ServerInput{Slug: "search", Name: "Search", Kind: "deepseek", AllowedUserGroups: model.MCPGroups{"default"}, Connection: model.MCPConnection{ChannelID: 12}})
	require.NoError(t, err)
	assert.False(t, server.Enabled)
	assert.Equal(t, "api_key", server.AuthMode)
	actor, err := Authenticate(context.Background(), server, "Bearer sk-ordinary-user-key", "127.0.0.1")
	require.NoError(t, err)
	assert.Equal(t, user.Id, actor.UserID)
	assert.Equal(t, key.Id, actor.TokenID)
	// The member owns the API token and the team pays for successful calls.
	teamPayer := model.User{Username: "mcp-team-payer", AffCode: "mcp-team-payer", Quota: 5000, Status: common.UserStatusEnabled}
	require.NoError(t, model.DB.Create(&teamPayer).Error)
	unit := model.BillingUnit{Name: "MCP team", OwnerUserId: user.Id, PayerUserId: teamPayer.Id}
	require.NoError(t, model.DB.Create(&unit).Error)
	require.NoError(t, model.DB.Create(&model.BillingUnitMember{UserId: user.Id, BillingUnitId: unit.Id}).Error)
	tools, err := ListTools(server.ID)
	require.NoError(t, err)
	var search ToolView
	for _, tool := range tools {
		if tool.Name == "web_search" {
			search = tool
		}
	}
	require.NotEmpty(t, search.ID)
	_, err = SetToolPrice(server.ID, search.ID, search.Revision, "per_success", "0.001")
	require.NoError(t, err)
	emptyGroups := model.MCPGroups{}
	enabled := true
	require.NoError(t, PatchTool(server.ID, search.ID, ToolPatch{ExpectedRevision: search.Revision + 1, Enabled: &enabled, AllowedUserGroups: &emptyGroups}, true))
	_, err = SetServerStatus(server.ID, server.Revision, true)
	require.NoError(t, err)
	options := InvocationOptions{IdempotencyKey: "task-one:call-one"}
	args := map[string]any{"query": "knowledge", "count": float64(5)}
	t.Run("quota saturation is audited before dispatch", func(t *testing.T) {
		quotaPerUnit := common.QuotaPerUnit
		common.QuotaPerUnit = 1e16
		t.Cleanup(func() { common.QuotaPerUnit = quotaPerUnit })
		_, err := PrepareCall(server.ID, search.Name, actor, args, InvocationOptions{RequestID: "quota-saturation-request", IdempotencyKey: "oversized"})
		require.Error(t, err)
		assert.Equal(t, "INVALID_PRICE", ErrorResponse(err).Code)
		var entry model.Log
		require.NoError(t, model.LOG_DB.Where("request_id = ? AND type = ?", "quota-saturation-request", model.LogTypeError).First(&entry).Error)
		var metadata struct {
			AdminInfo struct {
				QuotaSaturation map[string]any `json:"quota_saturation"`
			} `json:"admin_info"`
		}
		require.NoError(t, common.UnmarshalJsonStr(entry.Other, &metadata))
		assert.NotEmpty(t, metadata.AdminInfo.QuotaSaturation)
		assert.Equal(t, user.Id, entry.UserId)
		assert.Zero(t, entry.Quota)
		assert.Zero(t, requests.Load())
	})
	call, err := PrepareCall(server.ID, search.Name, actor, args, options)
	require.NoError(t, err)
	result, err := call.Run(context.Background(), nil)
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.EqualValues(t, 1, requests.Load())
	var updated model.User
	require.NoError(t, model.DB.First(&updated, user.Id).Error)
	assert.Equal(t, 1000, updated.Quota)
	assert.Zero(t, updated.UsedQuota)
	require.NoError(t, model.DB.First(&key, key.Id).Error)
	assert.Equal(t, 500, key.RemainQuota)
	assert.Equal(t, 500, key.UsedQuota)
	assert.Equal(t, teamPayer.Id, call.Invocation.PayerUserID)
	assert.Equal(t, unit.Id, call.Invocation.BillingUnitID)
	require.NoError(t, model.DB.First(&teamPayer, teamPayer.Id).Error)
	assert.Equal(t, 4500, teamPayer.Quota)
	assert.Equal(t, 1, updated.RequestCount)
	replay, err := PrepareCall(server.ID, search.Name, actor, args, options)
	require.NoError(t, err)
	_, err = replay.Run(context.Background(), nil)
	require.NoError(t, err)
	assert.EqualValues(t, 1, requests.Load())
	_, err = PrepareCall(server.ID, search.Name, actor, map[string]any{"query": "different"}, options)
	assert.ErrorIs(t, err, model.ErrMCPIdempotencyConflict)
	empty.Store(true)
	expectedKey.Store("provider-key-two")
	require.NoError(t, model.DB.Model(&channel).Update("key", "provider-key-two").Error)
	call, err = PrepareCall(server.ID, search.Name, actor, args, InvocationOptions{IdempotencyKey: "empty"})
	require.NoError(t, err)
	_, err = call.Run(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, model.DB.First(&updated, user.Id).Error)
	require.NoError(t, model.DB.First(&teamPayer, teamPayer.Id).Error)
	assert.Equal(t, 4000, teamPayer.Quota, "a valid empty result is charged once")
	assert.EqualValues(t, 2, requests.Load())
	require.NoError(t, model.DB.Model(&key).Update("remain_quota", 400).Error)
	call, err = PrepareCall(server.ID, search.Name, actor, args, InvocationOptions{IdempotencyKey: "token-limit"})
	require.NoError(t, err)
	_, err = call.Run(context.Background(), nil)
	require.Error(t, err)
	assert.Equal(t, "INSUFFICIENT_CREDITS", ErrorResponse(err).Code)
	assert.EqualValues(t, 2, requests.Load(), "an exhausted token cannot spend its owner's remaining wallet")
	require.NoError(t, model.DB.First(&updated, user.Id).Error)
	assert.Equal(t, 1000, updated.Quota)
	require.NoError(t, model.DB.Model(&key).Update("remain_quota", 500).Error)
	require.NoError(t, model.DB.Model(&teamPayer).Update("quota", 100).Error)
	call, err = PrepareCall(server.ID, search.Name, actor, args, InvocationOptions{IdempotencyKey: "no-funds"})
	require.NoError(t, err)
	_, err = call.Run(context.Background(), nil)
	require.Error(t, err)
	assert.Equal(t, "INSUFFICIENT_CREDITS", ErrorResponse(err).Code)
	assert.EqualValues(t, 2, requests.Load())
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current, err := model.GetMCPServer(server.ID)
		if err != nil {
			http.Error(w, "configuration unavailable", 503)
			return
		}
		identity, err := Authenticate(r.Context(), current, r.Header.Get("Authorization"), "127.0.0.1")
		if err != nil {
			http.Error(w, ErrorResponse(err).Code, ErrorResponse(err).Status)
			return
		}
		if err = ServeProtocol(w, r, current, identity); err != nil {
			http.Error(w, ErrorResponse(err).Code, ErrorResponse(err).Status)
		}
	}))
	t.Cleanup(endpoint.Close)
	client := &http.Client{Timeout: 5 * time.Second, Transport: &credentialTransport{base: http.DefaultTransport, headers: map[string]string{"Authorization": "Bearer sk-ordinary-user-key", "Idempotency-Key": "sdk-call"}}}
	session, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "contract-test", Version: "1"}, nil).Connect(context.Background(), &mcpsdk.StreamableClientTransport{Endpoint: endpoint.URL, HTTPClient: client, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close(); client.CloseIdleConnections() })
	directory, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, directory.Tools, 1)
	assert.Equal(t, "web_search", directory.Tools[0].Name)
	denied, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "web_search", Arguments: args})
	require.NoError(t, err)
	assert.True(t, denied.IsError)
	assert.Contains(t, fmt.Sprint(denied.Meta), "INSUFFICIENT_CREDITS")
	assert.EqualValues(t, 2, requests.Load())
	require.NoError(t, model.DB.Model(&channel).Update("base_url", "https://changed.example").Error)
	_, _, err = DeepSeekConnection(server)
	require.Error(t, err)
	assert.Equal(t, "UPSTREAM_ADDRESS_CHANGED", ErrorResponse(err).Code)
	require.NoError(t, model.DB.First(&channel, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusEnabled, channel.Status)
	assert.Zero(t, channel.UsedQuota)
	assert.Zero(t, channel.TestTime)
	assert.NotContains(t, fmt.Sprint(result), "provider-key")
}

func TestMCPDeepSeekConnectionFailureSettlement(t *testing.T) {
	setupMCPService(t)
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if assert.NoError(t, err) {
			assert.NoError(t, conn.Close())
		}
	}))
	t.Cleanup(upstream.Close)
	t.Setenv("MCP_ALLOWED_UPSTREAM_ORIGINS", upstream.URL)
	user := model.User{Username: "mcp-network-user", AffCode: "mcp-network", Group: "default", Status: common.UserStatusEnabled, Quota: 1000}
	require.NoError(t, model.DB.Create(&user).Error)
	token := model.Token{UserId: user.Id, Key: "mcp-network-key", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 1000}
	require.NoError(t, model.DB.Create(&token).Error)
	actor := &Actor{UserID: user.Id, TokenID: token.Id, Group: "default", CanCall: true}
	for _, tc := range []struct {
		name, endpoint, code, status, billing string
		quota, requests                       int
	}{
		{"blocked", "https://198.18.0.1", "UPSTREAM_ADDRESS_BLOCKED", "failed", "refunded", 1000, 0},
		{"disconnected", upstream.URL, "UPSTREAM_RESULT_UNKNOWN", "unknown", "reserved", 500, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channel := model.Channel{Name: tc.name, Type: constant.ChannelTypeDeepSeek, Key: "provider-network-key", Status: common.ChannelStatusEnabled, BaseURL: &tc.endpoint}
			require.NoError(t, model.DB.Create(&channel).Error)
			server, err := CreateServer(ServerInput{Slug: "network-" + tc.name, Name: tc.name, Kind: "deepseek", AllowedUserGroups: model.MCPGroups{"default"}, Connection: model.MCPConnection{ChannelID: channel.Id}})
			require.NoError(t, err)
			tools, err := ListTools(server.ID)
			require.NoError(t, err)
			var search ToolView
			for _, tool := range tools {
				if tool.Name == "web_search" {
					search = tool
				}
			}
			require.NotEmpty(t, search.ID)
			_, err = SetToolPrice(server.ID, search.ID, search.Revision, "per_success", "0.001")
			require.NoError(t, err)
			maxCharge := 500
			call, err := PrepareCall(server.ID, search.Name, actor, map[string]any{"query": "network test"}, InvocationOptions{Test: true, IdempotencyKey: tc.name, ExpectedToolRevision: search.Revision + 1, MaxChargeQuota: &maxCharge})
			require.NoError(t, err)
			_, err = call.Run(context.Background(), nil)
			require.Error(t, err)
			assert.Equal(t, tc.code, ErrorResponse(err).Code)
			require.NoError(t, model.DB.First(call.Invocation, "id = ?", call.Invocation.ID).Error)
			assert.Equal(t, tc.status, call.Invocation.Status)
			assert.Equal(t, tc.billing, call.Invocation.BillingStatus)
			require.NoError(t, model.DB.First(&user, user.Id).Error)
			require.NoError(t, model.DB.First(&token, token.Id).Error)
			assert.Equal(t, tc.quota, user.Quota)
			assert.Equal(t, tc.quota, token.RemainQuota)
			assert.EqualValues(t, tc.requests, requests.Load())
		})
	}
}

func TestMCPReadOnlyScopeRejectsToolCalls(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/mcp/knowledge", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_consultation"}}`))
	err := ServeProtocol(httptest.NewRecorder(), request, &model.MCPServer{}, &Actor{CanCall: false})
	require.Error(t, err)
	assert.Equal(t, 403, ErrorResponse(err).Status)
	assert.Equal(t, "INSUFFICIENT_SCOPE", ErrorResponse(err).Code)
}

func TestMCPPriceQuote(t *testing.T) {
	previous := common.QuotaPerUnit
	common.QuotaPerUnit = 500000
	t.Cleanup(func() { common.QuotaPerUnit = previous })
	rate := operation_setting.USDExchangeRate
	operation_setting.USDExchangeRate = 7.2
	t.Cleanup(func() { operation_setting.USDExchangeRate = rate })
	for _, item := range []struct{ price, currency string }{{"0.01", "USD"}, {"0.072", "CNY"}} {
		quote, err := QuotePrice("per_success", item.price, item.currency)
		require.NoError(t, err)
		assert.Equal(t, 5000, quote.ChargeQuota)
		assert.Equal(t, item.currency, quote.Currency)
	}
	for _, tc := range []struct {
		mode, price string
		quota       int
		valid       bool
	}{
		{"free", "0", 0, true}, {"per_success", "0.001", 500, true},
		{"per_success", "0.000003", 2, true}, {"per_success", "0.0000001", 0, false},
		{"per_success", "0", 0, true}, {"free", "0.001", 0, false},
		{"per_success", "-1", 0, false}, {"per_success", "NaN", 0, false},
		{"per_success", "1e999999", 0, false}, {"per_success", "9999999999", 0, false},
	} {
		t.Run(tc.mode+"/"+tc.price, func(t *testing.T) {
			quote, err := QuotePrice(tc.mode, tc.price)
			if !tc.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.quota, quote.ChargeQuota)
		})
	}
}

func TestMCPKnowledgeHTTPReuse(t *testing.T) {
	setupMCPService(t)
	var mu sync.Mutex
	var keys []string
	denied := false
	denyAfterEvents := false
	status := "completed"
	execution := "execution-one"
	answer := strings.Repeat("甲乙丙", 7000)
	events := []map[string]any{{"type": "thinking_delta", "content": "private reasoning"}, {"type": "text_delta", "content": answer}, {"type": "completed"}}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		var value any
		switch r.URL.Path {
		case "/v1/auth/mcp-exchange":
			id, secret, ok := r.BasicAuth()
			assert.True(t, ok)
			assert.Equal(t, "engine-service", id)
			assert.Equal(t, "service-secret", secret)
			value = map[string]any{"access_token": "engine-jwt", "token_type": "Bearer", "expires_in": 300}
		default:
			assert.Equal(t, "Bearer engine-jwt", r.Header.Get("Authorization"))
			if denied {
				w.WriteHeader(403)
				return
			}
			switch r.URL.Path {
			case "/v1/knowledge/kb":
				value = []map[string]any{{"id": "知识", "owner": "engine-alice", "name": "知识", "can_read": true}, {"id": "other", "owner": "engine-bob", "name": "Other", "can_read": true}}
			case "/v1/knowledge/kb/知识", "/v1/knowledge/kb/other":
				value = map[string]any{"can_read": true}
			case "/v1/knowledge/kb/知识/chat":
				assert.Equal(t, "engine-alice", r.URL.Query().Get("owner"))
				var payload map[string]any
				assert.NoError(t, common.DecodeJson(r.Body, &payload))
				assert.Equal(t, map[string]any{"question": "Question", "session_id": nil}, payload)
				keys = append(keys, r.Header.Get("Idempotency-Key"))
				value = map[string]any{"task_id": "task-one", "session_id": "session-one", "engine_type": "internal"}
			case "/v1/tasks/task-one":
				value = map[string]any{"id": "task-one", "session_id": "session-one", "status": status, "execution_progress": map[string]any{"execution_id": execution}, "raw_decl": "private prompt"}
			case "/v1/tasks/task-one/events":
				value = map[string]any{"task_id": "task-one", "events": events}
				if denyAfterEvents {
					denied = true
				}
			case "/v1/tasks/task-one/cancel":
				status = "cancelling"
				value = map[string]any{"task_id": "task-one", "status": status, "cancel_requested": true}
			default:
				t.Errorf("unexpected engine API: %s", r.URL.Path)
				http.NotFound(w, r)
				return
			}
		}
		encoded, err := common.Marshal(value)
		assert.NoError(t, err)
		_, _ = w.Write(encoded)
	}))
	t.Cleanup(upstream.Close)
	t.Setenv("MCP_ALLOWED_UPSTREAM_ORIGINS", upstream.URL)
	sealed, err := Seal(`{"client_secret":"service-secret"}`, "server:knowledge-http")
	require.NoError(t, err)
	server := &model.MCPServer{ID: "knowledge-http", Slug: "knowledge", Kind: "knowledge", Connection: model.MCPConnection{EngineBaseURL: upstream.URL, ClientID: "engine-service"}, CredentialCiphertext: sealed, Limits: model.MCPLimits{TimeoutSeconds: 30}}
	actor := &Actor{UserID: 100, AccessToken: "casdoor-parent"}
	invoke := func(name string, arguments map[string]any) (*mcpsdk.CallToolResult, error) {
		args, err := ValidateBuiltinArguments(name, arguments)
		if err != nil {
			return nil, err
		}
		outcome, err := executeKnowledge(context.Background(), server, &model.MCPTool{Name: name}, actor, args, nil)
		return outcome.Result, err
	}
	create := map[string]any{"knowledge_base": map[string]any{"kb_id": "知识", "owner": "engine-alice"}, "question": "Question", "request_id": "same-request"}
	for range 2 {
		result, err := invoke("create_consultation", create)
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"task_id": "task-one", "session_id": "session-one", "status": "completed"}, result.StructuredContent)
	}
	require.Len(t, keys, 2)
	assert.NotEmpty(t, keys[0])
	assert.Equal(t, keys[0], keys[1])
	var count int64
	require.NoError(t, model.DB.Model(&model.MCPTaskBinding{}).Where("server_id = ?", server.ID).Count(&count).Error)
	assert.EqualValues(t, 1, count)
	for _, patch := range []map[string]any{{"session_id": "foreign"}, {"session_id": "session-one", "knowledge_base": map[string]any{"kb_id": "other", "owner": "engine-alice"}}} {
		args := maps.Clone(create)
		maps.Copy(args, patch)
		_, err := invoke("create_consultation", args)
		require.Error(t, err)
		assert.Equal(t, "RESOURCE_NOT_FOUND", ErrorResponse(err).Code)
	}
	listed, err := invoke("list_knowledge_bases", map[string]any{"limit": float64(1)})
	require.NoError(t, err)
	listCursor := listed.StructuredContent.(map[string]any)["next_cursor"].(string)
	listed, err = invoke("list_knowledge_bases", map[string]any{"limit": float64(1), "cursor": listCursor})
	require.NoError(t, err)
	assert.Nil(t, listed.StructuredContent.(map[string]any)["next_cursor"])
	cursor, actual := "", ""
	for page := range 5 {
		result, err := invoke("get_consultation_result", map[string]any{"task_id": "task-one", "limit": float64(1), "wait_seconds": float64(0), "cursor": cursor})
		require.NoError(t, err)
		data := result.StructuredContent.(map[string]any)
		for _, event := range data["events"].([]map[string]any) {
			if event["type"] == "answer_delta" {
				actual += event["text"].(string)
			}
		}
		cursor = data["next_cursor"].(string)
		encoded, err := common.Marshal(data)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), "private")
		assert.Equal(t, true, data["task_finished"])
		if data["done"] == true {
			break
		}
		require.Less(t, page, 4, "all content must be readable")
	}
	assert.Equal(t, answer, actual)
	// Reusing the same cursor after constructing a new client replays the same
	// public page; no process-local cursor or SSE counter is needed.
	_, err = invoke("get_consultation_result", map[string]any{"task_id": "task-one", "cursor": cursor, "wait_seconds": float64(0)})
	require.NoError(t, err)
	_, err = invoke("get_consultation_result", map[string]any{"task_id": "task-one", "cursor": cursor + "corrupt", "wait_seconds": float64(0)})
	require.Error(t, err)
	assert.Equal(t, "CURSOR_EXPIRED", ErrorResponse(err).Code)
	associated := fmt.Sprintf("knowledge-events:%s:%d:%s", server.ID, actor.UserID, "task-one")
	plain, err := Open(cursor, associated)
	require.NoError(t, err)
	var expired knowledgeEventCursor
	require.NoError(t, common.UnmarshalJsonStr(plain, &expired))
	expired.Expires = time.Now().Unix() - 1
	encoded, err := common.Marshal(expired)
	require.NoError(t, err)
	expiredCursor, err := Seal(string(encoded), associated)
	require.NoError(t, err)
	_, err = invoke("get_consultation_result", map[string]any{"task_id": "task-one", "cursor": expiredCursor, "wait_seconds": float64(0)})
	require.Error(t, err)
	assert.Equal(t, "CURSOR_EXPIRED", ErrorResponse(err).Code)
	for _, identity := range []struct {
		user   int
		server string
	}{{101, server.ID}, {actor.UserID, "another-server"}} {
		_, err := Open(cursor, fmt.Sprintf("knowledge-events:%s:%d:task-one", identity.server, identity.user))
		require.Error(t, err)
	}
	mu.Lock()
	status = "running"
	mu.Unlock()
	cancel, err := invoke("cancel_consultation", map[string]any{"task_id": "task-one"})
	require.NoError(t, err)
	assert.Equal(t, "cancelling", cancel.StructuredContent.(map[string]any)["status"])
	mu.Lock()
	status = "completed"
	mu.Unlock()
	for _, other := range []Actor{{UserID: 101, AccessToken: "other"}} {
		original := actor
		actor = &other
		_, err := invoke("get_consultation_result", map[string]any{"task_id": "task-one", "cursor": cursor})
		require.Error(t, err)
		assert.Equal(t, "RESOURCE_NOT_FOUND", ErrorResponse(err).Code)
		actor = original
	}
	originalID := server.ID
	server.ID = "other-server"
	_, err = invoke("cancel_consultation", map[string]any{"task_id": "task-one"})
	require.Error(t, err)
	assert.Equal(t, "RESOURCE_NOT_FOUND", ErrorResponse(err).Code)
	server.ID = originalID
	for _, changed := range []string{"execution", "prefix"} {
		mu.Lock()
		if changed == "execution" {
			execution = "execution-two"
		} else {
			execution = "execution-one"
			events[1]["content"] = "replaced"
		}
		mu.Unlock()
		_, err = invoke("get_consultation_result", map[string]any{"task_id": "task-one", "cursor": cursor, "wait_seconds": float64(0)})
		require.Error(t, err)
		assert.Equal(t, "CURSOR_EXPIRED", ErrorResponse(err).Code)
	}
	mu.Lock()
	events[1]["content"] = answer
	denyAfterEvents = true
	mu.Unlock()
	_, err = invoke("get_consultation_result", map[string]any{"task_id": "task-one", "cursor": cursor})
	require.Error(t, err)
	assert.Equal(t, "RESOURCE_NOT_FOUND", ErrorResponse(err).Code)
}

func TestMCPCredentialsAreBoundToRecord(t *testing.T) {
	t.Setenv("MCP_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	sealed, err := Seal("secret-value", "server-one")
	require.NoError(t, err)
	assert.NotContains(t, sealed, "secret-value")
	plain, err := Open(sealed, "server-one")
	require.NoError(t, err)
	assert.Equal(t, "secret-value", plain)
	_, err = Open(sealed, "server-two")
	assert.Error(t, err)
	t.Setenv("MCP_ENCRYPTION_KEY", "")
	_, err = Seal("secret-value", "server-one")
	assert.Error(t, err)
}

func TestMCPLocalDeploymentOAuthClient(t *testing.T) {
	t.Setenv("MCP_PUBLIC_BASE_URL", "https://gateway.test")
	t.Setenv("MCP_CASDOOR_ISSUER", "https://issuer.test")
	t.Setenv("MCP_OAUTH_CLIENT_ID", "local-client")
	t.Setenv("MCP_OAUTH_CLIENT_SECRET", "fixture-secret")
	server := &model.MCPServer{Kind: "deepseek", Catalog: true, Connection: model.MCPConnection{URL: "https://gateway.test/mcp/providers/deepseek", ClientID: "local-client"}}
	cfg, err := oauthClient(server, oauthDiscovery{Issuer: "https://issuer.test"})
	require.NoError(t, err)
	assert.Equal(t, "fixture-secret", cfg.ClientSecret)
	_, err = oauthClient(server, oauthDiscovery{Issuer: "https://other.test"})
	require.Error(t, err)
	server.Kind = "remote_mcp"
	cfg, err = oauthClient(server, oauthDiscovery{Issuer: "https://other.test"})
	require.NoError(t, err)
	assert.Empty(t, cfg.ClientSecret)
}

func TestMCPRetiredKnowledgeProvider(t *testing.T) {
	t.Setenv("MCP_PUBLIC_BASE_URL", "https://gateway.test")
	assert.Empty(t, BuiltinTools("knowledge"))
	assert.Len(t, BuiltinTools("public_knowledge"), 2)
	assert.Len(t, BuiltinTools("deepseek"), 2)
	assert.Empty(t, LocalProvider("https://gateway.test/mcp/providers/knowledge"))
	_, err := CreateServer(ServerInput{Kind: "knowledge", Slug: "knowledge", Name: "Legacy"})
	require.Error(t, err)
	assert.Equal(t, "INVALID_PROVIDER", ErrorResponse(err).Code)
}

func TestMCPBuiltinArguments(t *testing.T) {
	for _, tc := range []struct {
		tool      string
		arguments map[string]any
		valid     bool
	}{
		{"web_search", map[string]any{"query": "知识", "need_content": false}, true},
		{"web_search", map[string]any{"query": "   "}, false},
		{"web_search", map[string]any{"query": "知识", "count": float64(51)}, false},
		{"web_search", map[string]any{"query": "知识", "count": float64(1.5)}, false},
		{"web_fetch", map[string]any{"url": "https://example.com/article"}, true},
		{"web_fetch", map[string]any{"url": "https://user:password@example.com"}, false},
		{"create_consultation", map[string]any{"knowledge_base": map[string]any{"kb_id": "知识", "owner": "u1"}, "question": "问题", "request_id": "request-1"}, true},
		{"create_consultation", map[string]any{"knowledge_base": map[string]any{"kb_id": "知识", "owner": "u1"}, "question": "问题", "request_id": "request-1", "user_id": "someone-else"}, false},
		{"get_consultation_result", map[string]any{"task_id": "task-1", "wait_seconds": float64(0)}, true},
		{"get_consultation_result", map[string]any{"task_id": "task-1", "wait_seconds": float64(26)}, false},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			_, err := ValidateBuiltinArguments(tc.tool, tc.arguments)
			if tc.valid {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

func TestMCPRemoteDiscoveryAndSessionIsolation(t *testing.T) {
	setupMCPService(t)
	ctx := context.Background()
	sessions := make(chan string, 2)
	upstream := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "fixture", Version: "1"}, nil)
	definition := &mcpsdk.Tool{Name: "echo", Description: "Echo", InputSchema: model.MCPJSON{"type": "object", "properties": model.MCPJSON{"text": model.MCPJSON{"type": "string"}}}}
	handler := func(_ context.Context, r *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		sessions <- r.Session.ID()
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "answer"}}, Meta: mcpsdk.Meta{"private": "hidden"}}, nil
	}
	upstream.AddTool(definition, handler)
	transport := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return upstream }, nil)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer service-credential", r.Header.Get("Authorization"))
		transport.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)
	t.Setenv("MCP_ALLOWED_UPSTREAM_ORIGINS", httpServer.URL)
	server, err := CreateServer(ServerInput{Slug: "remote-fixture", Name: "Remote", Kind: "remote_mcp", AllowedUserGroups: model.MCPGroups{"default"}, Connection: model.MCPConnection{URL: httpServer.URL, Transport: "streamable_http"}})
	require.NoError(t, err)
	server, err = SetCredentials(server.ID, CredentialsInput{ExpectedRevision: server.Revision, Headers: map[string]string{"Authorization": "Bearer service-credential"}})
	require.NoError(t, err)
	found, err := DiscoverTools(ctx, server.ID, 71)
	require.NoError(t, err)
	require.NoError(t, SyncTools(server.ID, found["discovery_id"].(string), 71, server.Revision))
	var tool model.MCPTool
	require.NoError(t, model.DB.Where("server_id = ?", server.ID).First(&tool).Error)
	assert.False(t, tool.Enabled)
	assert.Empty(t, tool.PriceMode)
	require.NoError(t, model.DB.Model(&tool).Updates(map[string]any{"enabled": true, "price_mode": "per_success", "unit_price_usd": "0.001", "price_revision": 2}).Error)
	for _, user := range []int{71, 72} {
		outcome, err := ExecuteAdapter(ctx, server, &tool, &Actor{UserID: user, AccessToken: "never-forward-user-token"}, map[string]any{"text": "hello"}, "", nil)
		require.NoError(t, err)
		assert.True(t, outcome.Billable)
		assert.Nil(t, outcome.Result.Meta)
	}
	first, second := <-sessions, <-sessions
	require.NotEmpty(t, first)
	assert.NotEqual(t, first, second)
	changed := *definition
	changed.InputSchema = model.MCPJSON{"type": "object", "properties": model.MCPJSON{"text": model.MCPJSON{"type": "string"}}, "required": []string{"text"}}
	upstream.AddTool(&changed, handler)
	server, err = model.GetMCPServer(server.ID)
	require.NoError(t, err)
	found, err = DiscoverTools(ctx, server.ID, 71)
	require.NoError(t, err)
	assert.Equal(t, []string{"echo"}, found["changed"])
	require.NoError(t, SyncTools(server.ID, found["discovery_id"].(string), 71, server.Revision))
	require.NoError(t, model.DB.First(&tool, "id = ?", tool.ID).Error)
	assert.False(t, tool.Enabled)
	assert.True(t, tool.ReviewRequired)
	assert.Equal(t, "0.001", tool.UnitPriceUSD)
	assert.EqualValues(t, 2, tool.PriceRevision)
	upstream.RemoveTools("echo")
	server, err = model.GetMCPServer(server.ID)
	require.NoError(t, err)
	found, err = DiscoverTools(ctx, server.ID, 71)
	require.NoError(t, err)
	require.NoError(t, SyncTools(server.ID, found["discovery_id"].(string), 71, server.Revision))
	require.NoError(t, model.DB.First(&tool, "id = ?", tool.ID).Error)
	assert.True(t, tool.Removed)
}

func TestMCPAPIKeyAuthorizationWithoutIdentityBinding(t *testing.T) {
	setupMCPService(t)
	t.Setenv("MCP_CASDOOR_ISSUER", "")
	t.Setenv("MCP_CASDOOR_PROVIDER_ID", "")
	restrictedIPs := "192.0.2.0/24"
	for i, tc := range []struct {
		name, authMode, kind, header, group string
		tokenStatus, userStatus             int
		expires                             int64
		allowedIPs                          *string
		wantCode                            string
	}{
		{name: "ordinary key", authMode: "api_key"},
		{name: "legacy mode", authMode: "engine_api_key"},
		{name: "no header", authMode: "api_key", header: "missing", wantCode: "UNAUTHORIZED"},
		{name: "wrong key", authMode: "api_key", header: "Bearer sk-unknown", wantCode: "UNAUTHORIZED"},
		{name: "expired key", authMode: "api_key", expires: 1, wantCode: "UNAUTHORIZED"},
		{name: "disabled key", authMode: "api_key", tokenStatus: common.TokenStatusDisabled, wantCode: "UNAUTHORIZED"},
		{name: "disabled user", authMode: "api_key", userStatus: common.UserStatusDisabled, wantCode: "USER_DISABLED"},
		{name: "IP restricted", authMode: "api_key", allowedIPs: &restrictedIPs, wantCode: "IP_NOT_ALLOWED"},
		{name: "group restricted", authMode: "api_key", group: "private", wantCode: "SERVER_ACCESS_DENIED"},
		{name: "unknown mode cannot become API key", authMode: "none", wantCode: "MCP_NOT_CONFIGURED"},
		{name: "knowledge cannot use API key", authMode: "api_key", kind: "knowledge", wantCode: "MCP_NOT_CONFIGURED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := model.User{Username: fmt.Sprintf("ordinary-key-user-%d", i), AffCode: fmt.Sprintf("ordinary-key-user-%d", i), Group: "default", Status: common.UserStatusEnabled}
			if tc.userStatus != 0 {
				user.Status = tc.userStatus
			}
			require.NoError(t, model.DB.Create(&user).Error)
			token := model.Token{UserId: user.Id, Key: fmt.Sprintf("ordinary-key-%d", i), Status: common.TokenStatusEnabled, ExpiredTime: -1, AllowIps: tc.allowedIPs}
			if tc.tokenStatus != 0 {
				token.Status = tc.tokenStatus
			}
			if tc.expires != 0 {
				token.ExpiredTime = tc.expires
			}
			require.NoError(t, model.DB.Create(&token).Error)
			server := &model.MCPServer{Kind: "deepseek", AuthMode: tc.authMode, AllowedUserGroups: model.MCPGroups{"default"}}
			if tc.kind != "" {
				server.Kind = tc.kind
			}
			if tc.group != "" {
				server.AllowedUserGroups = model.MCPGroups{tc.group}
			}
			header := "Bearer sk-" + token.Key
			if tc.header != "" {
				header = tc.header
			}
			actor, err := Authenticate(context.Background(), server, header, "127.0.0.1")
			if tc.wantCode != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantCode, ErrorResponse(err).Code)
				assert.Nil(t, actor)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, user.Id, actor.UserID)
			assert.Equal(t, token.Id, actor.TokenID)
			assert.Empty(t, actor.AccessToken, "caller key is never forwarded to providers")
		})
	}
}

func TestMCPCatalogTransportsAndOAuthInstallation(t *testing.T) {
	setupMCPService(t)
	const browserSession = "signed-in-session"
	for _, transport := range []string{"streamable_http", "http_sse", "oauth2", "oauth_sse"} {
		t.Run(transport, func(t *testing.T) {
			ctx := context.Background()
			sdk := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "fixture", Version: "2.0"}, nil)
			sdk.AddTool(&mcpsdk.Tool{Name: "check", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil
			})
			getServer := func(*http.Request) *mcpsdk.Server { return sdk }
			var handler http.Handler = mcpsdk.NewStreamableHTTPHandler(getServer, nil)
			if transport == "http_sse" || transport == "oauth_sse" {
				handler = mcpsdk.NewSSEHandler(getServer, nil)
			}
			mux := http.NewServeMux()
			fixture := httptest.NewServer(mux)
			defer fixture.Close()
			t.Setenv("MCP_ALLOWED_UPSTREAM_ORIGINS", fixture.URL)
			endpoint := fixture.URL + "/mcp"
			var exchanges atomic.Int32
			if transport == "oauth2" || transport == "oauth_sse" {
				mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
					fmt.Fprintf(w, `{"resource":%q,"authorization_servers":[%q]}`, endpoint, fixture.URL)
				})
				mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
					fmt.Fprintf(w, `{"issuer":%q,"authorization_endpoint":%q,"token_endpoint":%q,"code_challenge_methods_supported":["S256"],"authorization_response_iss_parameter_supported":true}`, fixture.URL, fixture.URL+"/authorize", fixture.URL+"/token")
				})
				mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
					assert.NoError(t, r.ParseForm())
					assert.Equal(t, endpoint, r.Form.Get("resource"))
					exchanges.Add(1)
					w.Header().Set("Content-Type", "application/json")
					if r.Form.Get("grant_type") == "refresh_token" {
						assert.Equal(t, "initial-refresh", r.Form.Get("refresh_token"))
						fmt.Fprint(w, `{"access_token":"renewed","refresh_token":"renewed-refresh","token_type":"Bearer","expires_in":3600}`)
						return
					}
					assert.NotEmpty(t, r.Form.Get("code_verifier"))
					fmt.Fprint(w, `{"access_token":"authorized","refresh_token":"initial-refresh","token_type":"Bearer","expires_in":3600}`)
				})
				mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer authorized" {
						w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+fixture.URL+`/.well-known/oauth-protected-resource/mcp"`)
						w.WriteHeader(401)
						return
					}
					handler.ServeHTTP(w, r)
				})
			} else {
				mux.Handle("/mcp", handler)
			}
			spec := ServerInput{Icon: "OpenAI", Readme: "Fixture README", Name: "Unified", Version: "2.0", AuthMode: "none", Connection: model.MCPConnection{URL: endpoint, Transport: transport}}
			if transport == "oauth2" || transport == "oauth_sse" {
				spec.AuthMode, spec.Connection.Transport, spec.Connection.ClientID = "oauth2", "streamable_http", "registered"
				if transport == "oauth_sse" {
					spec.Connection.Transport = "http_sse"
				}
			}
			server, err := CreateServer(spec)
			require.NoError(t, err)
			require.True(t, server.Catalog)
			require.False(t, server.Enabled)
			_, err = Install(server.ID, 81, false)
			require.Error(t, err)
			row, err := Install(server.ID, 81, true)
			require.NoError(t, err)
			if transport == "oauth2" || transport == "oauth_sse" {
				target, err := BeginAuthorization(ctx, row, browserSession)
				require.NoError(t, err)
				parsed, err := url.Parse(target)
				require.NoError(t, err)
				state := parsed.Query().Get("state")
				assert.Equal(t, "S256", parsed.Query().Get("code_challenge_method"))
				assert.Equal(t, endpoint, parsed.Query().Get("resource"))
				_, err = FinishAuthorization(ctx, 82, browserSession, state, "code", fixture.URL)
				require.Error(t, err)
				_, err = FinishAuthorization(ctx, 81, "different-session", state, "code", fixture.URL)
				require.Error(t, err)
				admin, err := FinishAuthorization(ctx, 81, browserSession, state, "code", fixture.URL)
				require.NoError(t, err)
				assert.True(t, admin)
				_, err = FinishAuthorization(ctx, 81, browserSession, state, "code", fixture.URL)
				require.Error(t, err)
				assert.EqualValues(t, 1, exchanges.Load())
			}
			tested, err := TestConnection(ctx, server.ID, 81, browserSession)
			require.NoError(t, err)
			assert.Equal(t, "2.0", tested["version"])
			discovery, err := DiscoverTools(ctx, server.ID, 81)
			require.NoError(t, err)
			require.NoError(t, SyncTools(server.ID, discovery["discovery_id"].(string), 81, server.Revision))
			views, err := ListTools(server.ID)
			require.NoError(t, err)
			require.Len(t, views, 1)
			tool := views[0]
			_, err = SetToolPrice(server.ID, tool.ID, tool.Revision, "free", "0")
			require.NoError(t, err)
			enabled := true
			require.NoError(t, PatchTool(server.ID, tool.ID, ToolPatch{ExpectedRevision: tool.Revision + 1, Enabled: &enabled}, true))
			server, err = SetServerStatus(server.ID, server.Revision+1, true)
			require.NoError(t, err)
			installed, err := Install(server.ID, 81, false)
			require.NoError(t, err)
			assert.NotEqual(t, row.ID, installed.ID)
			if transport == "oauth2" || transport == "oauth_sse" {
				_, err = InstallationToken(ctx, server, installed)
				require.Error(t, err, "personal installation must be enabled before calls")
				sharedToken, err := ReusableInstallationToken(ctx, server, 81)
				require.NoError(t, err)
				assert.Equal(t, "authorized", sharedToken)
				_, err = ReusableInstallationToken(ctx, server, 82)
				require.Error(t, err, "another user cannot reuse this grant")
				require.NoError(t, model.DB.Model(installed).Update("authorized", true).Error)
				installed, err = FindInstallation(server.ID, 81, false)
				require.NoError(t, err)
				sharedToken, err = InstallationToken(ctx, server, installed)
				require.NoError(t, err)
				assert.Equal(t, "authorized", sharedToken)
				target, err := BeginAuthorization(ctx, installed, browserSession)
				require.NoError(t, err)
				parsed, err := url.Parse(target)
				require.NoError(t, err)
				require.NoError(t, DisableInstallation(installed.ID, 81))
				_, err = FinishAuthorization(ctx, 81, browserSession, parsed.Query().Get("state"), "code", fixture.URL)
				require.Error(t, err)
				assert.EqualValues(t, 1, exchanges.Load())
				row, err = FindInstallation(server.ID, 81, true)
				require.NoError(t, err)
				plaintext, err := Open(string(row.Grant), "oauth-grant:"+row.ID)
				require.NoError(t, err)
				var grant oauthGrant
				require.NoError(t, common.UnmarshalJsonStr(plaintext, &grant))
				grant.Token.Expiry = time.Now().Add(-time.Second)
				require.NoError(t, saveGrant(row, server.ConnectionRevision, grant))
				row, err = FindInstallation(server.ID, 81, true)
				require.NoError(t, err)
				refreshed, err := InstallationToken(ctx, server, row)
				require.NoError(t, err)
				assert.Equal(t, "renewed", refreshed)
				assert.EqualValues(t, 2, exchanges.Load())
				require.NoError(t, DisconnectInstallation(installed.ID, 81))
				_, err = ReusableInstallationToken(ctx, server, 81)
				require.Error(t, err, "reset must clear the shared credential")
			} else {
				assert.False(t, installed.Authorized)
			}
		})
	}
}

func TestMCPPinnedStdioPackage(t *testing.T) {
	registry := os.Getenv("MCP_TEST_STDIO_REGISTRY")
	if registry == "" {
		t.Skip("set MCP_TEST_STDIO_REGISTRY to an isolated loopback OCI registry")
	}
	require.True(t, strings.HasPrefix(registry, "localhost:") || strings.HasPrefix(registry, "127.0.0.1:"))
	setupMCPService(t)
	dir := t.TempDir()
	source := `package main
import("context";"os";"github.com/modelcontextprotocol/go-sdk/mcp")
type Args struct{}
type Result struct {Argument string; Public string; Private string; UID int}
func main(){
 s:=mcp.NewServer(&mcp.Implementation{Name:"stdio-fixture",Version:"1.2.3"},nil)
 mcp.AddTool(s,&mcp.Tool{Name:"environment",Description:"read the fixture environment"},func(context.Context,*mcp.CallToolRequest,Args)(*mcp.CallToolResult,Result,error){return nil,Result{os.Args[1],os.Getenv("DECLARED"),os.Getenv("MCP_PRIVATE_FIXTURE"),os.Getuid()},nil})
 _=s.Run(context.Background(),&mcp.StdioTransport{})
}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(source), 0600))
	build := exec.Command("go", "build", "-o", filepath.Join(dir, "fixture"), filepath.Join(dir, "main.go"))
	build.Env = append(os.Environ(), "GOOS=linux", "CGO_ENABLED=0")
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY fixture /fixture\n"), 0600))
	ref := registry + "/mcp/test:1.2.3"
	for _, args := range [][]string{{"build", "-q", "-t", ref, dir}, {"push", ref}} {
		out, err := exec.Command("docker", args...).CombinedOutput()
		require.NoError(t, err, string(out))
	}
	t.Setenv("MCP_STDIO_PACKAGE_SOURCES", registry+"/mcp")
	t.Setenv("MCP_PRIVATE_FIXTURE", "gateway-only")
	server, err := CreateServer(ServerInput{Name: "Stdio fixture", Env: map[string]string{"DECLARED": "service value"}, Connection: model.MCPConnection{Transport: "stdio", Command: "/fixture 'two words'", Package: ref}})
	require.NoError(t, err)
	_, err = DiscoverTools(t.Context(), server.ID, 1)
	require.Error(t, err)
	server, err = InstallPackage(t.Context(), server.ID, server.Revision)
	require.NoError(t, err)
	require.Regexp(t, "^sha256:[a-f0-9]{64}$", server.Connection.PackageDigest)
	discovery, err := DiscoverTools(t.Context(), server.ID, 1)
	require.NoError(t, err)
	tools := discovery["tools"].([]model.MCPTool)
	require.Len(t, tools, 1)
	result, err := ExecuteAdapter(t.Context(), server, &tools[0], &Actor{}, map[string]any{}, "", nil)
	require.NoError(t, err)
	assert.Contains(t, fmt.Sprint(result.Result.StructuredContent), "two words")
	assert.Contains(t, fmt.Sprint(result.Result.StructuredContent), "service value")
	assert.NotContains(t, fmt.Sprint(result.Result.StructuredContent), "gateway-only")
	assert.Contains(t, fmt.Sprint(result.Result.StructuredContent), "65534")
}

func TestMCPAutomaticDiscoveryAuthorization(t *testing.T) {
	setupMCPService(t)
	mux := http.NewServeMux()
	fixture := httptest.NewServer(mux)
	defer fixture.Close()
	t.Setenv("MCP_ALLOWED_UPSTREAM_ORIGINS", fixture.URL)
	endpoint := fixture.URL + "/mcp"
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		// Some gateways omit WWW-Authenticate on their POST response.
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"resource":%q,"authorization_servers":[%q]}`, endpoint, fixture.URL)
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"issuer":%q,"authorization_endpoint":%q,"token_endpoint":%q,"registration_endpoint":%q,"token_endpoint_auth_methods_supported":["client_secret_post"],"code_challenge_methods_supported":["S256"]}`, fixture.URL, fixture.URL+"/authorize", fixture.URL+"/token", fixture.URL+"/register")
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, common.DecodeJson(r.Body, &body))
		assert.Equal(t, "client_secret_post", body["token_endpoint_auth_method"])
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"client_id":"dynamic-client","client_secret":"private-secret"}`)
	})
	server, err := CreateServer(ServerInput{Name: "Automatic OAuth", Connection: model.MCPConnection{URL: endpoint, Transport: "streamable_http"}})
	require.NoError(t, err)
	_, err = DiscoverTools(t.Context(), server.ID, 81)
	require.Error(t, err)
	assert.Equal(t, "REAUTHORIZATION_REQUIRED", ErrorResponse(err).Code)
	row, err := Install(server.ID, 81, true)
	require.NoError(t, err)
	target, err := BeginAuthorization(t.Context(), row, "browser-session")
	require.NoError(t, err)
	parsed, err := url.Parse(target)
	require.NoError(t, err)
	assert.Equal(t, "dynamic-client", parsed.Query().Get("client_id"))
	assert.Equal(t, "S256", parsed.Query().Get("code_challenge_method"))
	assert.NotEmpty(t, parsed.Query().Get("state"))
	assert.Equal(t, endpoint, parsed.Query().Get("resource"))
	saved, err := model.GetMCPServer(server.ID)
	require.NoError(t, err)
	assert.Equal(t, "oauth2", saved.AuthMode)
	assert.NotContains(t, saved.CredentialCiphertext, "private-secret")
	secret, err := Open(saved.CredentialCiphertext, "oauth-client:"+saved.ID)
	require.NoError(t, err)
	assert.Equal(t, "private-secret", secret)
	blocked, err := CreateServer(ServerInput{Name: "Untrusted reserved address", Connection: model.MCPConnection{URL: "https://198.18.0.1/mcp", Transport: "streamable_http"}})
	require.NoError(t, err)
	_, err = DiscoverTools(t.Context(), blocked.ID, 81)
	require.Error(t, err)
	assert.Equal(t, "UPSTREAM_ADDRESS_BLOCKED", ErrorResponse(err).Code)

}

func TestMCPSharedGrantUpgradeAndRevocation(t *testing.T) {
	setupMCPService(t)
	server, err := CreateServer(ServerInput{Name: "Shared grant", Connection: model.MCPConnection{URL: "https://service.test/mcp", Transport: "streamable_http", ClientID: "registered"}})
	require.NoError(t, err)
	require.NoError(t, model.DB.Model(server).Update("enabled", true).Error)
	server.Enabled = true
	personal, err := Install(server.ID, 91, false)
	require.NoError(t, err)
	// Represent an existing personal grant created before credentials were unified.
	grant := oauthGrant{Token: oauth2.Token{AccessToken: "existing-user-token", Expiry: time.Now().Add(time.Hour)}}
	raw, err := common.Marshal(grant)
	require.NoError(t, err)
	sealed, err := Seal(string(raw), "oauth-grant:"+personal.ID)
	require.NoError(t, err)
	require.NoError(t, model.DB.Model(personal).Update("grant_ciphertext", sealed).Error)
	token, err := ReusableInstallationToken(t.Context(), server, 91)
	require.NoError(t, err)
	assert.Equal(t, "existing-user-token", token)
	personal, err = FindInstallation(server.ID, 91, false)
	require.NoError(t, err)
	assert.False(t, personal.Authorized, "migrating a credential must not enable an installation")
	assert.Empty(t, personal.Grant)
	shared, err := FindInstallation(server.ID, 91, true)
	require.NoError(t, err)
	assert.NotEmpty(t, shared.Grant)
	_, err = ReusableInstallationToken(t.Context(), server, 92)
	require.Error(t, err)
	changed := *server
	changed.ConnectionRevision++
	_, err = ReusableInstallationToken(t.Context(), &changed, 91)
	require.Error(t, err, "changed destination requires a new authorization")
	require.Error(t, DisconnectInstallation(personal.ID, 92))
	require.NoError(t, DisableInstallation(personal.ID, 91))
	token, err = ReusableInstallationToken(t.Context(), server, 91)
	require.NoError(t, err)
	assert.Equal(t, "existing-user-token", token)
	require.NoError(t, DisconnectInstallation(personal.ID, 91))
	_, err = ReusableInstallationToken(t.Context(), server, 91)
	require.Error(t, err)
	require.ErrorIs(t, saveGrant(shared, server.ConnectionRevision, grant), model.ErrMCPRevisionConflict, "a stale refresh must not restore a revoked token")
}
