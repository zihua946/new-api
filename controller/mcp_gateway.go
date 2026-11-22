package controller

import (
	"context"
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	mcpsvc "github.com/QuantumNous/new-api/service/mcp"
	"github.com/gin-gonic/gin"
)

func MCPAppExchange(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	var actor *mcpsvc.Actor
	defer func() { recordMCPDelegationAudit(c, "mcp.app_exchange", actor) }()
	clientID, secret, ok := c.Request.BasicAuth()
	if !ok || len(clientID) > 256 || len(secret) > 4096 {
		c.Header("WWW-Authenticate", `Basic realm="new-api-mcp"`)
		mcpResponse(c, 0, nil, mcpsvc.Failure(401, "INVALID_APPLICATION", "Application authentication is required"))
		return
	}
	var input struct {
		AccessToken string `json:"access_token"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 40<<10)
	if !mcpDecode(c, &input) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	result, verified, err := mcpsvc.ExchangeMCPApplication(ctx, clientID, secret, input.AccessToken)
	actor = verified
	if err != nil {
		mcpResponse(c, 0, nil, err)
		return
	}
	// Token responses use the same unwrapped contract as known-engine app-exchange.
	c.JSON(http.StatusOK, result)
}

// MCPDelegationAuth is deliberately separate from dashboard/PAT and relay-key
// authentication. A credential for those APIs cannot fall through into delegation.
func MCPDelegationAuth(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !mcpsvc.AllowedClientOrigin(c.GetHeader("Origin")) {
		mcpResponse(c, 0, nil, mcpsvc.Failure(403, "ORIGIN_NOT_ALLOWED", "Request origin is not allowed"))
		c.Abort()
		recordMCPDelegationAudit(c, "mcp.gateway", nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	actor, err := mcpsvc.AuthenticateDelegation(ctx, c.GetHeader("Authorization"))
	cancel()
	if err != nil {
		c.Header("WWW-Authenticate", `Bearer realm="new-api-mcp"`)
		mcpResponse(c, 0, nil, err)
		c.Abort()
		recordMCPDelegationAudit(c, "mcp.gateway", nil)
		return
	}
	c.Set("mcp_delegated_actor", actor)
	ctx, cancel = context.WithDeadline(c.Request.Context(), actor.ExpiresAt)
	defer cancel()
	c.Request = c.Request.WithContext(ctx)
	c.Next()
	recordMCPDelegationAudit(c, "mcp.gateway", actor)
}

func MCPGateway(c *gin.Context) {
	actor := c.MustGet("mcp_delegated_actor").(*mcpsvc.Actor)
	if err := mcpsvc.ServeGateway(c.Writer, c.Request, actor); err != nil && !c.Writer.Written() {
		mcpResponse(c, 0, nil, err)
	}
}

func MCPGatewayConfig(c *gin.Context) {
	actor := c.MustGet("mcp_delegated_actor").(*mcpsvc.Actor)
	services, err := mcpsvc.GatewayServices(c.Request.Context(), actor)
	mcpResponse(c, 200, gin.H{"gateway_url": mcpsvc.PublicBaseURL() + "/mcp/gateway", "items": services, "authentication": "bearer_mcp_delegation"}, err)
}

func recordMCPDelegationAudit(c *gin.Context, action string, actor *mcpsvc.Actor) {
	entry := model.AuditLog{Category: model.AuditCategorySecurity, Action: action, AuthMethod: "mcp_delegation", ActorRole: common.RoleCommonUser, Success: c.Writer.Status() < 400}
	if actor != nil {
		entry.UserId = actor.UserID
		if user, err := model.GetUserById(actor.UserID, false); err == nil {
			entry.Username, entry.ActorRole = user.Username, user.Role
		}
		entry.Other.Op = &model.AuditOperation{Action: action, Params: model.AuditFields{"application": actor.Delegation.ClientID}}
	} else if clientID, _, ok := c.Request.BasicAuth(); ok && len(clientID) <= 256 {
		entry.Other.Op = &model.AuditOperation{Action: action, Params: model.AuditFields{"requested_application": clientID}}
	}
	model.RecordAuditLog(c, entry)
}
