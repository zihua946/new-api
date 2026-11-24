package mcp

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"gorm.io/gorm/clause"
)

func libraryIdentifier(value string) bool {
	return value != "" && len(value) <= 256 && value != "." && value != ".." && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "/\\%?#") && strings.IndexFunc(value, unicode.IsControl) < 0
}

func libraryPath(server *model.MCPServer, suffix string) string {
	return "/v1/knowledge/kb/" + url.PathEscape(server.Connection.KBID) + suffix + "?" + url.Values{"owner": {server.Connection.Owner}}.Encode()
}

// PublicLibraryURL identifies a fixed library independently of its catalog row.
func PublicLibraryURL(owner, kbID string) string {
	return PublicBaseURL() + "/mcp/providers/knowledge/" + url.PathEscape(owner) + "/" + url.PathEscape(kbID)
}

func publicLibraryConnection(endpoint string) (model.MCPConnection, bool) {
	base := PublicBaseURL()
	path, ok := strings.CutPrefix(endpoint, base+"/mcp/providers/knowledge/")
	if base == "" || !ok {
		return model.MCPConnection{}, false
	}
	owner, kbID, ok := strings.Cut(path, "/")
	if !ok {
		return model.MCPConnection{}, false
	}
	owner, ownerErr := url.PathUnescape(owner)
	kbID, kbErr := url.PathUnescape(kbID)
	if ownerErr != nil || kbErr != nil || !libraryIdentifier(owner) || !libraryIdentifier(kbID) || endpoint != PublicLibraryURL(owner, kbID) {
		return model.MCPConnection{}, false
	}
	return model.MCPConnection{URL: endpoint, Transport: "streamable_http", Owner: owner, KBID: kbID}, true
}

type publicEngine struct {
	knowledgeClient
	expires time.Time
}

func publicLibraryClient(ctx context.Context, server *model.MCPServer, actor *Actor) (*publicEngine, error) {
	current, err := model.GetMCPServer(server.ID)
	if err != nil || current.Kind != "public_knowledge" || current.Connection != server.Connection || !current.Enabled && !actor.Draft || current.ConnectionRevision != server.ConnectionRevision {
		return nil, Failure(404, "RESOURCE_NOT_FOUND", "Public library is unavailable")
	}
	if actor.InstallationID != "" {
		var count int64
		if err = model.DB.Model(&model.MCPInstallation{}).Where("id = ? AND server_id = ? AND user_id = ?", actor.InstallationID, server.ID, actor.UserID).Where("admin = ? OR (authorized = ? AND connection_revision = ?)", true, true, current.ConnectionRevision).Count(&count).Error; err != nil {
			return nil, err
		}
		if count != 1 {
			return nil, Failure(403, "INSTALLATION_UNAVAILABLE", "The installation is no longer available")
		}
	}
	if actor.AccessToken != "" {
		verified, err := AuthenticateOAuth(ctx, server, actor.AccessToken)
		if err != nil {
			return nil, err
		}
		if !verified.CanCall || verified.UserID != actor.UserID {
			return nil, Failure(403, "IDENTITY_MISMATCH", "Resource access no longer matches the caller")
		}
	}
	app, subject, err := loadEngineGrant(ctx, actor.UserID)
	if err != nil {
		return nil, err
	}
	if actor.Subject != "" && actor.Subject != subject {
		return nil, Failure(403, "IDENTITY_MISMATCH", "The connected application belongs to a different user")
	}
	base := strings.TrimRight(ConfigValue("knowledge_engine_url", ctx), "/")
	clientID, secret := ConfigValue("knowledge_client_id", ctx), ConfigValue("knowledge_client_secret", ctx)
	client, err := requestHTTPClient(ctx, base, time.Duration(server.Limits.TimeoutSeconds)*time.Second)
	if err != nil || clientID == "" || secret == "" {
		return nil, Failure(503, "ENGINE_APP_NOT_CONFIGURED", "Configure the trusted knowledge application")
	}
	defer client.CloseIdleConnections()
	entry, cacheKey, err := knowledgeEngineToken(ctx, actor.UserID, subject, base, clientID, secret, app, time.Duration(server.Limits.TimeoutSeconds)*time.Second)
	if err != nil {
		return nil, err
	}
	expires := entry.expires
	configured := *server
	configured.Connection.EngineBaseURL = base
	engine := &publicEngine{knowledgeClient: knowledgeClient{server: &configured, client: client, token: entry.token, engineTokenCacheKey: cacheKey}, expires: expires}
	check, cancel := context.WithDeadline(ctx, expires)
	defer cancel()
	var library struct {
		ID, Owner string
		Public    bool `json:"public_read"`
	}
	if err = engine.request(check, http.MethodGet, libraryPath(server, ""), nil, "", &library); err != nil {
		return nil, err
	}
	if library.ID != server.Connection.KBID || library.Owner != server.Connection.Owner || !library.Public {
		return nil, Failure(404, "RESOURCE_NOT_FOUND", "Public library is unavailable")
	}
	return engine, nil
}

func executePublicLibrary(ctx context.Context, server *model.MCPServer, tool *model.MCPTool, actor *Actor, args map[string]any, progress Progress) (Outcome, error) {
	pool := &requestHTTPPool{}
	defer pool.Close()
	ctx = context.WithValue(ctx, requestHTTPPoolKey{}, pool)
	// Validate ownership before contacting any engine business route.
	taskID, _ := args["task_id"].(string)
	sessionID, _ := args["session_id"].(string)
	if tool.Name == "get_consultation_result" {
		if _, err := knowledgeBinding(server, actor, taskID); err != nil {
			return Outcome{}, err
		}
	} else if tool.Name == "create_consultation" {
		question, _ := args["question"].(string)
		if strings.TrimSpace(question) == "" {
			return Outcome{}, Failure(422, "INVALID_ARGUMENTS", "Supply a question")
		}
		if sessionID != "" {
			var binding model.MCPTaskBinding
			err := model.DB.Where("server_id = ? AND user_id = ? AND session_id = ?", server.ID, actor.UserID, sessionID).First(&binding).Error
			if err != nil || binding.SessionID != sessionID || binding.KnowledgeBase["owner"] != server.Connection.Owner || binding.KnowledgeBase["kb_id"] != server.Connection.KBID {
				return Outcome{}, Failure(404, "RESOURCE_NOT_FOUND", "Consultation session is unavailable")
			}
		}
	} else {
		return Outcome{}, Failure(404, "TOOL_NOT_FOUND", "Knowledge tool is unavailable")
	}
	engine, err := publicLibraryClient(ctx, server, actor)
	if err != nil {
		return Outcome{}, err
	}
	defer engine.client.CloseIdleConnections()
	expires := engine.expires
	if !actor.ExpiresAt.IsZero() && actor.ExpiresAt.Before(expires) {
		expires = actor.ExpiresAt
	}
	business, cancel := context.WithDeadline(ctx, expires)
	defer cancel()
	var data map[string]any
	if tool.Name == "create_consultation" {
		requestID, _ := args["request_id"].(string)
		if requestID == "" {
			requestID = common.NewRequestId()
		}
		key := Digest(server.ID + "\x00" + strconv.Itoa(actor.UserID) + "\x00" + requestID)
		payload := map[string]any{"question": args["question"]}
		if sessionID != "" {
			payload["session_id"] = sessionID
		}
		if err = engine.request(business, http.MethodPost, libraryPath(server, "/chat"), payload, key, &data); err != nil {
			return Outcome{Unknown: ErrorResponse(err).Status >= 500}, err
		}
		taskID, _ = data["task_id"].(string)
		acceptedSession, _ := data["session_id"].(string)
		if !knowledgeIdentifier(taskID) || !knowledgeIdentifier(acceptedSession) || sessionID != "" && sessionID != acceptedSession {
			return Outcome{Unknown: true}, Failure(502, "INVALID_UPSTREAM_RESULT", "Invalid consultation acceptance")
		}
		binding := &model.MCPTaskBinding{ID: Digest(server.ID + "\x00" + strconv.Itoa(actor.UserID) + "\x00" + taskID), ServerID: server.ID, UserID: actor.UserID, TaskID: taskID, SessionID: acceptedSession, KnowledgeBase: model.MCPJSON{"owner": server.Connection.Owner, "kb_id": server.Connection.KBID}}
		if err = model.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(binding).Error; err != nil {
			return Outcome{Unknown: true}, err
		}
		stored, err := knowledgeBinding(server, actor, taskID)
		if err != nil || stored.SessionID != acceptedSession {
			return Outcome{Unknown: true}, Failure(409, "IDEMPOTENCY_CONFLICT", "Consultation binding changed")
		}
		data = map[string]any{"task_id": taskID, "session_id": acceptedSession}
	} else {
		// Authorization belongs to this bounded invocation, not individual deltas.
		data, err = engine.snapshot(business, taskID, progress)
		if err != nil {
			return Outcome{}, err
		}
	}
	if err = business.Err(); err != nil {
		return Outcome{Unknown: tool.Name == "create_consultation"}, err
	}
	// Creation retains its post-acceptance check; result reads authorize once.
	if tool.Name == "create_consultation" {
		if _, err = publicLibraryClient(ctx, server, actor); err != nil {
			return Outcome{Unknown: true}, err
		}
	}

	result, err := StructuredResult(data)
	return Outcome{Result: result, Billable: true}, err
}

func (engine *publicEngine) snapshot(ctx context.Context, taskID string, progress Progress) (map[string]any, error) {
	timeout := 5 * time.Second
	if progress != nil {
		timeout = time.Duration(engine.server.Limits.TimeoutSeconds) * time.Second
	}
	window, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(window, http.MethodGet, engine.server.Connection.EngineBaseURL+"/v1/tasks/"+url.PathEscape(taskID)+"/stream", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+engine.token)
	resp, err := engine.client.Do(req)
	if err != nil {
		return nil, Failure(503, "ENGINE_UNAVAILABLE", "Task stream could not be opened")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 403 || resp.StatusCode == 404 {
		return nil, Failure(404, "RESOURCE_NOT_FOUND", "Consultation is unavailable")
	}
	if resp.StatusCode == 401 {
		knowledgeEngineTokens.invalidate(engine.engineTokenCacheKey, engine.token)
		return nil, Failure(401, "ENGINE_AUTHORIZATION_REQUIRED", "Reconnect the knowledge application")
	}
	if resp.StatusCode != 200 || !strings.EqualFold(strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]), "text/event-stream") {
		return nil, Failure(502, "INVALID_UPSTREAM_RESULT", "Expected a knowledge task event stream")
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), MaxResultBytes)
	var frame, answer strings.Builder
	citations := []map[string]any{}
	status := ""
	done := false
	invalid := Failure(502, "INVALID_UPSTREAM_RESULT", "Invalid or incomplete knowledge task stream")
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line := scanner.Text()
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			if frame.Len() > 0 {
				frame.WriteByte('\n')
			}
			frame.WriteString(strings.TrimPrefix(value, " "))
			continue
		}
		if line != "" || frame.Len() == 0 {
			continue
		}
		var event map[string]any
		if common.UnmarshalJsonStr(frame.String(), &event) != nil {
			return nil, invalid
		}
		frame.Reset()
		if event["type"] == "connected" {
			if status != "" || event["task_id"] != taskID || !knowledgeStatus(event["status"]) {
				return nil, invalid
			}
			status = event["status"].(string)
			if progress != nil {
				if err := progress("Consultation connected", map[string]any{"task_id": taskID, "type": "status", "status": status}); err != nil {
					return nil, err
				}
			}
			continue
		}
		if status == "" {
			return nil, invalid
		}
		switch event["type"] {
		case "text_delta":
			text, ok := event["content"].(string)
			if !ok {
				return nil, invalid
			}
			if answer.Len()+len(text) > MaxResultBytes {
				return nil, invalid
			}
			answer.WriteString(text)
		case "citation":
			citations = append(citations, projectKnowledgeEvent(event))
		case "completed":
			status, done = "completed", true
		case "error":
			if status != "cancelled" {
				status = "failed"
			}
			done = true
		}
		if progress != nil {
			if projected := projectKnowledgeEvent(event); projected != nil {
				projected["task_id"] = taskID
				if done {
					projected["status"] = status
				}
				if err := progress("Consultation update", projected); err != nil {
					return nil, err
				}
			}
		}
		if done {
			break
		}
	}
	if !done {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if status == "" || !errors.Is(window.Err(), context.DeadlineExceeded) || status == "completed" || status == "failed" || status == "cancelled" {
			return nil, invalid
		}
	}
	return map[string]any{"task_id": taskID, "status": status, "answer": answer.String(), "citations": citations, "done": done}, nil
}
