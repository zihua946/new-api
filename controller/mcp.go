package controller

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service/authz"
	mcpsvc "github.com/QuantumNous/new-api/service/mcp"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func mcpResponse(c *gin.Context, status int, data any, err error) {
	if err != nil {
		failure := mcpsvc.ErrorResponse(err)
		c.JSON(failure.Status, gin.H{"success": false, "message": failure.Message, "code": failure.Code, "request_id": c.GetString(common.RequestIdKey)})
		return
	}
	c.JSON(status, gin.H{"success": true, "message": "", "data": data})
}

func mcpDecode(c *gin.Context, target any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, mcpsvc.MaxArgumentBytes))
	if err != nil || common.Unmarshal(body, target) != nil {
		mcpResponse(c, 0, nil, mcpsvc.Failure(422, "INVALID_REQUEST", "A valid, bounded JSON request is required"))
		return false
	}
	switch target.(type) {
	case *mcpsvc.ServerInput, *mcpsvc.ServerPatch:
		var fields struct {
			Limits struct {
				TimeoutSeconds *int `json:"timeout_seconds"`
			} `json:"limits"`
		}
		if common.Unmarshal(body, &fields) != nil || fields.Limits.TimeoutSeconds != nil && *fields.Limits.TimeoutSeconds == 0 {
			mcpResponse(c, 0, nil, mcpsvc.Failure(422, "INVALID_LIMITS", "Timeout must be between 1 and 600 seconds"))
			return false
		}
	}
	return true
}

func mcpSessionID(c *gin.Context) string {
	identity, ok := middleware.GetAuthIdentity(c)
	if !ok {
		return ""
	}
	return identity.SessionID
}

func mcpPage(c *gin.Context) (int, int, error) {
	page, err := strconv.Atoi(c.DefaultQuery("p", "1"))
	if err != nil || page < 1 || page > 1000000 {
		return 0, 0, mcpsvc.Failure(422, "INVALID_PAGE", "Page is invalid")
	}
	size, err := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if err != nil || size < 1 || size > 100 {
		return 0, 0, mcpsvc.Failure(422, "INVALID_PAGE", "Page size must be between 1 and 100")
	}
	return page, size, nil
}

func MCPServers(c *gin.Context) {
	if c.Request.Method == http.MethodPost {
		var input mcpsvc.ServerInput
		if !mcpDecode(c, &input) {
			return
		}
		input.Kind = ""
		server, err := mcpsvc.CreateServer(input)
		mcpResponse(c, 201, server, err)
		return
	}
	page, size, err := mcpPage(c)
	if err != nil {
		mcpResponse(c, 0, nil, err)
		return
	}
	query := model.DB.Model(&model.MCPServer{}).Where("archived = ? AND catalog = ?", false, true)
	if name := c.Query("name"); name != "" {
		query = query.Where("name LIKE ?", "%"+name+"%")
	}
	if kind := c.Query("kind"); kind != "" {
		query = query.Where("kind = ?", kind)
	}
	if enabled := c.Query("enabled"); enabled != "" {
		if enabled != "true" && enabled != "false" {
			mcpResponse(c, 0, nil, mcpsvc.Failure(422, "INVALID_FILTER", "Enabled filter is invalid"))
			return
		}
		query = query.Where("enabled = ?", enabled == "true")
	}
	var total int64
	var servers []model.MCPServer
	if err = query.Count(&total).Error; err == nil {
		err = query.Omit("Readme").Order("created_at DESC").Offset((page - 1) * size).Limit(size).Find(&servers).Error
	}
	for i := range servers {
		servers[i].CredentialsConfigured = servers[i].CredentialCiphertext != "" || servers[i].RuntimeSecrets != ""
	}
	mcpResponse(c, 200, gin.H{"items": servers, "page": page, "page_size": size, "total": total}, err)
}

func MCPServer(c *gin.Context) {
	id := c.Param("id")
	switch c.Request.Method {
	case http.MethodPatch:
		var input mcpsvc.ServerPatch
		if !mcpDecode(c, &input) {
			return
		}
		if _, err := mcpsvc.PatchServer(id, input, authz.Can(c.GetInt("id"), c.GetInt("role"), authz.MCPSensitiveWrite)); err != nil {
			mcpResponse(c, 0, nil, err)
			return
		}
	case http.MethodDelete:
		var input struct {
			ExpectedRevision int64 `json:"expected_revision"`
		}
		if !mcpDecode(c, &input) {
			return
		}
		err := model.ArchiveMCPServer(id, input.ExpectedRevision)
		mcpResponse(c, 200, gin.H{"archived": err == nil}, err)
		return
	}
	server, err := model.GetMCPServer(id)
	if err == nil {
		server.OAuthCallbackURL = mcpsvc.PublicBaseURL() + "/api/mcp/oauth/callback"
	}
	mcpResponse(c, 200, server, err)
}

func MCPServerStatus(c *gin.Context) {
	var input struct {
		Enabled          *bool `json:"enabled"`
		ExpectedRevision int64 `json:"expected_revision"`
	}
	if !mcpDecode(c, &input) {
		return
	}
	if input.Enabled == nil {
		mcpResponse(c, 0, nil, mcpsvc.Failure(422, "INVALID_REQUEST", "Enabled is required"))
		return
	}
	_, err := mcpsvc.SetServerStatusForUser(c.Request.Context(), c.Param("id"), input.ExpectedRevision, *input.Enabled, c.GetInt("id"))
	if err != nil {
		mcpResponse(c, 0, nil, err)
		return
	}
	server, err := model.GetMCPServer(c.Param("id"))
	mcpResponse(c, 200, server, err)
}

func MCPCredentials(c *gin.Context) {
	var input mcpsvc.CredentialsInput
	if !mcpDecode(c, &input) {
		return
	}
	_, err := mcpsvc.SetCredentials(c.Param("id"), input)
	if err != nil {
		mcpResponse(c, 0, nil, err)
		return
	}
	server, err := model.GetMCPServer(c.Param("id"))
	mcpResponse(c, 200, server, err)
}

func MCPChannelOptions(c *gin.Context) {
	if c.Query("kind") != "deepseek" {
		mcpResponse(c, 0, nil, mcpsvc.Failure(422, "INVALID_KIND", "Only DeepSeek channel references are supported"))
		return
	}
	var channels []struct {
		ID      int     `json:"id"`
		Name    string  `json:"name"`
		BaseURL *string `json:"base_url"`
		Status  int     `json:"status"`
	}
	err := model.DB.Model(&model.Channel{}).Select("id", "name", "base_url", "status").Where("type = ?", constant.ChannelTypeDeepSeek).Order("id").Limit(1000).Find(&channels).Error
	mcpResponse(c, 200, channels, err)
}

func MCPConnectionInfo(c *gin.Context) {
	server, err := model.GetMCPServer(c.Param("id"))
	if err != nil {
		mcpResponse(c, 0, nil, err)
		return
	}
	if server.Catalog {
		mcpResponse(c, 200, gin.H{"url": mcpsvc.PublicBaseURL() + "/mcp/" + server.ID, "gateway_url": mcpsvc.PublicBaseURL() + "/mcp/gateway", "app_exchange_url": mcpsvc.PublicBaseURL() + "/v1/auth/app-exchange", "transport": "streamable_http", "auth_mode": "mcp_delegation", "enabled": server.Enabled}, nil)
		return
	}
	mcpResponse(c, 200, gin.H{"url": mcpsvc.ResourceURL(server), "transport": "streamable_http", "auth_mode": server.AuthMode, "enabled": server.Enabled, "metadata_url": mcpsvc.PublicBaseURL() + "/.well-known/oauth-protected-resource/mcp/" + server.Slug}, nil)
}

func MCPTools(c *gin.Context) {
	if _, err := model.GetMCPServer(c.Param("id")); err != nil {
		mcpResponse(c, 0, nil, err)
		return
	}
	tools, err := mcpsvc.ListTools(c.Param("id"))
	mcpResponse(c, 200, tools, err)
}

func MCPTool(c *gin.Context) {
	var input mcpsvc.ToolPatch
	if !mcpDecode(c, &input) {
		return
	}
	// Description edits, execution enablement and scope edits have independent permissions.
	if input.Description != nil && !authz.Can(c.GetInt("id"), c.GetInt("role"), authz.MCPWrite) || (input.Enabled != nil || input.AcceptedSchemaHash != "") && !authz.Can(c.GetInt("id"), c.GetInt("role"), authz.MCPOperate) {
		mcpResponse(c, 0, nil, mcpsvc.Failure(403, "PERMISSION_DENIED", "MCP permission is required"))
		return
	}
	err := mcpsvc.PatchTool(c.Param("id"), c.Param("tool_id"), input, authz.Can(c.GetInt("id"), c.GetInt("role"), authz.MCPSensitiveWrite))
	if err != nil {
		mcpResponse(c, 0, nil, err)
		return
	}
	tools, err := mcpsvc.ListTools(c.Param("id"))
	for _, tool := range tools {
		if tool.ID == c.Param("tool_id") {
			mcpResponse(c, 200, tool, err)
			return
		}
	}
	mcpResponse(c, 0, nil, gorm.ErrRecordNotFound)
}

func MCPToolPrice(c *gin.Context) {
	if !authz.Can(c.GetInt("id"), c.GetInt("role"), authz.MCPWrite) && !authz.Can(c.GetInt("id"), c.GetInt("role"), authz.MCPSensitiveWrite) {
		mcpResponse(c, 0, nil, mcpsvc.Failure(403, "PERMISSION_DENIED", "MCP editing permission is required"))
		return
	}
	var input struct {
		ExpectedRevision int64   `json:"expected_revision"`
		Mode             string  `json:"mode"`
		UnitPriceUSD     string  `json:"unit_price_usd"`
		UnitPrice        *string `json:"unit_price"`
		Currency         string  `json:"currency"`
	}
	if !mcpDecode(c, &input) {
		return
	}
	if input.UnitPrice != nil {
		input.UnitPriceUSD = *input.UnitPrice
	} else if input.Currency == "" {
		input.Currency = "USD"
	}
	price, err := mcpsvc.SetToolPrice(c.Param("id"), c.Param("tool_id"), input.ExpectedRevision, input.Mode, input.UnitPriceUSD, input.Currency)
	mcpResponse(c, 200, price, err)
}

func MCPDiscovery(c *gin.Context) {
	data, err := mcpsvc.DiscoverTools(c.Request.Context(), c.Param("id"), c.GetInt("id"))
	mcpResponse(c, 201, data, err)
}

func MCPToolSync(c *gin.Context) {
	var input struct {
		DiscoveryID      string `json:"discovery_id"`
		ExpectedRevision int64  `json:"expected_revision"`
	}
	if !mcpDecode(c, &input) {
		return
	}
	err := mcpsvc.SyncTools(c.Param("id"), input.DiscoveryID, c.GetInt("id"), input.ExpectedRevision)
	mcpResponse(c, 200, gin.H{"synced": err == nil}, err)
}

func MCPConnectionTest(c *gin.Context) {
	data, err := mcpsvc.TestConnection(c.Request.Context(), c.Param("id"), c.GetInt("id"), mcpSessionID(c))
	mcpResponse(c, 200, data, err)
}

func MCPTestAuthorization(c *gin.Context) {
	sessionID, userID, serverID := mcpSessionID(c), c.GetInt("id"), c.Param("id")
	switch c.Request.Method {
	case http.MethodPost:
		data, err := mcpsvc.StartTestAuthorization(c.Request.Context(), serverID, userID, sessionID)
		mcpResponse(c, 201, data, err)
	case http.MethodDelete:
		if sessionID == "" {
			mcpResponse(c, 0, nil, mcpsvc.Failure(401, "BROWSER_SESSION_REQUIRED", "A browser session is required"))
			return
		}
		err := model.DB.Where("server_id = ? AND user_id = ? AND session_hash = ?", serverID, userID, mcpsvc.Digest(sessionID)).Delete(&model.MCPTestAuthorization{}).Error
		mcpResponse(c, 200, gin.H{"authorized": false}, err)
	default:
		auth, err := mcpsvc.GetTestAuthorization(serverID, userID, sessionID)
		if err == gorm.ErrRecordNotFound {
			mcpResponse(c, 200, gin.H{"authorized": false}, nil)
			return
		}
		mcpResponse(c, 200, gin.H{"authorized": err == nil, "authorization": auth}, err)
	}
}

func MCPTestAuthorizationCallback(c *gin.Context) {
	// Navigation from Casdoor has no dashboard Bearer header. Carry only the
	// one-use authorization response in a fragment; the authenticated SPA then
	// redeems it through POST, binding it to the original dashboard session.
	if c.Request.Method == http.MethodGet {
		state, code := c.Query("state"), c.Query("code")
		if state == "" || code == "" || len(state) > 256 || len(code) > 4096 {
			mcpResponse(c, 0, nil, mcpsvc.Failure(400, "INVALID_AUTHORIZATION_CALLBACK", "Authorization callback is invalid"))
			return
		}
		c.Header("Cache-Control", "no-store")
		c.Header("Referrer-Policy", "no-referrer")
		c.Redirect(http.StatusSeeOther, "/mcp#"+url.Values{"mcp_state": {state}, "mcp_code": {code}}.Encode())
		return
	}
	var input struct {
		State string `json:"state"`
		Code  string `json:"code"`
	}
	if !mcpDecode(c, &input) {
		return
	}
	serverID, err := mcpsvc.FinishTestAuthorization(c.Request.Context(), c.GetInt("id"), mcpSessionID(c), input.State, input.Code)
	mcpResponse(c, 200, gin.H{"server_id": serverID}, err)
}

func MCPTestInvocation(c *gin.Context) {
	server, err := model.GetMCPServer(c.Param("id"))
	if err != nil {
		mcpResponse(c, 0, nil, err)
		return
	}
	var input struct {
		ToolName             string         `json:"tool_name"`
		Arguments            map[string]any `json:"arguments"`
		ExpectedToolRevision int64          `json:"expected_tool_revision"`
		MaxChargeQuota       *int           `json:"max_charge_quota"`
	}
	if !mcpDecode(c, &input) {
		return
	}
	actor, err := mcpTestingActor(c, server)
	if err != nil {
		mcpResponse(c, 0, nil, err)
		return
	}
	options := mcpsvc.InvocationOptions{
		RequestID:            c.GetString(common.RequestIdKey),
		IdempotencyKey:       c.GetHeader("Idempotency-Key"),
		Test:                 !strings.Contains(c.FullPath(), "/self/"),
		ConfirmPrice:         true,
		ExpectedToolRevision: input.ExpectedToolRevision,
		MaxChargeQuota:       input.MaxChargeQuota,
	}
	call, err := mcpsvc.PrepareCall(server.ID, input.ToolName, actor, input.Arguments, options)
	if err != nil {
		mcpResponse(c, 0, nil, err)
		return
	}
	response := gin.H{"invocation_id": call.Invocation.ID, "status": call.Invocation.Status}
	if strings.Contains(c.GetHeader("Accept"), "text/event-stream") {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-store")
		c.Header("X-Accel-Buffering", "no")
		// Keep the durable invocation running if the browser disconnects. A
		// subsequent result read or idempotent retry never executes it twice.
		send := func(event string, value any) {
			if c.Request.Context().Err() != nil {
				return
			}
			data, err := common.Marshal(value)
			if err != nil {
				return
			}
			if _, err = fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, data); err == nil {
				c.Writer.Flush()
			}
		}
		send("invocation", response)
		if call.Created {
			var count int
			runMCPTest(call, func(message string, details ...map[string]any) error {
				count++
				params := gin.H{"progressToken": call.Invocation.ID, "progress": count, "message": message}
				if len(details) > 0 {
					params["_meta"] = gin.H{"knowyet.com/consultation": details[0]}
				}
				send("message", gin.H{"jsonrpc": "2.0", "method": "notifications/progress", "params": params})
				return nil
			})
		}
		send("complete", response)
		return
	}
	if call.Created {
		go runMCPTest(call, nil)
	}
	mcpResponse(c, 202, response, nil)
}

func runMCPTest(call *mcpsvc.PreparedCall, progress mcpsvc.Progress) {
	defer func() {
		if recover() != nil {
			common.SysError("MCP test worker interrupted; durable recovery will reconcile the invocation")
		}
	}()
	_, _ = call.Run(context.Background(), progress)
}

func MCPTestResult(c *gin.Context) {
	var invocation model.MCPInvocation
	query := model.DB.Where("id = ? AND user_id = ?", c.Param("invocation_id"), c.GetInt("id"))
	admin := !strings.Contains(c.FullPath(), "/self/")
	if admin {
		query = query.Where("is_test = ?", true)
	}
	err := query.First(&invocation).Error
	if err != nil {
		mcpResponse(c, 0, nil, err)
		return
	}
	server, err := model.GetMCPServer(invocation.ServerID)
	if err != nil {
		mcpResponse(c, 0, nil, err)
		return
	}
	actor, err := mcpTestingActor(c, server)
	if err != nil {
		mcpResponse(c, 0, nil, err)
		return
	}
	data := gin.H{"invocation": invocation}
	if invocation.Status == "succeeded" || invocation.Status == "failed" && invocation.ResultCiphertext != "" {
		result, err := mcpsvc.AuthorizedInvocationResult(c.Request.Context(), &invocation, actor, admin)
		if err != nil {
			mcpResponse(c, 0, nil, err)
			return
		}
		data["result"] = result
	}
	mcpResponse(c, 200, data, nil)
}

func MCPInvocations(c *gin.Context) {
	page, size, err := mcpPage(c)
	if err != nil {
		mcpResponse(c, 0, nil, err)
		return
	}
	query := model.DB.Model(&model.MCPInvocation{})
	if strings.Contains(c.FullPath(), "/self/") {
		query = query.Where("user_id = ?", c.GetInt("id"))
	}
	for _, field := range []string{"server_id", "tool_id", "status", "billing_status"} {
		if value := c.Query(field); value != "" {
			query = query.Where(field+" = ?", value)
		}
	}
	var total int64
	var rows []model.MCPInvocation
	if err = query.Count(&total).Error; err == nil {
		err = query.Order("created_at DESC").Offset((page - 1) * size).Limit(size).Find(&rows).Error
	}
	mcpResponse(c, 200, gin.H{"items": rows, "page": page, "page_size": size, "total": total}, err)
}

func MCPInvocation(c *gin.Context) {
	var invocation model.MCPInvocation
	err := model.DB.First(&invocation, "id = ?", c.Param("invocation_id")).Error
	if err != nil {
		mcpResponse(c, 0, nil, err)
		return
	}
	var operations []model.MCPBillingOperation
	err = model.DB.Where("invocation_id = ?", invocation.ID).Order("created_at").Find(&operations).Error
	mcpResponse(c, 200, gin.H{"invocation": invocation, "billing_operations": operations}, err)
}

func MCPUsage(c *gin.Context) {
	until := time.Now().UTC()
	from := until.Add(-30 * 24 * time.Hour)
	var err error
	if raw := c.Query("from"); raw != "" {
		from, err = time.Parse(time.RFC3339, raw)
	}
	if raw := c.Query("until"); raw != "" && err == nil {
		until, err = time.Parse(time.RFC3339, raw)
	}
	if err != nil || !from.Before(until) || until.Sub(from) > 31*24*time.Hour {
		mcpResponse(c, 0, nil, mcpsvc.Failure(422, "INVALID_PERIOD", "Usage period must be at most 31 days"))
		return
	}
	query := model.DB.Model(&model.MCPInvocation{}).Where("created_at >= ? AND created_at < ?", from, until)
	if strings.Contains(c.FullPath(), "/self/") {
		query = query.Where("user_id = ?", c.GetInt("id"))
	}
	if id := c.Query("server_id"); id != "" {
		query = query.Where("server_id = ?", id)
	}
	var rows []struct {
		ServerID     string `json:"server_id"`
		ToolID       string `json:"tool_id"`
		QuotaPerUnit string `json:"quota_per_unit"`
		Calls        int64  `json:"calls"`
		ChargedQuota int64  `json:"charged_quota"`
	}
	err = query.Select("server_id, tool_id, quota_per_unit, COUNT(*) AS calls, SUM(charged_quota) AS charged_quota").Group("server_id, tool_id, quota_per_unit").Scan(&rows).Error
	mcpResponse(c, 200, gin.H{"items": rows, "from": from, "until": until}, err)
}

func MCPProtocol(c *gin.Context) {
	server, err := model.GetMCPServer(c.Param("server_slug"))
	if err != nil || !server.Catalog || !server.Enabled {
		mcpResponse(c, 0, nil, gorm.ErrRecordNotFound)
		return
	}
	if !mcpsvc.AllowedClientOrigin(c.GetHeader("Origin")) {
		mcpResponse(c, 0, nil, mcpsvc.Failure(403, "ORIGIN_NOT_ALLOWED", "Request origin is not allowed"))
		return
	}
	delegated := c.MustGet("mcp_delegated_actor").(*mcpsvc.Actor)
	actor, err := mcpsvc.InstallationActor(c.Request.Context(), server, delegated, false)
	if err != nil {
		mcpResponse(c, 0, nil, err)
		return
	}
	if err = mcpsvc.ServeProtocol(c.Writer, c.Request, server, actor); err != nil && !c.Writer.Written() {
		mcpResponse(c, 0, nil, err)
	}
}

func MCPResourceMetadata(c *gin.Context) {
	server, err := model.GetMCPServerBySlug(c.Param("server_slug"))
	if err != nil || !server.Enabled || server.AuthMode != "casdoor_oauth" {
		mcpResponse(c, 0, nil, gorm.ErrRecordNotFound)
		return
	}
	c.JSON(200, mcpsvc.ProtectedResourceMetadata(server))
}

func MCPInstallPackage(c *gin.Context) {
	var input struct {
		ExpectedRevision int64 `json:"expected_revision"`
	}
	if !mcpDecode(c, &input) {
		return
	}
	server, err := mcpsvc.InstallPackage(c.Request.Context(), c.Param("id"), input.ExpectedRevision)
	mcpResponse(c, 200, server, err)
}

func mcpTestingActor(c *gin.Context, server *model.MCPServer) (*mcpsvc.Actor, error) {
	admin := !strings.Contains(c.FullPath(), "/self/")
	if !admin && (!server.Catalog || !server.Enabled) {
		return nil, gorm.ErrRecordNotFound
	}
	actor, err := mcpsvc.TestActor(c.Request.Context(), server, c.GetInt("id"), mcpSessionID(c))
	if err != nil || !server.Catalog {
		return actor, err
	}
	return mcpsvc.InstallationActor(c.Request.Context(), server, actor, admin)
}
func MCPTestTools(c *gin.Context) {
	server, err := model.GetMCPServer(c.Param("id"))
	if err != nil {
		mcpResponse(c, 0, nil, err)
		return
	}
	actor, err := mcpTestingActor(c, server)
	if err != nil {
		mcpResponse(c, 0, nil, err)
		return
	}
	tools, err := mcpsvc.AvailableTestTools(c.Request.Context(), server, actor, !strings.Contains(c.FullPath(), "/self/"))
	mcpResponse(c, 200, tools, err)
}

func MCPCommandPreview(c *gin.Context) {
	var connection model.MCPConnection
	if !mcpDecode(c, &connection) {
		return
	}
	args, err := mcpsvc.StdioCommand(connection)
	mcpResponse(c, 200, args, err)
}
