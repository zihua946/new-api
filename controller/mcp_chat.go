package controller

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	mcpsvc "github.com/QuantumNous/new-api/service/mcp"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm/clause"
)

type mcpRelayResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (r *mcpRelayResponse) Header() http.Header { return r.header }
func (r *mcpRelayResponse) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}
func (r *mcpRelayResponse) Write(data []byte) (int, error) {
	if len(data) > mcpsvc.MaxResultBytes-r.body.Len() {
		return 0, errors.New("model response is too large")
	}
	r.WriteHeader(http.StatusOK)
	return r.body.Write(data)
}

func (r *mcpRelayResponse) Flush() {}

var mcpChatKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

type mcpChatMessage struct {
	Role       string `json:"role"`
	Content    any    `json:"content"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls,omitempty"`
}

func MCPChatTest(relay http.Handler) gin.HandlerFunc {
	return func(c *gin.Context) {
		var input struct {
			Model          string           `json:"model"`
			InstallationID string           `json:"mcp_installation_id"`
			Messages       []mcpChatMessage `json:"messages"`
		}
		if !mcpDecode(c, &input) {
			return
		}
		key := c.GetHeader("Idempotency-Key")
		if !mcpChatKeyPattern.MatchString(key) || len(input.Model) == 0 || len(input.Model) > 256 || len(input.Messages) == 0 || len(input.Messages) > 64 {
			mcpResponse(c, 0, nil, mcpsvc.Failure(422, "INVALID_TEST", "A model, messages and valid Idempotency-Key are required"))
			return
		}
		for _, message := range input.Messages {
			if message.Role != "user" && message.Role != "assistant" && message.Role != "system" || len(message.ToolCalls) != 0 || message.ToolCallID != "" {
				mcpResponse(c, 0, nil, mcpsvc.Failure(422, "INVALID_TEST", "Only plain conversation messages are accepted"))
				return
			}
			if _, ok := message.Content.(string); !ok {
				mcpResponse(c, 0, nil, mcpsvc.Failure(422, "INVALID_TEST", "Message content must be text"))
				return
			}
		}
		var installation model.MCPInstallation
		err := model.DB.Where("id = ? AND user_id = ? AND admin = ?", input.InstallationID, c.GetInt("id"), false).First(&installation).Error
		if err != nil {
			mcpResponse(c, 0, nil, err)
			return
		}
		server, err := model.GetMCPServer(installation.ServerID)
		if err != nil || !server.Catalog || !server.Enabled {
			mcpResponse(c, 0, nil, mcpsvc.Failure(404, "SERVER_UNAVAILABLE", "MCP service is unavailable"))
			return
		}
		actor, err := mcpsvc.Authenticate(c.Request.Context(), server, c.GetHeader("Authorization"), c.ClientIP())
		if err == nil {
			actor, err = mcpsvc.InstallationActor(c.Request.Context(), server, actor, false)
		}
		if err != nil || actor.InstallationID != installation.ID {
			if err == nil {
				err = mcpsvc.Failure(403, "INSTALLATION_UNAVAILABLE", "MCP installation is unavailable")
			}
			mcpResponse(c, 0, nil, err)
			return
		}
		encoded, err := common.Marshal(input)
		if err != nil {
			mcpResponse(c, 0, nil, err)
			return
		}
		fingerprint := mcpsvc.Digest(fmt.Sprintf("%d:%d:%s", actor.UserID, actor.TokenID, key))
		run := model.MCPChatTest{ID: fingerprint, RequestHash: mcpsvc.Digest(string(encoded)), ExpiresAt: time.Now().Add(24 * time.Hour)}
		created := model.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&run)
		if created.Error != nil {
			mcpResponse(c, 0, nil, created.Error)
			return
		}
		if created.RowsAffected == 0 {
			var existing model.MCPChatTest
			err = model.DB.First(&existing, "id = ?", fingerprint).Error
			if err != nil || existing.RequestHash != run.RequestHash || existing.Result == "" || time.Now().After(existing.ExpiresAt) {
				mcpResponse(c, 0, nil, mcpsvc.Failure(409, "TEST_REPLAY_UNAVAILABLE", "The test is running, expired, or the key was used for a different request"))
				return
			}
			result, err := mcpsvc.Open(string(existing.Result), "chat-test:"+fingerprint)
			if err != nil {
				mcpResponse(c, 0, nil, err)
				return
			}
			c.Data(200, "application/json", []byte(result))
			return
		}
		result := runMCPConversation(c, relay, server, actor, input.Model, input.Messages, key, fingerprint)
		encoded, err = common.Marshal(result)
		if err == nil && len(encoded) > mcpsvc.MaxResultBytes {
			encoded, err = common.Marshal(map[string]any{"status": "incomplete", "error": "Response exceeds the test limit", "tool_calls": result["tool_calls"], "model_calls": result["model_calls"]})
		}
		if err == nil {
			var sealed string
			sealed, err = mcpsvc.Seal(string(encoded), "chat-test:"+fingerprint)
			if err == nil {
				err = model.DB.Model(&run).Update("result", sealed).Error
			}
		}
		if err != nil {
			mcpResponse(c, 0, nil, err)
			return
		}
		c.Data(200, "application/json", encoded)
	}
}

func runMCPConversation(c *gin.Context, relay http.Handler, server *model.MCPServer, actor *mcpsvc.Actor, modelName string, messages []mcpChatMessage, key, fingerprint string) map[string]any {
	ctx := c.Request.Context()
	calls := make([]any, 0, 10)
	modelCalls := make([]any, 0, 5)
	result := map[string]any{"status": "incomplete", "model": modelName}
	defer func() { result["tool_calls"] = calls; result["model_calls"] = modelCalls }()
	views, err := mcpsvc.AvailableTools(ctx, server, actor)
	if err != nil {
		result["error"] = mcpsvc.ErrorResponse(err)
		return result
	}
	if len(views) == 0 {
		result["error"] = "No enabled MCP tools are available"
		return result
	}
	names := map[string]string{}
	tools := make([]any, 0, len(views))
	for i, view := range views {
		name := fmt.Sprintf("mcp_tool_%d", i+1)
		names[name] = view.Name
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": name, "description": view.Name + ": " + view.Description, "parameters": view.InputSchema}})
	}
	for round := range 5 {
		requestBody, err := common.Marshal(map[string]any{"model": modelName, "messages": messages, "tools": tools, "stream": false})
		if err != nil || len(requestBody) > mcpsvc.MaxResultBytes {
			result["error"] = "Model input exceeds the test limit"
			return result
		}
		record := map[string]any{}
		relayContext := service.ObserveTextSettlement(ctx, func(quota int, err error) {
			if err != nil {
				record["billing_status"] = "pending"
				return
			}
			record["charged_quota"] = quota
			record["billing_status"] = "settled"
		})
		request, err := http.NewRequestWithContext(relayContext, http.MethodPost, "/v1/chat/completions", bytes.NewReader(requestBody))
		if err != nil {
			result["error"] = "Model request could not be prepared"
			return result
		}
		request.RemoteAddr = c.Request.RemoteAddr
		request.Host = c.Request.Host
		for _, header := range []string{"Authorization", "X-Forwarded-For", "X-Real-IP"} {
			request.Header.Set(header, c.GetHeader(header))
		}
		request.Header.Set("Content-Type", "application/json")
		response := &mcpRelayResponse{header: make(http.Header)}
		relay.ServeHTTP(response, request)
		record["request_id"] = response.Header().Get(common.RequestIdKey)
		modelCalls = append(modelCalls, record)
		var reply struct {
			Choices []struct {
				Message mcpChatMessage `json:"message"`
			} `json:"choices"`
			Usage any `json:"usage"`
			Error any `json:"error"`
		}
		if err := common.Unmarshal(response.body.Bytes(), &reply); err != nil || response.status >= 400 || len(reply.Choices) == 0 {
			result["error"] = reply.Error
			if reply.Error == nil {
				result["error"] = "Model request failed or returned no answer"
			}
			return result
		}
		record["usage"] = reply.Usage
		message := reply.Choices[0].Message
		if len(message.ToolCalls) == 0 {
			result["status"] = "completed"
			result["answer"] = message.Content
			return result
		}
		if round == 4 || len(message.ToolCalls) > 10-len(calls) {
			result["error"] = "MCP test reached its model or tool call limit"
			return result
		}
		messages = append(messages, message)
		for _, tool := range message.ToolCalls {
			original, valid := names[tool.Function.Name]
			if !valid || tool.Type != "function" || tool.ID == "" {
				result["error"] = "Model returned an invalid tool call"
				return result
			}
			var args map[string]any
			if common.UnmarshalJsonStr(tool.Function.Arguments, &args) != nil {
				result["error"] = "Model returned invalid tool arguments"
				return result
			}
			idempotency := mcpsvc.Digest(fingerprint + ":" + key + fmt.Sprintf(":%d", len(calls)))
			prepared, err := mcpsvc.PrepareCall(server.ID, original, actor, args, mcpsvc.InvocationOptions{RequestID: c.GetString(common.RequestIdKey), IdempotencyKey: idempotency})
			var content []byte
			if err == nil {
				output, callErr := prepared.Run(ctx, nil)
				err = callErr
				if err == nil {
					content, err = common.Marshal(map[string]any{"content": output.Content, "structuredContent": output.StructuredContent, "isError": output.IsError})
				}
			}
			call := map[string]any{"tool": original}
			if prepared != nil && prepared.Invocation != nil {
				var invocation model.MCPInvocation
				if model.DB.First(&invocation, "id = ?", prepared.Invocation.ID).Error == nil {
					call["invocation_id"] = invocation.ID
					call["charged_quota"] = invocation.ChargedQuota
					call["billing_status"] = invocation.BillingStatus
				}
			}
			if err != nil {
				failure := mcpsvc.ErrorResponse(err)
				call["error"] = failure
				content = []byte(failure.Message)
			}
			calls = append(calls, call)
			messages = append(messages, mcpChatMessage{Role: "tool", ToolCallID: tool.ID, Content: strings.ToValidUTF8(string(content), "")})
		}
	}
	return result
}
