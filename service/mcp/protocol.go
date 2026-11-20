package mcp

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func AllowedClientOrigin(origin string) bool {
	if origin == "" {
		return true
	}
	if origin == PublicBaseURL() && origin != "" {
		return true
	}
	return slices.Contains(strings.Split(os.Getenv("MCP_ALLOWED_CLIENT_ORIGINS"), ","), origin)
}

func ProtectedResourceMetadata(server *model.MCPServer) map[string]any {
	return map[string]any{"resource": ResourceURL(server), "authorization_servers": []string{strings.TrimRight(os.Getenv("MCP_CASDOOR_ISSUER"), "/")}, "scopes_supported": []string{"mcp:tools:read", "mcp:tools:call"}, "bearer_methods_supported": []string{"header"}}
}

// Each HTTP request authenticates independently. Stateless SDK sessions cannot
// accidentally transfer one user's tool catalog or authorization to another.
func ServeProtocol(w http.ResponseWriter, r *http.Request, server *model.MCPServer, actor *Actor) error {
	if !actor.CanCall && r.Method == http.MethodPost {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxArgumentBytes))
		if err != nil {
			return Failure(413, "REQUEST_TOO_LARGE", "MCP request exceeds the request limit")
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var envelope struct {
			Method string `json:"method"`
		}
		// Malformed protocol requests still belong to the SDK. A valid call
		// without the call scope is rejected at the HTTP authorization boundary.
		if common.Unmarshal(body, &envelope) == nil && envelope.Method == "tools/call" {
			return Failure(403, "INSUFFICIENT_SCOPE", "The mcp:tools:call scope is required")
		}
	}
	var envelope struct {
		Method string `json:"method"`
	}
	if server.Catalog && server.Kind == "remote_mcp" && r.Method == http.MethodPost {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxArgumentBytes))
		if err != nil {
			return Failure(413, "REQUEST_TOO_LARGE", "MCP request exceeds the request limit")
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		_ = common.Unmarshal(body, &envelope)
	}
	if server.Kind == "knowledge" {
		return Failure(410, "SERVICE_RETIRED", "Service type retired")
	}
	tools, err := ListTools(server.ID)
	if envelope.Method == "tools/list" || envelope.Method == "tools/call" {
		tools, err = AvailableTools(r.Context(), server, actor)
	}
	if err != nil {
		return err
	}
	sdk := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "new-api", Version: "1.0.0"}, nil)
	for _, view := range tools {
		tool := view.MCPTool
		if !CanUseTool(server, &tool, actor, false) || server.Catalog && view.Pricing == nil {
			continue
		}

		definition := &mcpsdk.Tool{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema}
		if len(tool.OutputSchema) > 0 {
			definition.OutputSchema = tool.OutputSchema
		}
		sdk.AddTool(definition, func(ctx context.Context, request *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			var arguments map[string]any
			if len(request.Params.Arguments) != 0 {
				if common.Unmarshal(request.Params.Arguments, &arguments) != nil {
					return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "Arguments must be a JSON object"}
				}
			}
			key := ""
			if request.Extra != nil {
				key = request.Extra.Header.Get("Idempotency-Key")
			}
			if server.Catalog && server.Kind == "remote_mcp" && envelope.Method != "tools/call" {
				views, err := AvailableTools(ctx, server, actor)
				if err != nil {
					return ProtocolFailure(err)
				}
				if !slices.ContainsFunc(views, func(view ToolView) bool { return view.ID == tool.ID }) {
					return ProtocolFailure(Failure(403, "TOOL_ACCESS_DENIED", "Tool is unavailable to this identity"))
				}
			}
			requestID, _ := r.Context().Value(common.RequestIdKey).(string)
			prepared, err := PrepareCall(server.ID, tool.Name, actor, arguments, InvocationOptions{RequestID: requestID, IdempotencyKey: key})
			if err != nil {
				return ProtocolFailure(err)
			}
			var progress Progress
			if token := request.Params.GetProgressToken(); token != nil {
				var count float64
				progress = func(message string) {
					count++
					_ = request.Session.NotifyProgress(ctx, &mcpsdk.ProgressNotificationParams{ProgressToken: token, Progress: count, Message: message})
				}
			}
			result, err := prepared.Run(ctx, progress)
			if err != nil {
				return ProtocolFailure(err)
			}
			return result, nil
		})
	}
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return sdk }, &mcpsdk.StreamableHTTPOptions{Stateless: true, MaxRequestBodyBytes: MaxArgumentBytes})
	handler.ServeHTTP(w, r)
	return nil
}

func ProtocolFailure(err error) (*mcpsdk.CallToolResult, error) {
	failure := ErrorResponse(err)
	if failure.Code == "INVALID_ARGUMENTS" || failure.Code == "INVALID_IDEMPOTENCY_KEY" {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: failure.Message}
	}
	return &mcpsdk.CallToolResult{IsError: true, Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: failure.Message}}, Meta: mcpsdk.Meta{"knowyet.com/error": map[string]any{"code": failure.Code}}}, nil
}

func AvailableTools(ctx context.Context, server *model.MCPServer, actor *Actor) ([]ToolView, error) {
	return AvailableTestTools(ctx, server, actor, false)
}
func AvailableTestTools(ctx context.Context, server *model.MCPServer, actor *Actor, draft bool) ([]ToolView, error) {
	if server.Kind == "knowledge" {
		return nil, Failure(410, "SERVICE_RETIRED", "Service type retired")
	}
	tools, err := ListTools(server.ID)
	if err != nil {
		return nil, err
	}
	var upstream map[string]bool
	if server.Catalog && server.Kind == "remote_mcp" {
		ctx, cancel := context.WithTimeout(ctx, time.Duration(server.Limits.TimeoutSeconds)*time.Second)
		defer cancel()
		session, closeSession, err := connectRemote(ctx, server, actor.AccessToken)
		if err != nil {
			return nil, err
		}
		defer closeSession()
		remote, err := readUpstreamTools(ctx, session)
		if err != nil {
			return nil, err
		}
		hashes := make(map[string]string, len(remote))
		for _, tool := range remote {
			hashes[tool.Name] = tool.SchemaHash
		}
		upstream = map[string]bool{}
		for _, view := range tools {
			hash, ok := hashes[view.Name]
			upstream[view.Name] = ok && hash == reviewedSchemaHash(&view.MCPTool)
		}

	}
	return slices.DeleteFunc(tools, func(view ToolView) bool {
		return !CanUseTool(server, &view.MCPTool, actor, draft) || (server.Catalog && view.Pricing == nil) || (upstream != nil && !upstream[view.Name])
	}), nil
}

func reviewedSchemaHash(tool *model.MCPTool) string {
	if tool.SchemaHash != "" {
		return tool.SchemaHash
	}
	hash, _ := SchemaHash(tool.InputSchema, tool.OutputSchema)
	return hash
}
