package mcp

import (
	"context"
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

func TestConnection(ctx context.Context, serverID string, userID int, sessionID string) (result map[string]any, testErr error) {
	server, err := model.GetMCPServer(serverID)
	if err != nil {
		return nil, err
	}
	defer func() {
		health := model.MCPJSON{"checked_at": time.Now().UTC().Format(time.RFC3339), "connected": testErr == nil, "tool_execution_verified": false}
		if testErr != nil {
			health["error_code"] = ErrorResponse(testErr).Code
		} else {
			for _, key := range []string{"version", "protocol_version"} {
				if value, ok := result[key].(string); ok && len(value) <= 128 {
					health[key] = value
				}
			}
		}
		// A test of an older configuration must not overwrite the current result.
		if err := model.DB.Model(&model.MCPServer{}).Where("id = ? AND revision = ?", server.ID, server.Revision).Update("connection_health", health).Error; err != nil {
			common.SysError("MCP connection health could not be persisted")
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(server.Limits.TimeoutSeconds)*time.Second)
	defer cancel()
	if server.Kind == "public_knowledge" {
		if _, err = publicLibraryClient(ctx, server, &Actor{UserID: userID, Draft: !server.Enabled}); err != nil {
			return nil, err
		}
		return map[string]any{"connected": true, "authentication_verified": true}, nil
	}
	if server.Catalog {
		token, err := discoveryToken(ctx, server, userID)
		if err != nil {
			return nil, err
		}
		if server.Kind != "remote_mcp" {
			if _, err = AuthenticateOAuth(ctx, server, token); err != nil {
				return nil, err
			}
			if _, err = providerConfig(server); err != nil {
				return nil, err
			}
			return map[string]any{"connected": true, "version": "1.0.0"}, nil
		}
		session, closeSession, err := connectRemote(ctx, server, token)
		if err != nil {
			return nil, err
		}
		defer closeSession()
		if _, err = session.ListTools(ctx, nil); err != nil {
			return nil, Failure(502, "MCP_DISCOVERY_FAILED", "Remote tool discovery failed")
		}
		initialized := session.InitializeResult()
		if initialized == nil || initialized.ServerInfo == nil {
			return nil, Failure(502, "UPSTREAM_CONNECT_FAILED", "MCP initialization did not report server information")
		}
		return map[string]any{"connected": true, "version": initialized.ServerInfo.Version, "protocol_version": initialized.ProtocolVersion}, nil
	}
	switch server.Kind {
	case "deepseek":
		base, key, err := DeepSeekConnection(server)
		if err != nil {
			return nil, err
		}
		client, err := NewHTTPClient(base, 15*time.Second)
		if err != nil {
			return nil, err
		}
		defer client.CloseIdleConnections()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Authorization", "Bearer "+key)
		response, err := client.Do(request)
		if err != nil {
			return nil, Failure(502, "CONNECTION_FAILED", "Upstream connection failed")
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			return nil, Failure(502, "AUTHENTICATION_CHECK_FAILED", "The upstream read-only authentication check failed")
		}
		return map[string]any{"connected": true, "authentication_verified": true, "tool_execution_verified": false}, nil
	case "knowledge":
		return nil, Failure(410, "SERVICE_RETIRED", "Service type retired")
	default:
		session, closeSession, err := connectRemote(ctx, server)
		if err != nil {
			return nil, err
		}
		defer closeSession()
		if _, err = session.ListTools(ctx, nil); err != nil {
			return nil, Failure(502, "MCP_DISCOVERY_FAILED", "MCP tool discovery failed")
		}
		return map[string]any{"connected": true, "handshake_verified": true, "tool_execution_verified": false}, nil
	}
}
