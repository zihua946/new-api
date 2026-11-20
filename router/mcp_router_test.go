package router

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service/authz"
	mcpsvc "github.com/QuantumNous/new-api/service/mcp"
	"github.com/gin-gonic/gin"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCPManagementHTTPContract(t *testing.T) {
	oldDB, oldLogDB := model.DB, model.LOG_DB
	oldMaster, oldRedis, oldRate, oldSQLite := common.IsMasterNode, common.RedisEnabled, common.GlobalApiRateLimitEnable, common.SQLitePath
	t.Setenv("SQL_DSN", "")
	t.Setenv("LOG_SQL_DSN", "")
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
		assert.Equal(t, 404, w.Code)
	})
	t.Setenv("MCP_ENABLED", "false")
	assert.Equal(t, 404, request("GET", "/api/mcp/servers", "root", "", "", "").Code)
	assert.Equal(t, 404, request("POST", "/mcp/remote-test", "root", `{}`, "", "1").Code)
}
