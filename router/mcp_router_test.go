package router

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service/authz"
	mcpsvc "github.com/QuantumNous/new-api/service/mcp"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

func TestMCPManagementHTTPContract(t *testing.T) {
	oldDB, oldLogDB := model.DB, model.LOG_DB
	oldMainType, oldLogType := common.MainDatabaseType(), common.LogDatabaseType()
	oldMaster, oldRedis, oldRate, oldSQLite := common.IsMasterNode, common.RedisEnabled, common.GlobalApiRateLimitEnable, common.SQLitePath
	testDSN := os.Getenv("MCP_ROUTER_TEST_SQL_DSN")
	if testDSN != "" {
		require.True(t, strings.HasPrefix(testDSN, "root@tcp(127.0.0.1:23306)/mcp_router_test?") || strings.HasPrefix(testDSN, "postgres://postgres:mcp-local-test@127.0.0.1:25432/mcp_router_test?"))
	}
	t.Setenv("SQL_DSN", testDSN)
	logDSN := os.Getenv("MCP_ROUTER_TEST_LOG_SQL_DSN")
	if logDSN != "" {
		parsed, err := url.Parse(logDSN)
		require.NoError(t, err)
		require.Equal(t, "clickhouse", parsed.Scheme)
		require.Equal(t, "127.0.0.1:56751", parsed.Host)
		require.Equal(t, "/mcp_gateway_log_test", parsed.Path)
	}
	t.Setenv("LOG_SQL_DSN", logDSN)
	t.Setenv("MCP_ENABLED", "true")
	t.Setenv("MCP_PUBLIC_BASE_URL", "https://gateway.test")
	t.Setenv("MCP_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	common.IsMasterNode, common.RedisEnabled, common.GlobalApiRateLimitEnable = true, false, false
	common.SQLitePath = filepath.Join(t.TempDir(), "management.db")
	require.NoError(t, model.InitDB())
	require.NoError(t, model.InitLogDB())
	require.NoError(t, authz.Init(model.DB))
	t.Cleanup(func() {
		db, err := model.DB.DB()
		require.NoError(t, err)
		require.NoError(t, db.Close())
		model.DB, model.LOG_DB = oldDB, oldLogDB
		common.SetMainDatabaseType(oldMainType)
		common.SetLogDatabaseType(oldLogType)
		common.IsMasterNode, common.RedisEnabled, common.GlobalApiRateLimitEnable, common.SQLitePath = oldMaster, oldRedis, oldRate, oldSQLite
	})
	for _, entry := range []struct {
		name string
		role int
	}{{"root", common.RoleRootUser}, {"admin", common.RoleAdminUser}, {"user", common.RoleCommonUser}} {
		key := "management-" + entry.name
		require.NoError(t, model.DB.Create(&model.User{Username: entry.name, AffCode: entry.name, Role: entry.role, Status: common.UserStatusEnabled, AuthVersion: 1, AccessToken: &key, Group: "default"}).Error)
	}
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	SetMCPRouter(engine)
	request := func(method, path, identity, body, origin, guard string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if identity != "" {
			r.Header.Set("Authorization", "Bearer management-"+identity)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if guard != "" {
			r.Header.Set("X-MCP-Request", guard)
		}
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, r)
		return w
	}
	create := `{"slug":"remote-test","name":"Remote","kind":"remote_mcp","allowed_user_groups":["default"],"connection":{"url":"https://example.com/mcp","transport":"streamable_http"}}`
	for _, tc := range []struct {
		identity, origin, guard string
		status                  int
	}{
		{"", "", "", 401}, {"user", "", "1", 403}, {"admin", "", "1", 403}, {"root", "https://evil.test", "1", 403}, {"root", "", "", 403},
	} {
		w := request("POST", "/api/mcp/servers", tc.identity, create, tc.origin, tc.guard)
		assert.Equal(t, tc.status, w.Code, w.Body.String())
	}
	created := request("POST", "/api/mcp/servers", "root", create, "https://gateway.test", "1")
	require.Equal(t, 201, created.Code, created.Body.String())
	var result struct {
		Data model.MCPServer `json:"data"`
	}
	require.NoError(t, common.Unmarshal(created.Body.Bytes(), &result))
	assert.False(t, result.Data.Enabled)
	path := "/api/mcp/servers/" + result.Data.ID
	patched := request("PATCH", path, "admin", `{"expected_revision":1,"name":"Renamed"}`, "", "1")
	require.Equal(t, 200, patched.Code, patched.Body.String())
	stale := request("PATCH", path, "admin", `{"expected_revision":1,"name":"Stale"}`, "", "1")
	assert.Equal(t, 409, stale.Code)
	assert.Contains(t, stale.Body.String(), "REVISION_CONFLICT")
	forbidden := request("PATCH", path, "admin", `{"expected_revision":2,"allowed_user_groups":["other"]}`, "", "1")
	assert.Equal(t, 403, forbidden.Code)
	secret := request("PUT", path+"/credentials", "root", `{"expected_revision":2,"headers":{"Authorization":"Bearer upstream-secret"}}`, "", "1")
	require.Equal(t, 200, secret.Code, secret.Body.String())
	for _, endpoint := range []string{path, "/api/mcp/servers"} {
		w := request("GET", endpoint, "admin", "", "", "")
		require.Equal(t, 200, w.Code, w.Body.String())
		assert.NotContains(t, w.Body.String(), "upstream-secret")
		assert.NotContains(t, w.Body.String(), "credential_ciphertext")
	}
	assert.Equal(t, 422, request("GET", "/api/mcp/servers?page_size=101", "admin", "", "", "").Code)
	t.Run("administrators can price tools without permission to change credentials", func(t *testing.T) {
		tool := model.MCPTool{ID: "price-test-tool", ServerID: result.Data.ID, Name: "price-test", Description: "Price test", Revision: 1}
		require.NoError(t, model.DB.Create(&tool).Error)
		endpoint := path + "/tools/" + tool.ID + "/pricing"
		body := `{"expected_revision":1,"mode":"per_success","unit_price":"0.001","currency":"USD"}`
		for _, tc := range []struct {
			identity, origin, guard string
			status                  int
		}{
			{"", "", "1", 401}, {"user", "", "1", 403},
			{"admin", "https://evil.test", "1", 403}, {"admin", "", "", 403},
		} {
			assert.Equal(t, tc.status, request("PUT", endpoint, tc.identity, body, tc.origin, tc.guard).Code)
		}
		var admin model.User
		require.NoError(t, model.DB.Where("username = ?", "admin").First(&admin).Error)
		require.NoError(t, authz.SetUserPermissions(admin.Id, authz.PermissionsMap{authz.ResourceMCP: {authz.ActionWrite: false}}))
		assert.Equal(t, 403, request("PUT", endpoint, "admin", body, "", "1").Code)
		require.NoError(t, authz.ClearUserPermissions(admin.Id))
		updated := request("PUT", endpoint, "admin", body, "", "1")
		require.Equal(t, 200, updated.Code, updated.Body.String())
		assert.Contains(t, updated.Body.String(), `"unit_price":"0.001"`)
		assert.Equal(t, 409, request("PUT", endpoint, "admin", body, "", "1").Code)
		assert.Equal(t, 403, request("PUT", path+"/credentials", "admin", `{"expected_revision":3}`, "", "1").Code)
	})
	t.Run("draft metadata and transport bounds survive reopening", func(t *testing.T) {
		for _, tc := range []struct {
			connection      string
			timeout, status int
		}{
			{`{"transport":"streamable_http","url":"https://example.com/mcp"}`, 1, 201},
			{`{"transport":"http_sse","url":"https://example.com/sse"}`, 600, 201},
			{`{"transport":"stdio","command":"echo 'two words'","package":"registry.test/mcp/example:1.2.3"}`, 600, 201},
			{`{"transport":"stdio","url":"https://example.com","command":"echo"}`, 1, 422},
			{`{"transport":"http_sse","url":"https://example.com","command":"echo"}`, 1, 422},
			{`{"transport":"streamable_http","url":"https://example.com"}`, 601, 422},
		} {
			body := fmt.Sprintf(`{"name":"Draft","icon":"OpenAI","readme":"# Instructions","connection":%s,"limits":{"timeout_seconds":%d}}`, tc.connection, tc.timeout)
			reply := request("POST", "/api/mcp/servers", "root", body, "", "1")
			require.Equal(t, tc.status, reply.Code, reply.Body.String())
			if tc.status != 201 {
				continue
			}
			var saved struct{ Data struct{ ID string } }
			require.NoError(t, common.Unmarshal(reply.Body.Bytes(), &saved))
			detail := request("GET", "/api/mcp/servers/"+saved.Data.ID, "root", "", "", "")
			assert.Contains(t, detail.Body.String(), `"readme":"# Instructions"`)
			assert.Contains(t, detail.Body.String(), `"icon":"OpenAI"`)
		}
		assert.NotContains(t, request("GET", "/api/mcp/servers", "root", "", "", "").Body.String(), "# Instructions")
		assert.Equal(t, 60, result.Data.Limits.TimeoutSeconds)
	})
	callback := request("GET", "/api/mcp/test-authorizations/callback?state=one-use-state&code=one-use-code", "", "", "", "")
	assert.Equal(t, 303, callback.Code)
	assert.Contains(t, callback.Header().Get("Location"), "/mcp#mcp_code=")
	assert.Equal(t, "no-referrer", callback.Header().Get("Referrer-Policy"))
	redeem := request("POST", "/api/mcp/test-authorizations/callback", "admin", `{"state":"one-use-state","code":"one-use-code"}`, "", "1")
	assert.Equal(t, 400, redeem.Code, "PAT cannot replace the originating browser session")

	t.Run("catalog includes copy configurations for built-in and remote installed services", func(t *testing.T) {
		for _, fixture := range []struct {
			id, kind, transport, url, copiedType, copiedURL string
			enabled                                         bool
		}{
			{"copy-builtin", "deepseek", "streamable_http", "http://internal.test/deepseek", "streamable-http", "https://gateway.test/mcp/copy-builtin", true},
			{"copy-remote", "remote_mcp", "http_sse", "https://provider.test/sse", "sse", "https://provider.test/sse", true},
		} {
			require.NoError(t, model.DB.Create(&model.MCPServer{ID: fixture.id, Slug: fixture.id, Name: fixture.id, Kind: fixture.kind, Catalog: true, Enabled: fixture.enabled, Connection: model.MCPConnection{Transport: fixture.transport, URL: fixture.url}}).Error)
			var user model.User
			require.NoError(t, model.DB.First(&user, "username = ?", "user").Error)
			require.NoError(t, model.DB.Create(&model.MCPInstallation{ID: fixture.id, ServerID: fixture.id, UserID: user.Id}).Error)
			reply := request("GET", "/api/mcp/catalog", "user", "", "", "")
			require.Equal(t, 200, reply.Code)
			var response struct {
				Data struct {
					Items []struct {
						ID               string
						ClientConnection struct {
							Type string
							URL  string
						} `json:"client_connection"`
					}
				}
			}
			require.NoError(t, common.Unmarshal(reply.Body.Bytes(), &response))
			var copiedURL, copiedType string
			for _, item := range response.Data.Items {
				if item.ID == fixture.id {
					copiedURL, copiedType = item.ClientConnection.URL, item.ClientConnection.Type
				}
			}
			assert.Equal(t, fixture.copiedURL, copiedURL)
			assert.Equal(t, fixture.copiedType, copiedType)
		}
	})

	t.Run("marketplace hides disabled and archived services even when installed", func(t *testing.T) {
		server := model.MCPServer{ID: "visibility-test", Slug: "visibility-test", Name: "visibility-test", Kind: "remote_mcp", Catalog: true, Enabled: true}
		require.NoError(t, model.DB.Create(&server).Error)
		var user model.User
		require.NoError(t, model.DB.First(&user, "username = ?", "user").Error)
		require.NoError(t, model.DB.Create(&model.MCPInstallation{ID: server.ID, ServerID: server.ID, UserID: user.Id}).Error)
		for _, state := range []struct {
			name              string
			enabled, archived bool
			count             int64
		}{
			{"enabled", true, false, 1},
			{"disabled", false, false, 0},
			{"archived", true, true, 0},
			{"re-enabled", true, false, 1},
		} {
			t.Run(state.name, func(t *testing.T) {
				require.NoError(t, model.DB.Model(&server).Updates(map[string]any{"enabled": state.enabled, "archived": state.archived}).Error)
				for _, endpoint := range []string{"/api/mcp/catalog", "/api/mcp/self/installations"} {
					reply := request("GET", endpoint+"?name="+server.Name, "user", "", "", "")
					require.Equal(t, 200, reply.Code, reply.Body.String())
					var response struct {
						Data struct {
							Items []struct{ ID string }
							Total int64
						}
					}
					require.NoError(t, common.Unmarshal(reply.Body.Bytes(), &response))
					assert.Equal(t, state.count, response.Data.Total, endpoint)
					require.Len(t, response.Data.Items, int(state.count), endpoint)
					if state.count > 0 {
						assert.Equal(t, server.ID, response.Data.Items[0].ID, endpoint)
					}
				}
			})
		}
	})

	t.Run("installed tool is charged once across chat test replay", func(t *testing.T) {
		var toolCalls atomic.Int32
		upstream := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, nil)
		upstream.AddTool(&sdk.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			toolCalls.Add(1)
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "tool says hello"}}}, nil
		})
		endpoint := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return upstream }, nil))
		defer endpoint.Close()
		t.Setenv("MCP_ALLOWED_UPSTREAM_ORIGINS", endpoint.URL)
		server, err := mcpsvc.CreateServer(mcpsvc.ServerInput{Icon: "OpenAI", Readme: "Fixture README", Name: "Integrated", Connection: model.MCPConnection{URL: endpoint.URL, Transport: "streamable_http"}})
		require.NoError(t, err)
		require.NoError(t, model.DB.Create(&model.MCPTool{ID: "echo-tool", ServerID: server.ID, Name: "echo", InputSchema: model.MCPJSON{"type": "object"}, Enabled: true, PriceMode: "per_success", UnitPriceUSD: "0.001", PriceRevision: 1, Revision: 1}).Error)
		_, err = mcpsvc.SetServerStatus(server.ID, server.Revision, true)
		require.NoError(t, err)
		var user model.User
		require.NoError(t, model.DB.First(&user, "username = ?", "user").Error)
		require.NoError(t, model.DB.Model(&user).Update("quota", 10000).Error)
		token := model.Token{UserId: user.Id, Key: "mcptesttoken", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 10000}
		require.NoError(t, model.DB.Create(&token).Error)
		catalog := request("GET", "/api/mcp/catalog", "user", "", "", "")
		require.Equal(t, 200, catalog.Code)
		require.Contains(t, catalog.Body.String(), server.ID)
		var catalogResponse struct {
			Data struct {
				Items []struct {
					ID               string
					ClientConnection struct {
						Type string
						URL  string
					} `json:"client_connection"`
				}
			}
		}
		require.NoError(t, common.Unmarshal(catalog.Body.Bytes(), &catalogResponse))
		var copiedURL, copiedType string
		for _, item := range catalogResponse.Data.Items {
			if item.ID == server.ID {
				copiedURL, copiedType = item.ClientConnection.URL, item.ClientConnection.Type
			}
		}
		assert.Equal(t, endpoint.URL, copiedURL)
		assert.Equal(t, "streamable-http", copiedType)
		body, err := common.Marshal(map[string]any{"server_id": server.ID})
		require.NoError(t, err)
		install := request("POST", "/api/mcp/self/installations", "user", string(body), "", "1")
		require.Equal(t, 201, install.Code, install.Body.String())
		var installed struct{ Data model.MCPInstallation }
		require.NoError(t, common.Unmarshal(install.Body.Bytes(), &installed))
		assert.False(t, installed.Data.Authorized)
		require.Equal(t, 200, request("POST", "/api/mcp/self/installations/"+installed.Data.ID+"/enable", "user", "{}", "", "1").Code)
		var relayCalls atomic.Int32
		engine.POST("/v1/chat/completions", func(c *gin.Context) {
			if relayCalls.Add(1) == 1 {
				c.Data(200, "application/json", []byte(`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call1","type":"function","function":{"name":"mcp_tool_1","arguments":"{}"}}]}}]}`))
				return
			}
			raw, err := c.GetRawData()
			assert.NoError(t, err)
			assert.Contains(t, string(raw), "tool says hello")
			c.Data(200, "application/json", []byte(`{"choices":[{"message":{"role":"assistant","content":"hello"}}]}`))
		})
		body, err = common.Marshal(map[string]any{"model": "deepseek-flash", "mcp_installation_id": installed.Data.ID, "messages": []map[string]string{{"role": "user", "content": "Say hello"}}})
		require.NoError(t, err)
		for range 2 {
			r := httptest.NewRequest("POST", "/v1/mcp/test/chat/completions", strings.NewReader(string(body)))
			r.Header.Set("Authorization", "Bearer sk-"+token.Key)
			r.Header.Set("Idempotency-Key", "catalog-test")
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, r)
			require.Equal(t, 200, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), `"status":"completed"`)
		}
		assert.EqualValues(t, 1, toolCalls.Load())
		assert.EqualValues(t, 2, relayCalls.Load())
		var calls []model.MCPInvocation
		require.NoError(t, model.DB.Where("server_id = ?", server.ID).Find(&calls).Error)
		require.Len(t, calls, 1)
		assert.Equal(t, "settled", calls[0].BillingStatus)
		require.Positive(t, calls[0].ChargedQuota)
		require.NoError(t, model.DB.First(&user, user.Id).Error)
		assert.Equal(t, 10000-calls[0].ChargedQuota, user.Quota)
		require.Equal(t, 200, request("DELETE", "/api/mcp/self/installations/"+installed.Data.ID+"/authorization", "user", "", "", "1").Code)
		require.Equal(t, 403, request("GET", "/api/mcp/self/servers/"+server.ID+"/test-tools", "user", "", "", "").Code)
		require.Equal(t, 200, request("DELETE", "/api/mcp/self/installations/"+installed.Data.ID, "user", "", "", "1").Code)
		r := httptest.NewRequest("POST", "/mcp/"+server.ID, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		r.Header.Set("Authorization", "Bearer sk-"+token.Key)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, r)
		assert.Equal(t, 401, w.Code, "relay API keys no longer authenticate the external MCP endpoint")
	})
	t.Run("delegated gateway isolates users and preserves saved grants", func(t *testing.T) {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		require.NoError(t, err)
		var active atomic.Bool
		active.Store(true)
		livePatch := map[string]any{}
		var issuer string
		casdoor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var result any
			switch r.URL.Path {
			case "/.well-known/openid-configuration":
				result = map[string]any{"issuer": issuer, "jwks_uri": issuer + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}}
			case "/keys":
				result = map[string]any{"keys": []any{map[string]any{"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "delegation-test", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}}
			case "/api/login/oauth/introspect":
				id, secret, ok := r.BasicAuth()
				assert.True(t, ok)
				if id == "agent-platform" {
					assert.Equal(t, "other-casdoor-secret", secret)
				} else {
					assert.Equal(t, "known-engine", id)
					assert.Equal(t, "casdoor-app-secret", secret)
				}
				require.NoError(t, r.ParseForm())
				assert.Empty(t, r.Form.Get("resource"), "this is an application grant, not an upstream MCP resource grant")
				claims := jwt.MapClaims{}
				_, err := jwt.ParseWithClaims(r.Form.Get("token"), claims, func(*jwt.Token) (any, error) { return &key.PublicKey, nil }, jwt.WithValidMethods([]string{"RS256"}))
				if !assert.NoError(t, err) {
					http.Error(w, "invalid", 401)
					return
				}
				result = map[string]any{"active": active.Load(), "iss": issuer, "sub": claims["sub"], "aud": claims["aud"], "client_id": id, "exp": claims["exp"], "scope": claims["scope"]}
				maps.Copy(result.(map[string]any), livePatch)
			default:
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			encoded, err := common.Marshal(result)
			assert.NoError(t, err)
			_, _ = w.Write(encoded)
		}))
		defer casdoor.Close()
		issuer = casdoor.URL
		t.Setenv("MCP_CASDOOR_ISSUER", issuer)
		t.Setenv("MCP_CASDOOR_ORGANIZATION", "private-org")
		t.Setenv("MCP_CASDOOR_PROVIDER_ID", "777")
		require.NoError(t, model.DB.Create(&model.CustomOAuthProvider{Id: 777, Name: "Delegation Casdoor", Slug: "delegation-casdoor", Enabled: true, UserIdField: "sub", WellKnown: issuer + "/.well-known/openid-configuration"}).Error)
		managementKey := "management-delegated-alice"
		for _, user := range []model.User{
			{Id: 141, Username: "delegated-alice", AffCode: "delegated-alice", AccessToken: &managementKey, Role: common.RoleAdminUser, Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, Quota: 10000},
			{Id: 142, Username: "delegated-bob", AffCode: "delegated-bob", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, Quota: 10000},
		} {
			require.NoError(t, model.DB.Create(&user).Error)
			require.NoError(t, model.DB.Create(&model.UserOAuthBinding{UserId: user.Id, ProviderId: 777, ProviderUserId: user.Username}).Error)
		}
		var calls atomic.Int32
		var fail atomic.Bool
		upstream := sdk.NewServer(&sdk.Implementation{Name: "authorized-upstream", Version: "1"}, nil)
		upstream.AddTool(&sdk.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			calls.Add(1)
			return &sdk.CallToolResult{IsError: fail.Load(), Content: []sdk.Content{&sdk.TextContent{Text: "private alice result"}}}, nil
		})
		handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return upstream }, nil)
		endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "Bearer saved-upstream-authorization", r.Header.Get("Authorization"), "only the existing upstream grant may be forwarded")
			handler.ServeHTTP(w, r)
		}))
		defer endpoint.Close()
		t.Setenv("MCP_ALLOWED_UPSTREAM_ORIGINS", issuer+","+endpoint.URL)
		servers := make([]*model.MCPServer, 0, 2)
		grants := make([]model.MCPInstallation, 0, 2)
		for _, name := range []string{"Delegated One", "Delegated Two"} {
			server, err := mcpsvc.CreateServer(mcpsvc.ServerInput{Name: name, Icon: "OpenAI", Readme: "Delegation fixture", AuthMode: "oauth2", Connection: model.MCPConnection{URL: endpoint.URL, Transport: "streamable_http", ClientID: "upstream-client"}})
			require.NoError(t, err)
			require.NoError(t, model.DB.Create(&model.MCPTool{ID: server.ID, ServerID: server.ID, Name: "echo", Enabled: true, InputSchema: model.MCPJSON{"type": "object"}, PriceMode: "per_success", UnitPriceUSD: "0.001", Revision: 1, PriceRevision: 1}).Error)
			server, err = mcpsvc.SetServerStatus(server.ID, server.Revision, true)
			require.NoError(t, err)
			shared, err := mcpsvc.Install(server.ID, 141, true)
			require.NoError(t, err)
			data, err := common.Marshal(map[string]any{"Token": oauth2.Token{AccessToken: "saved-upstream-authorization", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}})
			require.NoError(t, err)
			sealed, err := mcpsvc.Seal(string(data), "oauth-grant:"+shared.ID)
			require.NoError(t, err)
			require.NoError(t, model.DB.Model(shared).Updates(map[string]any{"authorized": true, "connection_revision": server.ConnectionRevision, "grant_ciphertext": sealed}).Error)
			require.NoError(t, model.DB.First(shared, "id = ?", shared.ID).Error)
			grants = append(grants, *shared)
			row, err := mcpsvc.Install(server.ID, 141, false)
			require.NoError(t, err)
			require.NoError(t, model.DB.Model(row).Updates(map[string]any{"authorized": true, "connection_revision": server.ConnectionRevision}).Error)
			servers = append(servers, server)
		}
		configFile := filepath.Join(t.TempDir(), "trusted-mcp-apps.json")
		app := map[string]any{"enabled": true, "exchange_secret_sha256": mcpsvc.Digest("exchange-secret"), "casdoor_client_secret": "casdoor-app-secret", "server_ids": []string{servers[0].ID, servers[1].ID}, "max_charge_quota": 10000}
		otherApp := maps.Clone(app)
		otherApp["exchange_secret_sha256"] = mcpsvc.Digest("other-exchange-secret")
		otherApp["casdoor_client_secret"] = "other-casdoor-secret"
		writeApp := func() {
			data, err := common.Marshal(map[string]any{"known-engine": app, "agent-platform": otherApp})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(configFile, data, 0600))
		}
		writeApp()
		t.Setenv("MCP_TRUSTED_APPS_FILE", configFile)
		base := jwt.MapClaims{"iss": issuer, "sub": "delegated-alice", "aud": "known-engine", "azp": "known-engine", "owner": "private-org", "scope": "openid system.login mcp:gateway", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}
		sign := func(patch jwt.MapClaims) string {
			claims := maps.Clone(base)
			maps.Copy(claims, patch)
			token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
			token.Header["kid"] = "delegation-test"
			raw, err := token.SignedString(key)
			require.NoError(t, err)
			return raw
		}
		userToken := sign(nil)
		exchange := func(client, secret, raw string) *httptest.ResponseRecorder {
			data, err := common.Marshal(map[string]string{"access_token": raw})
			require.NoError(t, err)
			r := httptest.NewRequest("POST", "/v1/auth/app-exchange", strings.NewReader(string(data)))
			r.SetBasicAuth(client, secret)
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, r)
			return w
		}
		for _, tc := range []struct {
			name, client, secret string
			patch                jwt.MapClaims
		}{
			{"wrong secret", "known-engine", "wrong", nil},
			{"unknown app", "other", "exchange-secret", nil},
			{"audience", "known-engine", "exchange-secret", jwt.MapClaims{"aud": "other"}},
			{"multiple audiences", "known-engine", "exchange-secret", jwt.MapClaims{"aud": []string{"known-engine", "other"}}},
			{"issuer", "known-engine", "exchange-secret", jwt.MapClaims{"iss": "https://other.test"}},
			{"organization", "known-engine", "exchange-secret", jwt.MapClaims{"owner": "other"}},
			{"scope", "known-engine", "exchange-secret", jwt.MapClaims{"scope": "openid system.login"}},
			{"party", "known-engine", "exchange-secret", jwt.MapClaims{"azp": "other"}},
			{"expired", "known-engine", "exchange-secret", jwt.MapClaims{"exp": time.Now().Add(-time.Hour).Unix()}},
			{"future", "known-engine", "exchange-secret", jwt.MapClaims{"nbf": time.Now().Add(time.Hour).Unix()}},
			{"unlinked", "known-engine", "exchange-secret", jwt.MapClaims{"sub": "unlinked"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				w := exchange(tc.client, tc.secret, sign(tc.patch))
				require.True(t, w.Code == 401 || w.Code == 403, w.Body.String())
			})
		}
		for _, patch := range []map[string]any{{"active": false}, {"iss": "other"}, {"aud": "other"}, {"sub": "other"}, {"client_id": "other"}, {"scope": "system.login"}, {"exp": time.Now().Add(-time.Hour).Unix()}} {
			livePatch = patch
			require.Equal(t, 401, exchange("known-engine", "exchange-secret", userToken).Code)
		}
		livePatch = map[string]any{}
		for _, raw := range []string{"sk-mcptesttoken", "management-user", "not-a-jwt"} {
			require.Equal(t, 401, exchange("known-engine", "exchange-secret", raw).Code)
		}
		otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
		require.NoError(t, err)
		forged := jwt.NewWithClaims(jwt.SigningMethodRS256, base)
		forged.Header["kid"] = "delegation-test"
		forgedToken, err := forged.SignedString(otherKey)
		require.NoError(t, err)
		require.Equal(t, 401, exchange("known-engine", "exchange-secret", forgedToken).Code)
		w := exchange("known-engine", "exchange-secret", userToken)
		require.Equal(t, 200, w.Code, w.Body.String())
		assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
		var token mcpsvc.MCPExchangeResult
		require.NoError(t, common.Unmarshal(w.Body.Bytes(), &token))
		assert.Equal(t, "Bearer", token.TokenType)
		assert.Positive(t, token.ExpiresIn)
		assert.LessOrEqual(t, token.ExpiresIn, int64(300))
		assert.NotContains(t, token.AccessToken, userToken)
		mcpClaims := jwt.MapClaims{}
		parsedJWT, _, err := jwt.NewParser().ParseUnverified(token.AccessToken, mcpClaims)
		require.NoError(t, err, "agent backends can read standard JWT expiry claims")
		assert.Equal(t, "HS256", parsedJWT.Header["alg"])
		assert.Equal(t, "at+jwt", parsedJWT.Header["typ"])
		assert.Equal(t, "https://gateway.test", mcpClaims["iss"])
		assert.Equal(t, []any{"https://gateway.test/mcp/gateway"}, mcpClaims["aud"])
		assert.Equal(t, "141", mcpClaims["sub"])
		assert.Equal(t, "known-engine", mcpClaims["azp"])
		assert.Equal(t, "mcp_delegation", mcpClaims["token_use"])
		assert.Equal(t, "mcp:tools:read mcp:tools:call", mcpClaims["scope"])
		assert.EqualValues(t, token.ExpiresAt, mcpClaims["exp"])
		assert.EqualValues(t, token.ExpiresIn, token.ExpiresAt-int64(mcpClaims["iat"].(float64)))
		assert.NotEmpty(t, mcpClaims["jti"])
		payload, err := base64.RawURLEncoding.DecodeString(strings.Split(token.AccessToken, ".")[1])
		require.NoError(t, err)
		for _, secret := range []string{userToken, "exchange-secret", "casdoor-app-secret", "saved-upstream-authorization", "upstream_token"} {
			assert.NotContains(t, string(payload), secret, "readable JWT claims must not expose upstream credentials")
		}
		// Fixture-only signing material allows exercising claim checks independently
		// of the signature check; caller applications never receive this key.
		mac := hmac.New(sha256.New, make([]byte, 32))
		_, _ = mac.Write([]byte("new-api/mcp-delegation-jwt/v1"))
		jwtKey := mac.Sum(nil)
		signMCP := func(patch jwt.MapClaims) string {
			claims := maps.Clone(mcpClaims)
			maps.Copy(claims, patch)
			value := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
			value.Header["typ"] = "at+jwt"
			raw, err := value.SignedString(jwtKey)
			require.NoError(t, err)
			return raw
		}
		short := exchange("known-engine", "exchange-secret", sign(jwt.MapClaims{"exp": time.Now().Add(60 * time.Second).Unix()}))
		require.Equal(t, 200, short.Code)
		var shorter mcpsvc.MCPExchangeResult
		require.NoError(t, common.Unmarshal(short.Body.Bytes(), &shorter))
		assert.LessOrEqual(t, shorter.ExpiresIn, int64(60))
		invoke := func(credential, path, body, idempotency string) *httptest.ResponseRecorder {
			method := "POST"
			if body == "" {
				method = "GET"
			}
			r := httptest.NewRequest(method, path, strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+credential)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Accept", "application/json, text/event-stream")
			r.Header.Set("MCP-Protocol-Version", "2025-06-18")
			r.Header.Set("Idempotency-Key", idempotency)
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, r)
			return w
		}
		for _, path := range []string{"/mcp/gateway", "/mcp/" + servers[0].ID, "/api/mcp/self/config"} {
			for _, bad := range []string{"sk-mcptesttoken", "management-user", userToken, "mcpd_old-token", token.AccessToken + "tampered"} {
				require.Equal(t, 401, invoke(bad, path, "", "").Code)
			}
		}
		originRequest := httptest.NewRequest("GET", "/api/mcp/self/config", nil)
		originRequest.Header.Set("Authorization", "Bearer "+token.AccessToken)
		originRequest.Header.Set("Origin", "https://untrusted.test")
		originResponse := httptest.NewRecorder()
		engine.ServeHTTP(originResponse, originRequest)
		assert.Equal(t, 403, originResponse.Code)
		assert.Equal(t, 401, invoke(token.AccessToken, "/api/mcp/catalog", "", "").Code, "MCP delegation cannot authenticate dashboard APIs")
		info := request("GET", "/api/mcp/servers/"+servers[0].ID+"/connection-info", "root", "", "", "")
		require.Equal(t, 200, info.Code)
		assert.Contains(t, info.Body.String(), `"auth_mode":"mcp_delegation"`)
		assert.Contains(t, info.Body.String(), "/mcp/gateway")
		otherUserToken := sign(jwt.MapClaims{"aud": "agent-platform", "azp": "agent-platform"})
		otherExchange := exchange("agent-platform", "other-exchange-secret", otherUserToken)
		require.Equal(t, 200, otherExchange.Code, otherExchange.Body.String())
		var otherToken mcpsvc.MCPExchangeResult
		require.NoError(t, common.Unmarshal(otherExchange.Body.Bytes(), &otherToken))
		otherClaims := jwt.MapClaims{}
		_, _, err = jwt.NewParser().ParseUnverified(otherToken.AccessToken, otherClaims)
		require.NoError(t, err)
		assert.Equal(t, "agent-platform", otherClaims["azp"])
		assert.Equal(t, "141", otherClaims["sub"])
		assert.Equal(t, 200, invoke(otherToken.AccessToken, "/api/mcp/self/config", "", "").Code)
		config := invoke(token.AccessToken, "/api/mcp/self/config", "", "")
		require.Equal(t, 200, config.Code, config.Body.String())
		assert.Contains(t, config.Body.String(), servers[0].ID)
		assert.Contains(t, config.Body.String(), servers[1].ID)
		list := invoke(token.AccessToken, "/mcp/gateway", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "")
		require.Equal(t, 200, list.Code, list.Body.String())
		var catalog struct {
			Result struct {
				Tools []struct{ Name, Description string }
			}
		}
		require.NoError(t, common.Unmarshal(mcpRPCReply(t, list.Body.String()), &catalog))
		require.Len(t, catalog.Result.Tools, 2)
		assert.NotEqual(t, catalog.Result.Tools[0].Name, catalog.Result.Tools[1].Name)
		callBody := fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":%q,"arguments":{}}}`, catalog.Result.Tools[0].Name)
		for range 2 {
			reply := invoke(token.AccessToken, "/mcp/gateway", callBody, "delegate-once")
			require.Equal(t, 200, reply.Code, reply.Body.String())
			require.Contains(t, reply.Body.String(), "private alice result")
		}
		// Exercise initialization and invocation through an actual MCP SDK client.
		gatewayHTTP := httptest.NewServer(engine)
		defer gatewayHTTP.Close()
		client := &http.Client{Timeout: 10 * time.Second, Transport: &mcpDelegationTransport{token: token.AccessToken}}
		session, err := sdk.NewClient(&sdk.Implementation{Name: "agent-fixture", Version: "1"}, nil).Connect(t.Context(), &sdk.StreamableClientTransport{Endpoint: gatewayHTTP.URL + "/mcp/gateway", HTTPClient: client, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
		require.NoError(t, err)
		defer session.Close()
		directory, err := session.ListTools(t.Context(), nil)
		require.NoError(t, err)
		require.Len(t, directory.Tools, 2)
		sdkResult, err := session.CallTool(t.Context(), &sdk.CallToolParams{Name: catalog.Result.Tools[0].Name, Arguments: map[string]any{}})
		require.NoError(t, err)
		encodedSDK, _ := common.Marshal(sdkResult)
		assert.False(t, sdkResult.IsError, "%s", encodedSDK)
		assert.EqualValues(t, 1, calls.Load())
		var charged model.MCPInvocation
		require.NoError(t, model.DB.Where("user_id = ?", 141).First(&charged).Error)
		assert.Zero(t, charged.TokenID)
		assert.Equal(t, 141, charged.PayerUserID)
		assert.Equal(t, "settled", charged.BillingStatus)
		assert.Positive(t, charged.ChargedQuota)
		var alice, bob model.User
		require.NoError(t, model.DB.First(&alice, 141).Error)
		require.NoError(t, model.DB.First(&bob, 142).Error)
		assert.Equal(t, 10000-charged.ChargedQuota, alice.Quota)
		assert.Equal(t, 10000, bob.Quota)
		bobExchange := exchange("known-engine", "exchange-secret", sign(jwt.MapClaims{"sub": "delegated-bob"}))
		require.Equal(t, 200, bobExchange.Code)
		var bobToken mcpsvc.MCPExchangeResult
		require.NoError(t, common.Unmarshal(bobExchange.Body.Bytes(), &bobToken))
		bobList := invoke(bobToken.AccessToken, "/mcp/gateway", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "")
		require.Equal(t, 200, bobList.Code, bobList.Body.String())
		assert.NotContains(t, bobList.Body.String(), catalog.Result.Tools[0].Name)
		assert.Equal(t, 404, invoke(bobToken.AccessToken, "/mcp/"+servers[0].ID, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "").Code)
		// Insufficient funds must prevent execution; tool errors refund the reservation.
		require.NoError(t, model.DB.Model(&alice).Update("quota", 0).Error)
		noFunds := invoke(token.AccessToken, "/mcp/gateway", callBody, "no-funds")
		assert.Contains(t, noFunds.Body.String(), "INSUFFICIENT_CREDITS")
		assert.EqualValues(t, 1, calls.Load())
		require.NoError(t, model.DB.Model(&alice).Update("quota", 9000).Error)
		fail.Store(true)
		failed := invoke(token.AccessToken, "/mcp/gateway", callBody, "tool-error")
		assert.Contains(t, failed.Body.String(), `"isError":true`)
		require.NoError(t, model.DB.First(&alice, 141).Error)
		assert.Equal(t, 9000, alice.Quota)
		fail.Store(false)
		for _, tc := range []struct {
			name  string
			patch jwt.MapClaims
		}{
			{"expired", jwt.MapClaims{"exp": time.Now().Add(-time.Minute).Unix()}},
			{"future", jwt.MapClaims{"nbf": time.Now().Add(time.Hour).Unix()}},
			{"issuer", jwt.MapClaims{"iss": "https://other.test"}},
			{"audience", jwt.MapClaims{"aud": "new-api-dashboard"}},
			{"multiple audiences", jwt.MapClaims{"aud": []string{"https://gateway.test/mcp/gateway", "new-api-dashboard"}}},
			{"purpose", jwt.MapClaims{"token_use": "access"}},
			{"scope", jwt.MapClaims{"scope": "mcp:tools:read"}},
			{"different user", jwt.MapClaims{"sub": "142"}},
			{"different app", jwt.MapClaims{"azp": "another-app"}},
			{"missing expiry", jwt.MapClaims{"exp": nil}},
			{"missing issued at", jwt.MapClaims{"iat": nil}},
			{"missing not before", jwt.MapClaims{"nbf": nil}},
			{"missing jti", jwt.MapClaims{"jti": nil}},
			{"tampered grant", jwt.MapClaims{"grant": "invalid-ciphertext"}},
			{"extended lifetime", jwt.MapClaims{"exp": time.Now().Add(time.Hour).Unix()}},
		} {
			t.Run("MCP JWT "+tc.name, func(t *testing.T) {
				assert.Equal(t, 401, invoke(signMCP(tc.patch), "/mcp/gateway", callBody, "invalid-claims").Code)
			})
		}
		for _, method := range []jwt.SigningMethod{jwt.SigningMethodNone, jwt.SigningMethodHS512} {
			value := jwt.NewWithClaims(method, mcpClaims)
			value.Header["typ"] = "at+jwt"
			var key any = jwtKey
			if method == jwt.SigningMethodNone {
				key = jwt.UnsafeAllowNoneSignatureType
			}
			wrongAlgorithm, err := value.SignedString(key)
			require.NoError(t, err)
			assert.Equal(t, 401, invoke(wrongAlgorithm, "/mcp/gateway", callBody, "wrong-algorithm").Code)
		}
		wrongType := jwt.NewWithClaims(jwt.SigningMethodHS256, mcpClaims)
		wrongTypeToken, err := wrongType.SignedString(jwtKey)
		require.NoError(t, err)
		assert.Equal(t, 401, invoke(wrongTypeToken, "/mcp/gateway", callBody, "wrong-type").Code)

		active.Store(false)
		assert.Equal(t, 401, invoke(token.AccessToken, "/mcp/gateway", callBody, "revoked").Code)
		active.Store(true)
		require.NoError(t, model.DB.Model(&alice).Update("auth_version", 2).Error)
		assert.Equal(t, 401, invoke(token.AccessToken, "/api/mcp/self/config", "", "").Code)
		require.NoError(t, model.DB.Model(&alice).Update("auth_version", 1).Error)
		require.NoError(t, model.DB.Model(&alice).Update("status", common.UserStatusDisabled).Error)
		assert.Equal(t, 403, invoke(token.AccessToken, "/api/mcp/self/config", "", "").Code)
		require.NoError(t, model.DB.Model(&alice).Update("status", common.UserStatusEnabled).Error)
		app["server_ids"] = []string{servers[1].ID}
		writeApp()
		assert.Equal(t, 401, invoke(token.AccessToken, "/mcp/gateway", callBody, "changed-app").Code)
		restricted := exchange("known-engine", "exchange-secret", userToken)
		require.Equal(t, 200, restricted.Code)
		var restrictedToken mcpsvc.MCPExchangeResult
		require.NoError(t, common.Unmarshal(restricted.Body.Bytes(), &restrictedToken))
		assert.Equal(t, 403, invoke(restrictedToken.AccessToken, "/mcp/"+servers[0].ID, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "").Code)
		app["max_charge_quota"] = 1
		writeApp()
		budgetExchange := exchange("known-engine", "exchange-secret", userToken)
		require.Equal(t, 200, budgetExchange.Code)
		var budgetToken mcpsvc.MCPExchangeResult
		require.NoError(t, common.Unmarshal(budgetExchange.Body.Bytes(), &budgetToken))
		budgetBody := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{}}}`
		budgetReply := invoke(budgetToken.AccessToken, "/mcp/"+servers[1].ID, budgetBody, "budget")
		assert.Contains(t, budgetReply.Body.String(), "DELEGATION_BUDGET_EXCEEDED")
		app["max_charge_quota"] = 10000
		app["enabled"] = false
		writeApp()
		assert.Equal(t, 401, invoke(restrictedToken.AccessToken, "/mcp/gateway", callBody, "disabled-app").Code)
		app["enabled"] = true
		app["server_ids"] = []string{servers[0].ID, servers[1].ID}
		writeApp()
		// User disconnect takes effect even while the delegation token remains valid.
		row, err := mcpsvc.FindInstallation(servers[0].ID, 141, false)
		require.NoError(t, err)
		require.NoError(t, model.DB.Model(row).Update("authorized", false).Error)
		afterDisconnect := invoke(token.AccessToken, "/api/mcp/self/config", "", "")
		require.Equal(t, 200, afterDisconnect.Code)
		assert.NotContains(t, afterDisconnect.Body.String(), servers[0].ID)
		for _, before := range grants {
			var after model.MCPInstallation
			require.NoError(t, model.DB.First(&after, "id = ?", before.ID).Error)
			assert.Equal(t, before.Grant, after.Grant, "delegation must not overwrite upstream grants")
			tools := request("GET", "/api/mcp/servers/"+before.ServerID+"/test-tools", "delegated-alice", "", "", "")
			require.Equal(t, 200, tools.Code, tools.Body.String())
			assert.Contains(t, tools.Body.String(), "echo", "management testing keeps using the saved upstream grant")
			connection := request("POST", "/api/mcp/servers/"+before.ServerID+"/connection-tests", "delegated-alice", "{}", "", "1")
			require.Equal(t, 200, connection.Code, connection.Body.String())
		}
		audits, _, err := model.GetAuditLogs(model.AuditLogFilter{Category: model.AuditCategorySecurity}, 0, 1000, common.RoleRootUser)
		require.NoError(t, err)
		require.NotEmpty(t, audits)
		encoded, err := common.Marshal(audits)
		require.NoError(t, err)
		assert.Contains(t, string(encoded), "mcp.app_exchange")
		assert.Contains(t, string(encoded), "mcp.gateway")
		for _, secret := range []string{userToken, token.AccessToken, otherUserToken, otherToken.AccessToken, "exchange-secret", "casdoor-app-secret", "other-exchange-secret", "other-casdoor-secret", "saved-upstream-authorization"} {
			assert.NotContains(t, string(encoded), secret)
		}
	})

	t.Setenv("MCP_ENABLED", "false")
	assert.Equal(t, 404, request("GET", "/api/mcp/servers", "root", "", "", "").Code)
	assert.Equal(t, 404, request("POST", "/mcp/remote-test", "root", `{}`, "", "1").Code)
}

// mcpRPCReply accepts both Streamable HTTP response encodings.
func mcpRPCReply(t *testing.T, body string) []byte {
	t.Helper()
	for line := range strings.SplitSeq(body, "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			return []byte(data)
		}
	}
	return []byte(body)
}

// mcpDelegationTransport supplies the agent backend's MCP JWT bearer credential.
type mcpDelegationTransport struct{ token string }

func (transport *mcpDelegationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	request.Header.Set("Authorization", "Bearer "+transport.token)
	request.Header.Set("Idempotency-Key", "delegate-once")
	return http.DefaultTransport.RoundTrip(request)
}
