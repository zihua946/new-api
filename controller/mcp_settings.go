package controller

import (
	"fmt"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	mcpsvc "github.com/QuantumNous/new-api/service/mcp"
	"github.com/gin-gonic/gin"
	"net/http"
)

func mcpConfigurationProof(c *gin.Context, target string, revision int64) bool {
	raw, err := common.Marshal(map[string]any{"target": target, "revision": revision})
	if err != nil {
		return false
	}
	return middleware.RequireSecurityProof(c, service.VerificationOperation{Scope: service.VerificationScopeMCPConfigWrite, Context: raw}) != nil
}
func MCPSettings(c *gin.Context) {
	if c.Request.Method == http.MethodGet {
		view, err := mcpsvc.ReadSettings()
		mcpResponse(c, 200, view, err)
		return
	}
	var input struct {
		ExpectedRevision int64             `json:"expected_revision"`
		Values           map[string]string `json:"values"`
	}
	if common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 65536), &input) != nil {
		mcpResponse(c, 0, nil, mcpsvc.Failure(400, "INVALID_CONFIG", "Invalid MCP configuration"))
		return
	}
	if !mcpConfigurationProof(c, "settings", input.ExpectedRevision) {
		return
	}
	view, err := mcpsvc.SaveSettings(c.Request.Context(), input.ExpectedRevision, input.Values)
	if err == nil {
		model.RecordLog(c.GetInt("id"), model.LogTypeManage, "Updated MCP settings")
	}
	mcpResponse(c, 200, view, err)
}
func MCPTrustedApps(c *gin.Context) {
	if c.Request.Method == http.MethodGet {
		rows, err := mcpsvc.ListTrustedApplications()
		mcpResponse(c, 200, rows, err)
		return
	}
	var input mcpsvc.TrustedApplicationInput
	if common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 65536), &input) != nil {
		mcpResponse(c, 0, nil, mcpsvc.Failure(400, "INVALID_APPLICATION", "Invalid application configuration"))
		return
	}
	id := c.Param("app_id")
	if !mcpConfigurationProof(c, "app:"+id, input.ExpectedRevision) {
		return
	}
	var err error
	if c.Request.Method == http.MethodDelete {
		err = mcpsvc.DeleteTrustedApplication(id, input.ExpectedRevision)
	} else {
		err = mcpsvc.SaveTrustedApplication(id, input)
	}
	if err == nil {
		model.RecordLog(c.GetInt("id"), model.LogTypeManage, fmt.Sprintf("Changed MCP trusted application %q", id))
	}
	mcpResponse(c, 200, nil, err)
}

func MCPIdentityProviders(c *gin.Context) {
	var rows []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	err := model.DB.Model(&model.CustomOAuthProvider{}).Select("id", "name").Where("enabled = ? AND user_id_field = ?", true, "sub").Order("id").Find(&rows).Error
	mcpResponse(c, 200, rows, err)
}

func MCPCheckSettings(c *gin.Context) {
	view, err := mcpsvc.ReadSettings()
	if err != nil {
		mcpResponse(c, 0, nil, err)
		return
	}
	checks := map[string]bool{}
	checks["public_base_url"] = mcpsvc.PublicBaseURL() != ""
	checks["casdoor_provider_id"] = mcpsvc.ConfigValue("casdoor_issuer") != "" && mcpsvc.ConfigValue("casdoor_organization") != ""
	checks["oauth_client_id"] = view.Values["oauth_client_id"] != "" && view.Secrets["oauth_client_secret"]
	checks["knowledge_client_id"] = view.Values["knowledge_engine_url"] != "" && view.Values["knowledge_client_id"] != "" && view.Secrets["knowledge_client_secret"]
	mcpResponse(c, 200, checks, nil)
}
