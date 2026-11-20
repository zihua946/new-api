package router

import (
	"net/http"

	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/service/authz"
	mcpsvc "github.com/QuantumNous/new-api/service/mcp"
	"github.com/gin-gonic/gin"
)

func SetMCPRouter(engine *gin.Engine) {
	// Separate groups deliberately omit the management/relay gzip middleware.
	public := engine.Group("", middleware.RouteTag("mcp"), mcpFeatureGate)
	public.Any("/mcp/:server_slug", controller.MCPProtocol)
	public.POST("/v1/mcp/test/chat/completions", middleware.TokenAuth(), controller.MCPChatTest(engine))
	public.Any("/mcp/providers/:provider/:instance", controller.MCPLocalProtocol)
	public.GET("/.well-known/oauth-protected-resource/mcp/providers/:provider/:instance", controller.MCPLocalMetadata)
	public.Any("/mcp/providers/:provider", controller.MCPLocalProtocol)
	public.GET("/.well-known/oauth-protected-resource/mcp/providers/:provider", controller.MCPLocalMetadata)
	api := engine.Group("/api/mcp", middleware.RouteTag("api"), mcpFeatureGate, middleware.DisableCache(), middleware.GlobalAPIRateLimit())
	api.GET("/test-authorizations/callback", controller.MCPTestAuthorizationCallback)
	api.GET("/oauth/callback", controller.MCPOAuthCallback)
	api.POST("/oauth/callback", middleware.UserAuth(), mcpMutationGuard, controller.MCPOAuthCallback)
	api.GET("/catalog", middleware.UserAuth(), controller.MCPCatalog)
	api.GET("/catalog/:id", middleware.UserAuth(), controller.MCPCatalogDetail)
	api.GET("/catalog/:id/tools", middleware.UserAuth(), controller.MCPCatalogTools)
	api.GET("/self/config", middleware.TokenOrUserAuth(), controller.MCPCatalog)
	self := api.Group("/self", middleware.UserAuth(), mcpMutationGuard)
	self.GET("/installations", controller.MCPCatalog)
	self.POST("/installations", controller.MCPInstall)
	self.DELETE("/installations/:id", controller.MCPUninstall)
	self.POST("/installations/:id/enable", controller.MCPAuthorizeInstallation)
	self.POST("/installations/:id/disable", controller.MCPDisableInstallation)
	self.DELETE("/installations/:id/authorization", controller.MCPDisconnectInstallation)
	self.POST("/installations/:id/authorization", controller.MCPAuthorizeInstallation)
	self.GET("/servers/:id/test-tools", controller.MCPTestTools)
	self.POST("/servers/:id/test-invocations", controller.MCPTestInvocation)
	self.GET("/test-invocations/:invocation_id", controller.MCPTestResult)
	self.GET("/invocations", controller.MCPInvocations)
	self.GET("/usage", controller.MCPUsage)
	admin := api.Group("", middleware.AdminAuth(), mcpMutationGuard)
	admin.GET("/oauth/config", middleware.RequirePermission(authz.MCPRead), controller.MCPOAuthConfig)
	admin.POST("/command-preview", middleware.RequirePermission(authz.MCPSensitiveWrite), controller.MCPCommandPreview)
	admin.GET("/servers", middleware.RequirePermission(authz.MCPRead), controller.MCPServers)
	admin.POST("/servers", middleware.RequirePermission(authz.MCPSensitiveWrite), controller.MCPServers)
	admin.GET("/servers/:id", middleware.RequirePermission(authz.MCPRead), controller.MCPServer)
	admin.PATCH("/servers/:id", middleware.RequirePermission(authz.MCPWrite), controller.MCPServer)
	admin.DELETE("/servers/:id", middleware.RequirePermission(authz.MCPSensitiveWrite), controller.MCPServer)
	admin.POST("/servers/:id/status", middleware.RequirePermission(authz.MCPOperate), controller.MCPServerStatus)
	admin.POST("/servers/:id/package", middleware.RequirePermission(authz.MCPSensitiveWrite), controller.MCPInstallPackage)
	admin.PUT("/servers/:id/credentials", middleware.RequirePermission(authz.MCPSensitiveWrite), controller.MCPCredentials)
	admin.GET("/channel-options", middleware.RequirePermission(authz.MCPRead), controller.MCPChannelOptions)
	admin.GET("/servers/:id/connection-info", middleware.RequirePermission(authz.MCPRead), controller.MCPConnectionInfo)
	admin.GET("/servers/:id/tools", middleware.RequirePermission(authz.MCPRead), controller.MCPTools)
	admin.POST("/servers/:id/discovery-authorization", middleware.RequirePermission(authz.MCPOperate), controller.MCPAuthorizeDiscovery)
	admin.POST("/servers/:id/discoveries", middleware.RequirePermission(authz.MCPOperate), controller.MCPDiscovery)
	admin.POST("/servers/:id/tools/sync", middleware.RequirePermission(authz.MCPOperate), controller.MCPToolSync)
	admin.PATCH("/servers/:id/tools/:tool_id", controller.MCPTool)
	admin.PUT("/servers/:id/tools/:tool_id/pricing", middleware.RequirePermission(authz.MCPSensitiveWrite), controller.MCPToolPrice)
	admin.POST("/servers/:id/connection-tests", middleware.RequirePermission(authz.MCPOperate), controller.MCPConnectionTest)
	admin.POST("/servers/:id/test-authorizations", middleware.RequirePermission(authz.MCPOperate), controller.MCPTestAuthorization)
	admin.POST("/test-authorizations/callback", middleware.RequirePermission(authz.MCPOperate), controller.MCPTestAuthorizationCallback)
	admin.GET("/servers/:id/test-authorization", middleware.RequirePermission(authz.MCPOperate), controller.MCPTestAuthorization)
	admin.DELETE("/servers/:id/test-authorization", middleware.RequirePermission(authz.MCPOperate), controller.MCPTestAuthorization)
	admin.GET("/servers/:id/test-tools", middleware.RequirePermission(authz.MCPOperate), controller.MCPTestTools)
	admin.POST("/servers/:id/test-invocations", middleware.RequirePermission(authz.MCPOperate), controller.MCPTestInvocation)
	admin.GET("/test-invocations/:invocation_id", middleware.RequirePermission(authz.MCPOperate), controller.MCPTestResult)
	admin.GET("/invocations", middleware.RequirePermission(authz.MCPRead), controller.MCPInvocations)
	admin.GET("/invocations/:invocation_id", middleware.RequirePermission(authz.MCPRead), controller.MCPInvocation)
	admin.GET("/usage", middleware.RequirePermission(authz.MCPRead), controller.MCPUsage)
}

func mcpFeatureGate(c *gin.Context) {
	if !mcpsvc.Enabled() {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	c.Next()
}

func mcpMutationGuard(c *gin.Context) {
	if c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead {
		c.Next()
		return
	}
	// A non-simple header prevents cross-site form requests even without Origin.
	// Never derive trusted origins from unvalidated Host/proxy request headers.
	if c.GetHeader("X-MCP-Request") != "1" || c.GetHeader("Sec-Fetch-Site") == "cross-site" || !mcpsvc.AllowedClientOrigin(c.GetHeader("Origin")) {
		c.AbortWithStatusJSON(403, gin.H{"success": false, "code": "CSRF_REJECTED", "message": "MCP request origin could not be verified"})
		return
	}
	c.Next()
}
