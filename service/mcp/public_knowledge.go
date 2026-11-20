package mcp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
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
	base := strings.TrimRight(os.Getenv("MCP_KNOWLEDGE_ENGINE_URL"), "/")
	clientID, secret := os.Getenv("MCP_KNOWLEDGE_CLIENT_ID"), os.Getenv("MCP_KNOWLEDGE_CLIENT_SECRET")
	client, err := NewHTTPClient(base, time.Duration(server.Limits.TimeoutSeconds)*time.Second)
	if err != nil || clientID == "" || secret == "" {
		return nil, Failure(503, "ENGINE_APP_NOT_CONFIGURED", "Configure the trusted knowledge application")
	}
	defer client.CloseIdleConnections()
	body, err := common.Marshal(map[string]string{"access_token": app.AccessToken})
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/auth/app-exchange", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.SetBasicAuth(clientID, secret)
	response, err := client.Do(request)
	if err != nil {
		return nil, Failure(503, "ENGINE_UNAVAILABLE", "Knowledge engine is temporarily unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		if response.StatusCode == 401 || response.StatusCode == 403 {
			var failure struct{ Detail string }
			_ = common.DecodeJson(response.Body, &failure)
			switch failure.Detail {
			case "Invalid application credentials", "Application authentication required", "Unknown application", "Application is not enabled", "Application is not configured", "Invalid application audience", "Delegated applications require an independent client ID":
				return nil, Failure(503, "ENGINE_APP_NOT_CONFIGURED", "Verify the trusted application registration and exchange credentials")
			}
			return nil, Failure(401, "ENGINE_AUTHORIZATION_REQUIRED", "Authorize the knowledge application again")
		}
		return nil, Failure(503, "ENGINE_UNAVAILABLE", "Knowledge engine is temporarily unavailable")
	}
	var token struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if common.DecodeJson(response.Body, &token) != nil || !strings.EqualFold(token.TokenType, "Bearer") || token.AccessToken == "" || token.ExpiresIn <= 0 || token.ExpiresIn > 3600 {
		return nil, Failure(502, "TOKEN_EXCHANGE_FAILED", "Invalid knowledge engine authorization response")
	}
	expires := time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	if app.Expiry.Before(expires) {
		expires = app.Expiry
	}
	configured := *server
	configured.Connection.EngineBaseURL = base
	engine := &publicEngine{knowledgeClient: knowledgeClient{server: &configured, client: client, token: token.AccessToken}, expires: expires}
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
	business, cancel := context.WithDeadline(ctx, engine.expires)
	defer cancel()
	var data map[string]any
	if tool.Name == "create_consultation" {
		key := Digest(server.ID + "\x00" + strconv.Itoa(actor.UserID) + "\x00" + args["request_id"].(string))
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
		data, err = engine.snapshot(business, taskID, progress)
		if err != nil {
			return Outcome{}, err
		}
	}
	// The stream window is bounded. Recheck the grant, publication and public
	// flag immediately before delivering any newly read or accepted information.
	if _, err = publicLibraryClient(ctx, server, actor); err != nil {
		return Outcome{Unknown: tool.Name == "create_consultation"}, err
	}
	result, err := StructuredResult(data)
	return Outcome{Result: result, Billable: true}, err
}

func (engine *publicEngine) snapshot(ctx context.Context, taskID string, progress Progress) (map[string]any, error) {
	window, cancel := context.WithTimeout(ctx, 5*time.Second)
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
			answer.WriteString(text)
		case "citation":
			citations = append(citations, projectKnowledgeEvent(event))
		case "execution_progress":
			if progress != nil {
				progress("Consultation is running")
			}
		case "completed":
			status, done = "completed", true
		case "error":
			if status != "cancelled" {
				status = "failed"
			}
			done = true
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
