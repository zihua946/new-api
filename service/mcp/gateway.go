package mcp

import (
	"context"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

type GatewayService struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	URL    string `json:"url"`
	server *model.MCPServer
	actor  *Actor
}

func GatewayServices(ctx context.Context, actor *Actor) ([]GatewayService, error) {
	if actor == nil || actor.Delegation == nil {
		return nil, Failure(401, "INVALID_DELEGATION", "MCP delegation is required")
	}
	var installations []model.MCPInstallation
	err := model.DB.Where("user_id = ? AND admin = ? AND authorized = ?", actor.UserID, false, true).Order("server_id").Find(&installations).Error
	if err != nil {
		return nil, err
	}
	services := make([]GatewayService, 0, len(installations))
	for _, installation := range installations {
		if !actor.Delegation.AllowsServer(installation.ServerID) {
			continue
		}
		server, err := model.GetMCPServer(installation.ServerID)
		if err != nil || !server.Catalog || !server.Enabled || server.Archived || installation.ConnectionRevision != server.ConnectionRevision {
			continue
		}
		connected, err := InstallationActor(ctx, server, actor, false)
		if err != nil {
			if ErrorResponse(err).Status < 500 {
				continue // Expired, revoked or missing upstream authorization is never replaced.
			}
			return nil, err
		}
		services = append(services, GatewayService{ID: server.ID, Name: server.Name, URL: PublicBaseURL() + "/mcp/" + server.ID, server: server, actor: connected})
	}
	return services, nil
}

// ServeGateway exposes a single Streamable HTTP MCP connection to agent hosts.
// Tool names are stable and namespaced; identical upstream names cannot collide.
func ServeGateway(w http.ResponseWriter, r *http.Request, actor *Actor) error {
	services, err := GatewayServices(r.Context(), actor)
	if err != nil {
		return err
	}
	var bindings []protocolTool
	for _, service := range services {
		views, err := AvailableTools(r.Context(), service.server, service.actor)
		if err != nil {
			if ErrorResponse(err).Status < 500 {
				continue
			}
			return err
		}
		for _, view := range views {
			tool := view.MCPTool
			tool.Description = service.Name + " / " + tool.Name + ": " + tool.Description
			bindings = append(bindings, protocolTool{server: service.server, actor: service.actor, tool: tool, name: "mcp_" + Digest(service.ID + "\x00" + tool.Name)[:40]})
		}
	}
	// Bound the aggregate catalog just as individual upstream results are bounded.
	definitions := make([]model.MCPTool, 0, len(bindings))
	for _, binding := range bindings {
		definitions = append(definitions, binding.tool)
	}
	encoded, err := common.Marshal(definitions)
	if err != nil || len(encoded) > MaxResultBytes {
		return Failure(413, "CATALOG_TOO_LARGE", "The delegated MCP tool catalog exceeds the response limit")
	}
	return serveProtocolTools(w, r, bindings)
}
