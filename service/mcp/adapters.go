package mcp

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptrace"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type Outcome struct {
	Result   *mcpsdk.CallToolResult
	Billable bool
	Unknown  bool
}
type Progress func(message string)

func StructuredResult(value any) (*mcpsdk.CallToolResult, error) {
	encoded, err := common.Marshal(value)
	if err != nil || len(encoded) > MaxResultBytes {
		return nil, Failure(502, "INVALID_UPSTREAM_RESULT", "Upstream result is invalid or too large")
	}
	return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: string(encoded)}}, StructuredContent: value}, nil
}

func DeepSeekConnection(server *model.MCPServer) (string, string, error) {
	channel, err := model.CacheGetChannel(server.Connection.ChannelID)
	if err != nil || channel.Type != constant.ChannelTypeDeepSeek || channel.Status != common.ChannelStatusEnabled {
		return "", "", Failure(503, "CHANNEL_UNAVAILABLE", "Referenced DeepSeek channel is unavailable")
	}
	base := strings.TrimSuffix(strings.TrimRight(channel.GetBaseURL(), "/"), "/v1")
	if base != server.Connection.ConfirmedBaseURL {
		return "", "", Failure(503, "UPSTREAM_ADDRESS_CHANGED", "The channel address changed and requires confirmation")
	}
	key, _, keyErr := channel.GetNextEnabledKey()
	if keyErr != nil || strings.TrimSpace(key) == "" {
		return "", "", Failure(503, "CHANNEL_UNAVAILABLE", "Referenced channel has no enabled credentials")
	}
	return base, key, nil
}

func ExecuteAdapter(ctx context.Context, server *model.MCPServer, tool *model.MCPTool, actor *Actor, args map[string]any, idempotencyKey string, progress Progress) (Outcome, error) {
	server, err := providerConfig(server)
	if err != nil {
		return Outcome{}, err
	}
	switch server.Kind {
	case "deepseek":
		base, key, err := DeepSeekConnection(server)
		if err != nil {
			return Outcome{}, err
		}
		client, err := NewHTTPClient(base, time.Duration(server.Limits.TimeoutSeconds)*time.Second)
		if err != nil {
			return Outcome{}, err
		}
		defer client.CloseIdleConnections()
		payload, err := common.Marshal(args)
		if err != nil {
			return Outcome{}, err
		}
		path := "/v1/tools/search"
		if tool.Name == "web_fetch" {
			path = "/v1/tools/fetch"
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(payload))
		if err != nil {
			return Outcome{}, err
		}
		request.Header.Set("Authorization", "Bearer "+key)
		request.Header.Set("Content-Type", "application/json")
		// Before a connection is obtained, no request can reach DeepSeek.
		// Once connected, preserve uncertainty even for partial writes or redirects.
		var connected atomic.Bool
		request = request.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
		}))
		response, err := client.Do(request)
		if err != nil {
			if !connected.Load() {
				var failure *Error
				if errors.As(err, &failure) {
					return Outcome{}, failure
				}
				return Outcome{}, Failure(502, "UPSTREAM_CONNECTION_FAILED", "The upstream connection failed before the request was sent")
			}
			return Outcome{Unknown: true}, Failure(502, "UPSTREAM_RESULT_UNKNOWN", "The upstream result could not be confirmed")
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return Outcome{}, Failure(502, "UPSTREAM_REJECTED", "DeepSeek did not return a successful result")
		}
		var data map[string]any
		if err = common.DecodeJson(response.Body, &data); err != nil {
			return Outcome{}, Failure(502, "INVALID_UPSTREAM_RESULT", "DeepSeek returned an invalid result")
		}
		if tool.Name == "web_search" {
			if _, ok := data["results"].([]any); !ok {
				return Outcome{}, Failure(502, "INVALID_UPSTREAM_RESULT", "DeepSeek search result is invalid")
			}
		} else if _, ok := data["content"].(string); !ok {
			return Outcome{}, Failure(502, "INVALID_UPSTREAM_RESULT", "DeepSeek fetch result is invalid")
		}
		result, err := StructuredResult(data)
		return Outcome{Result: result, Billable: true}, err
	case "public_knowledge":
		return executePublicLibrary(ctx, server, tool, actor, args, progress)
	case "remote_mcp":
		session, closeSession, err := connectRemote(ctx, server, actor.AccessToken)
		if err != nil {
			return Outcome{}, err
		}
		defer closeSession()
		if server.Catalog {
			directory, err := readUpstreamTools(ctx, session)
			if err != nil {
				return Outcome{}, err
			}
			match := false
			for _, current := range directory {
				if current.Name == tool.Name && current.SchemaHash == reviewedSchemaHash(tool) {
					match = true
					break
				}
			}
			if !match {
				return Outcome{}, Failure(409, "TOOL_CHANGED", "Tool is unavailable or its schema has changed")
			}
		}
		result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: tool.Name, Arguments: args})
		if err != nil {
			var rpcErr *jsonrpc.Error
			if errors.As(err, &rpcErr) {
				return Outcome{}, Failure(502, "UPSTREAM_REJECTED", "The tool rejected this request")
			}
			return Outcome{Unknown: true}, Failure(502, "UPSTREAM_RESULT_UNKNOWN", "The remote tool result could not be confirmed")
		}
		// Do not expose server-private upstream metadata; gateway metadata is
		// attached after final settlement.
		result.Meta = nil
		return Outcome{Result: result, Billable: !result.IsError}, nil
	default:
		return Outcome{}, Failure(422, "INVALID_SERVER_KIND", "Unsupported server type")
	}
}

func serverCredentials(server *model.MCPServer) (*CredentialsInput, error) {
	if server.Catalog {
		if server.RuntimeSecrets != "" {
			plain, err := Open(string(server.RuntimeSecrets), "runtime:"+server.ID)
			var credentials CredentialsInput
			if err != nil || common.UnmarshalJsonStr(plain, &credentials) != nil {
				return nil, Failure(503, "CREDENTIAL_UNAVAILABLE", "Server credentials are unavailable")
			}
			return &credentials, nil
		}
		if server.Kind == "knowledge" {
			return &CredentialsInput{ClientSecret: os.Getenv("MCP_KNOWLEDGE_CLIENT_SECRET")}, nil
		}
		return &CredentialsInput{}, nil
	}
	if server.CredentialCiphertext == "" {
		return &CredentialsInput{}, nil
	}
	plain, err := Open(server.CredentialCiphertext, "server:"+server.ID)
	if err != nil {
		return nil, err
	}
	var credentials CredentialsInput
	if common.UnmarshalJsonStr(plain, &credentials) != nil {
		return nil, Failure(503, "CREDENTIAL_UNAVAILABLE", "Server credentials are unavailable")
	}
	return &credentials, nil
}

type credentialTransport struct {
	base       http.RoundTripper
	headers    map[string]string
	challenged atomic.Bool
}

func (t *credentialTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	for key, value := range t.headers {
		copy.Header.Set(key, value)
	}
	response, err := t.base.RoundTrip(copy)
	if err == nil && response.StatusCode == http.StatusUnauthorized {
		t.challenged.Store(true)
	}
	return response, err
}
func (t *credentialTransport) CloseIdleConnections() {
	if transport, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		transport.CloseIdleConnections()
	}
}

func connectRemote(ctx context.Context, server *model.MCPServer, tokens ...string) (*mcpsdk.ClientSession, func(), error) {
	if server.Connection.Transport == "stdio" {
		return connectStdio(ctx, server)
	}
	client, err := NewHTTPClient(server.Connection.URL, time.Duration(server.Limits.TimeoutSeconds)*time.Second)
	if err != nil {
		return nil, nil, err
	}
	credentials, err := serverCredentials(server)
	if err != nil {
		return nil, nil, err
	}
	if server.Catalog && server.AuthMode == "oauth2" {
		if len(tokens) != 1 || tokens[0] == "" {
			client.CloseIdleConnections()
			return nil, nil, Failure(401, "REAUTHORIZATION_REQUIRED", "Authorize the MCP service before connecting")
		}
		if credentials.Headers == nil {
			credentials.Headers = make(map[string]string)
		}
		credentials.Headers["Authorization"] = "Bearer " + tokens[0]
	}
	withCredentials := &credentialTransport{base: client.Transport, headers: credentials.Headers}
	client.Transport = withCredentials
	var transport mcpsdk.Transport = &mcpsdk.StreamableClientTransport{Endpoint: server.Connection.URL, HTTPClient: client, MaxRetries: -1, DisableStandaloneSSE: true}
	if server.Connection.Transport == "http_sse" {
		transport = &mcpsdk.SSEClientTransport{Endpoint: server.Connection.URL, HTTPClient: client}
	}
	session, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "new-api-mcp", Version: common.Version}, nil).Connect(ctx, transport, nil)
	if err != nil {
		client.CloseIdleConnections()
		if withCredentials.challenged.Load() {
			if server.AuthMode == "oauth2" {
				return nil, nil, Failure(401, "REAUTHORIZATION_REQUIRED", "The MCP service rejected the authorization. Connect it again")
			}
			return nil, nil, Failure(401, "REAUTHORIZATION_REQUIRED", "Authorize discovery for this service")
		}
		var upstreamFailure *Error
		if errors.As(err, &upstreamFailure) {
			return nil, nil, upstreamFailure
		}
		return nil, nil, Failure(502, "UPSTREAM_CONNECT_FAILED", "Remote MCP handshake failed")
	}
	return session, func() { session.Close(); client.CloseIdleConnections() }, nil
}

func engineToken(ctx context.Context, server *model.MCPServer, actor *Actor, client *http.Client) (string, error) {
	if actor.AccessToken == "" {
		return "", Failure(401, "TEST_AUTHORIZATION_REQUIRED", "Authorize this MCP service with your own Casdoor account")
	}
	credentials, err := serverCredentials(server)
	if err != nil {
		return "", err
	}
	encoded, err := common.Marshal(map[string]any{"subject_token": actor.AccessToken, "resource": ResourceURL(server), "mcp_server_id": server.ID})
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.Connection.EngineBaseURL+"/v1/auth/mcp-exchange", bytes.NewReader(encoded))
	if err != nil {
		return "", err
	}
	request.SetBasicAuth(server.Connection.ClientID, credentials.ClientSecret)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return "", Failure(503, "ENGINE_UNAVAILABLE", "Knowledge engine is unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		if response.StatusCode == 403 {
			return "", Failure(403, "IDENTITY_NOT_LINKED", "Knowledge service authorization is unavailable for this identity")
		}
		return "", Failure(502, "TOKEN_EXCHANGE_FAILED", "Knowledge engine token exchange failed")
	}
	var result struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if common.DecodeJson(response.Body, &result) != nil || result.AccessToken == "" || result.TokenType != "Bearer" || result.ExpiresIn <= 0 || result.ExpiresIn > 300 {
		return "", Failure(502, "TOKEN_EXCHANGE_FAILED", "Knowledge engine returned an invalid exchange result")
	}
	return result.AccessToken, nil
}
