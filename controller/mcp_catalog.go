package controller

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/QuantumNous/new-api/model"
	mcpsvc "github.com/QuantumNous/new-api/service/mcp"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type mcpClientConnection struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

type mcpCatalogItem struct {
	Icon             model.MCPEncryptedResult `json:"icon,omitempty"`
	ID               string                   `json:"id"`
	Name             string                   `json:"name"`
	Description      string                   `json:"description"`
	Version          string                   `json:"version"`
	AuthMode         string                   `json:"auth_mode"`
	Enabled          bool                     `json:"enabled"`
	Installation     *model.MCPInstallation   `json:"installation,omitempty"`
	ClientConnection *mcpClientConnection     `json:"client_connection,omitempty"`
	URL              string                   `json:"url,omitempty"`
}

func MCPCatalog(c *gin.Context) {
	page, size, err := mcpPage(c)
	if err != nil {
		mcpResponse(c, 0, nil, err)
		return
	}
	userID := c.GetInt("id")
	engineAuthorized, err := mcpsvc.HasEngineAuthorization(userID)
	if err != nil {
		mcpResponse(c, 0, nil, err)
		return
	}
	var installations []model.MCPInstallation
	if err = model.DB.Where("user_id = ? AND admin = ?", userID, false).Find(&installations).Error; err != nil {
		mcpResponse(c, 0, nil, err)
		return
	}
	owned := make(map[string]*model.MCPInstallation, len(installations))
	ids := make([]string, 0, len(installations))
	for i := range installations {
		item := &installations[i]
		owned[item.ServerID] = item
		ids = append(ids, item.ServerID)
	}
	q := model.DB.Model(&model.MCPServer{}).Where("catalog = ? AND enabled = ? AND archived = ?", true, true, false)
	if c.FullPath() == "/api/mcp/self/installations" {
		q = q.Where("id IN ?", ids)
	}
	if name := c.Query("name"); name != "" {
		q = q.Where("name LIKE ?", "%"+name+"%")
	}
	var total int64
	var rows []model.MCPServer
	if err = q.Count(&total).Error; err == nil {
		err = q.Select("id", "name", "description", "icon", "version", "kind", "catalog", "connection", "enabled", "archived", "connection_revision").Order("name, id").Offset((page - 1) * size).Limit(size).Find(&rows).Error
	}
	if err != nil {
		mcpResponse(c, 0, nil, err)
		return
	}
	items := make([]mcpCatalogItem, 0, len(rows))
	for _, server := range rows {
		item := mcpCatalogItem{Icon: server.Icon, ID: server.ID, Name: server.Name, Description: server.Description, Version: server.Version, AuthMode: server.AuthMode, Enabled: server.Enabled && !server.Archived, Installation: owned[server.ID], ClientConnection: &mcpClientConnection{Type: "streamable-http", URL: mcpsvc.PublicBaseURL() + "/mcp/" + server.ID}}
		if item.Installation != nil {
			item.Installation.Authorized = item.Installation.Authorized && item.Installation.ConnectionRevision == server.ConnectionRevision
			if server.Kind == "public_knowledge" {
				item.Installation.Authorized = item.Installation.Authorized && engineAuthorized
			}
			if item.Enabled && item.Installation.Authorized {
				item.URL = mcpsvc.PublicBaseURL() + "/mcp/" + server.ID
			}
		}
		if server.Kind == "remote_mcp" && server.Connection.URL != "" {
			switch server.Connection.Transport {
			case "streamable_http":
				item.ClientConnection = &mcpClientConnection{Type: "streamable-http", URL: server.Connection.URL}
			case "http_sse":
				item.ClientConnection = &mcpClientConnection{Type: "sse", URL: server.Connection.URL}
			}
		}
		items = append(items, item)
	}
	mcpResponse(c, 200, gin.H{"items": items, "total": total, "page": page, "page_size": size, "authentication": "bearer_mcp_delegation"}, nil)
}

func MCPCatalogTools(c *gin.Context) {
	server, err := model.GetMCPServer(c.Param("id"))
	if err != nil || !server.Catalog || !server.Enabled {
		mcpResponse(c, 0, nil, gorm.ErrRecordNotFound)
		return
	}
	tools, err := mcpsvc.ListTools(server.ID)
	published := make([]mcpsvc.ToolView, 0, len(tools))
	for _, tool := range tools {
		if tool.Enabled && !tool.Removed && !tool.ReviewRequired && tool.Pricing != nil {
			published = append(published, tool)
		}
	}
	mcpResponse(c, 200, published, err)
}

func MCPInstall(c *gin.Context) {
	var input struct {
		ServerID string `json:"server_id"`
	}
	if !mcpDecode(c, &input) {
		return
	}
	row, err := mcpsvc.Install(input.ServerID, c.GetInt("id"), false)
	mcpResponse(c, 201, row, err)
}

func MCPUninstall(c *gin.Context) {
	err := model.DB.Transaction(func(tx *gorm.DB) error {
		var row model.MCPInstallation
		if err := tx.Where("id = ? AND user_id = ? AND admin = ?", c.Param("id"), c.GetInt("id"), false).First(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		if err := tx.Delete(&row).Error; err != nil {
			return err
		}
		return tx.Where("installation_id = ?", row.ID).Delete(&model.MCPAuthorizationFlow{}).Error
	})
	mcpResponse(c, 200, gin.H{}, err)
}

func MCPDisableInstallation(c *gin.Context) {
	err := mcpsvc.DisableInstallation(c.Param("id"), c.GetInt("id"))
	mcpResponse(c, 200, gin.H{}, err)
}

func MCPDisconnectInstallation(c *gin.Context) {
	err := mcpsvc.DisconnectInstallation(c.Param("id"), c.GetInt("id"))
	mcpResponse(c, 200, gin.H{}, err)
}

func MCPAuthorizeInstallation(c *gin.Context) {
	var row model.MCPInstallation
	err := model.DB.Where("id = ? AND user_id = ? AND admin = ?", c.Param("id"), c.GetInt("id"), false).First(&row).Error
	if err != nil {
		mcpResponse(c, 0, nil, err)
		return
	}
	if strings.HasSuffix(c.FullPath(), "/enable") {
		server, err := model.GetMCPServer(row.ServerID)
		if err != nil || !server.Enabled {
			mcpResponse(c, 0, nil, gorm.ErrRecordNotFound)
			return
		}
		reuse := server.AuthMode == "none"
		if server.Kind == "public_knowledge" {
			reuse, err = mcpsvc.HasEngineAuthorization(row.UserID)
		}
		if err != nil {
			mcpResponse(c, 0, nil, err)
			return
		}
		if !reuse && server.Kind != "public_knowledge" {
			_, tokenErr := mcpsvc.ReusableInstallationToken(c.Request.Context(), server, row.UserID)
			if tokenErr == nil {
				reuse = true
			} else if mcpsvc.ErrorResponse(tokenErr).Status != 401 {
				mcpResponse(c, 0, nil, tokenErr)
				return
			}
		}
		if reuse {
			res := model.DB.Model(&row).Where("COALESCE(authorization_attempt, '') = ?", row.AuthorizationAttempt).Updates(map[string]any{"authorized": true, "connection_revision": server.ConnectionRevision, "authorization_attempt": uuid.NewString()})
			err = res.Error
			if err == nil && res.RowsAffected != 1 {
				err = model.ErrMCPRevisionConflict
			}
			mcpResponse(c, 200, gin.H{"connected": true}, err)
			return
		}
	}
	target, err := mcpsvc.BeginAuthorization(c.Request.Context(), &row, mcpSessionID(c))
	mcpResponse(c, 200, gin.H{"authorization_url": target}, err)
}

func MCPAuthorizeDiscovery(c *gin.Context) {
	row, err := mcpsvc.Install(c.Param("id"), c.GetInt("id"), true)
	if err != nil {
		mcpResponse(c, 0, nil, err)
		return
	}
	target, err := mcpsvc.BeginAuthorization(c.Request.Context(), row, mcpSessionID(c))
	mcpResponse(c, 200, gin.H{"authorization_url": target}, err)
}

func MCPOAuthCallback(c *gin.Context) {
	if c.Request.Method == http.MethodGet {
		state, code, issuer := c.Query("state"), c.Query("code"), c.Query("iss")
		if state == "" || len(state) > 256 || len(code) > 4096 || len(issuer) > 2048 {
			mcpResponse(c, 0, nil, mcpsvc.Failure(400, "INVALID_AUTHORIZATION_CALLBACK", "Invalid OAuth callback"))
			return
		}
		c.Header("Cache-Control", "no-store")
		c.Header("Referrer-Policy", "no-referrer")
		base := strings.TrimRight(os.Getenv("MCP_DASHBOARD_BASE_URL"), "/")
		if base != "" {
			parsed, err := url.Parse(base)
			if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" || !mcpsvc.AllowedClientOrigin(base) || (parsed.Scheme != "http" && parsed.Scheme != "https") {
				mcpResponse(c, 0, nil, mcpsvc.Failure(503, "MCP_NOT_CONFIGURED", "Invalid MCP dashboard origin"))
				return
			}
		}
		var pending model.MCPAuthorizationFlow
		var installation model.MCPInstallation
		if model.DB.Where("id = ?", mcpsvc.Digest(state)).First(&pending).Error != nil || model.DB.Where("id = ?", pending.InstallationID).First(&installation).Error != nil {
			mcpResponse(c, 0, nil, mcpsvc.Failure(400, "INVALID_AUTHORIZATION_STATE", "Authorization expired or could not be verified"))
			return
		}
		path := "/mcp/servers/" + url.PathEscape(installation.ServerID) + "?tab=tools"
		if installation.Admin {
			path = "/mcp?server_id=" + url.QueryEscape(installation.ServerID) + "&discover=true"
		}
		c.Redirect(303, base+path+"#"+url.Values{"mcp_state": {state}, "mcp_code": {code}, "mcp_iss": {issuer}}.Encode())
		return
	}
	var input struct {
		State  string `json:"state"`
		Code   string `json:"code"`
		Issuer string `json:"issuer"`
	}
	if !mcpDecode(c, &input) {
		return
	}
	admin, err := mcpsvc.FinishAuthorization(c.Request.Context(), c.GetInt("id"), mcpSessionID(c), input.State, input.Code, input.Issuer)
	mcpResponse(c, 200, gin.H{"admin": admin}, err)
}

func MCPLocalProtocol(c *gin.Context) {
	server, err := localMCPServer(c)
	if err != nil || !server.Catalog {
		mcpResponse(c, 0, nil, gorm.ErrRecordNotFound)
		return
	}
	if !mcpsvc.AllowedClientOrigin(c.GetHeader("Origin")) {
		mcpResponse(c, 0, nil, mcpsvc.Failure(403, "ORIGIN_NOT_ALLOWED", "Request origin is not allowed"))
		return
	}
	actor, err := mcpsvc.Authenticate(c.Request.Context(), &model.MCPServer{ID: server.ID, Catalog: true, Kind: server.Kind, Connection: server.Connection, AuthMode: "casdoor_oauth"}, c.GetHeader("Authorization"), c.ClientIP())
	if err != nil {
		if mcpsvc.ErrorResponse(err).Status == 401 {
			c.Header("WWW-Authenticate", `Bearer resource_metadata="`+mcpsvc.PublicBaseURL()+`/.well-known/oauth-protected-resource`+strings.TrimPrefix(mcpsvc.ResourceURL(server), mcpsvc.PublicBaseURL())+`"`)
		}
		mcpResponse(c, 0, nil, err)
		return
	}
	if server.Kind == "public_knowledge" {
		actor, err = mcpsvc.InstallationActor(c.Request.Context(), server, actor, false)
		if err != nil {
			mcpResponse(c, 0, nil, err)
			return
		}
	}
	if !server.Enabled {
		mcpResponse(c, 0, nil, gorm.ErrRecordNotFound)
		return
	}
	if err = mcpsvc.ServeProtocol(c.Writer, c.Request, server, actor); err != nil && !c.Writer.Written() {
		mcpResponse(c, 0, nil, err)
	}
}

func MCPLocalMetadata(c *gin.Context) {
	server, err := localMCPServer(c)
	if err != nil || !server.Catalog {
		mcpResponse(c, 0, nil, gorm.ErrRecordNotFound)
		return
	}
	c.JSON(200, mcpsvc.ProtectedResourceMetadata(server))
}

func MCPOAuthConfig(c *gin.Context) {
	base := mcpsvc.PublicBaseURL()
	if base == "" {
		mcpResponse(c, 0, nil, mcpsvc.Failure(503, "MCP_NOT_CONFIGURED", "Configure the public MCP base URL"))
		return
	}
	mcpResponse(c, 200, gin.H{"callback_url": base + "/api/mcp/oauth/callback"}, nil)
}

func MCPCatalogDetail(c *gin.Context) {
	server, err := model.GetMCPServer(c.Param("id"))
	if err != nil || !server.Enabled || !server.Catalog {
		mcpResponse(c, 0, nil, gorm.ErrRecordNotFound)
		return
	}
	installation, err := mcpsvc.FindInstallation(server.ID, c.GetInt("id"), false)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		installation = nil
		err = nil
	} else if err == nil {
		installation.Authorized = installation.Authorized && installation.ConnectionRevision == server.ConnectionRevision
		if installation.Authorized && server.Kind == "public_knowledge" {
			installation.Authorized, err = mcpsvc.HasEngineAuthorization(c.GetInt("id"))
		}
	}
	mcpResponse(c, 200, gin.H{"id": server.ID, "name": server.Name, "icon": server.Icon, "readme": server.Readme, "description": server.Description, "kind": server.Kind, "version": server.Version, "auth_mode": server.AuthMode, "installation": installation, "enabled": true, "limits": server.Limits}, err)
}

func localMCPServer(c *gin.Context) (*model.MCPServer, error) {
	if id := c.Param("instance"); id != "" {
		server, err := model.GetMCPServer(id)
		if err != nil || server.ID != id || c.Param("provider") != "knowledge" || server.Kind != "public_knowledge" || !server.Enabled {
			return nil, gorm.ErrRecordNotFound
		}
		return server, nil
	}
	server, err := model.GetMCPServerBySlug("provider-" + c.Param("provider"))
	if err != nil || mcpsvc.LocalProvider(server.Connection.URL) != c.Param("provider") {
		return nil, gorm.ErrRecordNotFound
	}
	return server, nil
}
