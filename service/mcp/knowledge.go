package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type knowledgeClient struct {
	client              *http.Client
	server              *model.MCPServer
	token               string
	engineTokenCacheKey string
}

func (engine *knowledgeClient) request(ctx context.Context, method, path string, payload any, key string, target any) error {
	var body bytes.Buffer
	if payload != nil {
		encoded, err := common.Marshal(payload)
		if err != nil {
			return err
		}
		body.Write(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, engine.server.Connection.EngineBaseURL+path, &body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+engine.token)
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response, err := engine.client.Do(request)
	if err != nil {
		return Failure(502, "ENGINE_UNAVAILABLE", "Knowledge engine request did not complete")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusUnauthorized && engine.engineTokenCacheKey != "" {
			knowledgeEngineTokens.invalidate(engine.engineTokenCacheKey, engine.token)
			return Failure(401, "ENGINE_AUTHORIZATION_REQUIRED", "Knowledge engine authorization is no longer valid")
		}
		status, code := response.StatusCode, "ENGINE_REQUEST_FAILED"
		switch status {
		case 403, 404:
			status, code = 404, "RESOURCE_NOT_FOUND"
		case 409:
			code = "IDEMPOTENCY_CONFLICT"
		}
		if status >= 500 {
			status = 502
		}
		return Failure(status, code, "Knowledge engine did not accept this request")
	}
	if common.DecodeJson(response.Body, target) != nil {
		return Failure(502, "INVALID_UPSTREAM_RESULT", "Knowledge engine result is invalid or exceeds the response limit")
	}
	return nil
}

func knowledgeBinding(server *model.MCPServer, actor *Actor, taskID string) (*model.MCPTaskBinding, error) {
	var binding model.MCPTaskBinding
	err := model.DB.Where("server_id = ? AND user_id = ? AND task_id = ?", server.ID, actor.UserID, taskID).First(&binding).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, Failure(404, "RESOURCE_NOT_FOUND", "Consultation is unavailable")
	}
	if err != nil {
		return nil, err
	}
	if binding.TaskID != taskID || server.Kind == "public_knowledge" && (binding.KnowledgeBase["owner"] != server.Connection.Owner || binding.KnowledgeBase["kb_id"] != server.Connection.KBID) {
		return nil, Failure(404, "RESOURCE_NOT_FOUND", "Consultation is unavailable")
	}
	return &binding, nil
}

func (engine *knowledgeClient) task(ctx context.Context, binding *model.MCPTaskBinding) (map[string]any, error) {
	var data map[string]any
	if err := engine.request(ctx, http.MethodGet, "/v1/tasks/"+url.PathEscape(binding.TaskID), nil, "", &data); err != nil {
		return nil, err
	}
	if data["session_id"] != binding.SessionID || !knowledgeStatus(data["status"]) {
		return nil, Failure(404, "RESOURCE_NOT_FOUND", "Consultation is unavailable")
	}
	return data, nil
}

func knowledgeStatus(value any) bool {
	switch value {
	case "pending", "queued", "starting", "running", "cancelling", "completed", "failed", "cancelled":
		return true
	}
	return false
}

func executeKnowledge(ctx context.Context, server *model.MCPServer, tool *model.MCPTool, actor *Actor, args map[string]any, progress Progress) (Outcome, error) {
	var binding *model.MCPTaskBinding
	var err error
	if tool.Name == "get_consultation_result" || tool.Name == "cancel_consultation" {
		binding, err = knowledgeBinding(server, actor, args["task_id"].(string))
		if err != nil {
			return Outcome{}, err
		}
	}
	if tool.Name == "create_consultation" {
		if sessionID, _ := args["session_id"].(string); sessionID != "" {
			var source model.MCPTaskBinding
			err = model.DB.Where("server_id = ? AND user_id = ? AND session_id = ?", server.ID, actor.UserID, sessionID).Order("created_at").First(&source).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return Outcome{}, err
			}
			kb := args["knowledge_base"].(map[string]any)
			if err != nil || source.KnowledgeBase["kb_id"] != kb["kb_id"] || source.KnowledgeBase["owner"] != kb["owner"] {
				return Outcome{}, Failure(404, "RESOURCE_NOT_FOUND", "Consultation session is unavailable")
			}
		}
	}
	client, err := NewHTTPClient(server.Connection.EngineBaseURL, time.Duration(max(server.Limits.TimeoutSeconds, 30))*time.Second)
	if err != nil {
		return Outcome{}, err
	}
	defer client.CloseIdleConnections()
	token, err := engineToken(ctx, server, actor, client)
	if err != nil {
		return Outcome{}, err
	}
	engine := knowledgeClient{client: client, server: server, token: token}
	var data map[string]any
	switch tool.Name {
	case "list_knowledge_bases":
		data, err = engine.list(ctx, actor, args)
	case "create_consultation":
		kb := args["knowledge_base"].(map[string]any)
		key := Digest(server.ID + "\x00" + strconv.Itoa(actor.UserID) + "\x00" + args["request_id"].(string))
		path := "/v1/knowledge/kb/" + url.PathEscape(kb["kb_id"].(string)) + "/chat?" + url.Values{"owner": {kb["owner"].(string)}}.Encode()
		payload := map[string]any{"question": args["question"], "session_id": args["session_id"]}
		err = engine.request(ctx, http.MethodPost, path, payload, key, &data)
		if err != nil {
			return Outcome{Unknown: ErrorResponse(err).Status >= 500}, err
		}
		taskID, _ := data["task_id"].(string)
		sessionID, _ := data["session_id"].(string)
		if !knowledgeIdentifier(taskID) || !knowledgeIdentifier(sessionID) {
			return Outcome{Unknown: true}, Failure(502, "INVALID_UPSTREAM_RESULT", "Knowledge engine task identifiers are missing")
		}
		binding = &model.MCPTaskBinding{ID: Digest(server.ID + "\x00" + strconv.Itoa(actor.UserID) + "\x00" + taskID), ServerID: server.ID, UserID: actor.UserID, TaskID: taskID, SessionID: sessionID, KnowledgeBase: model.MCPJSON(kb)}
		if err = model.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(binding).Error; err != nil {
			return Outcome{Unknown: true}, err
		}
		stored, err := knowledgeBinding(server, actor, taskID)
		if err != nil {
			return Outcome{Unknown: true}, err
		}
		if stored.SessionID != sessionID || stored.KnowledgeBase["kb_id"] != kb["kb_id"] || stored.KnowledgeBase["owner"] != kb["owner"] {
			return Outcome{Unknown: true}, Failure(409, "IDEMPOTENCY_CONFLICT", "Consultation binding does not match")
		}
		current, err := engine.task(ctx, stored)
		if err != nil {
			return Outcome{Unknown: true}, err
		}
		data = map[string]any{"task_id": taskID, "session_id": sessionID, "status": current["status"]}
	case "get_consultation_result":
		data, err = engine.results(ctx, actor, binding, args, progress)
	case "cancel_consultation":
		current, taskErr := engine.task(ctx, binding)
		if taskErr != nil {
			return Outcome{}, taskErr
		}
		if current["status"] == "completed" || current["status"] == "failed" || current["status"] == "cancelled" {
			data = map[string]any{"task_id": binding.TaskID, "cancel_requested": false, "status": current["status"]}
		} else {
			err = engine.request(ctx, http.MethodPost, "/v1/tasks/"+url.PathEscape(binding.TaskID)+"/cancel", map[string]any{}, "", &data)
			if err == nil {
				accepted, ok := data["cancel_requested"].(bool)
				if !ok || !knowledgeStatus(data["status"]) {
					return Outcome{}, Failure(502, "INVALID_UPSTREAM_RESULT", "Invalid cancellation result")
				}
				data = map[string]any{"task_id": binding.TaskID, "cancel_requested": accepted, "status": data["status"]}
			}
		}
	default:
		return Outcome{}, Failure(404, "TOOL_NOT_FOUND", "Knowledge tool is unavailable")
	}
	if err != nil {
		return Outcome{}, err
	}
	result, err := StructuredResult(data)
	return Outcome{Result: result, Billable: server.Catalog}, err
}

func knowledgeIdentifier(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

type knowledgeListCursor struct {
	Owner   string `json:"owner"`
	KBID    string `json:"kb_id"`
	Expires int64  `json:"expires"`
}

func (engine *knowledgeClient) list(ctx context.Context, actor *Actor, args map[string]any) (map[string]any, error) {
	associated := fmt.Sprintf("knowledge-list:%s:%d", engine.server.ID, actor.UserID)
	after := knowledgeListCursor{}
	if cursor, _ := args["cursor"].(string); cursor != "" {
		plain, err := Open(cursor, associated)
		if err != nil || common.UnmarshalJsonStr(plain, &after) != nil || after.Expires <= time.Now().Unix() {
			return nil, Failure(410, "CURSOR_EXPIRED", "Knowledge list cursor is unavailable")
		}
	}
	var rows []struct {
		ID      string `json:"id"`
		Owner   string `json:"owner"`
		Name    string `json:"name"`
		CanRead bool   `json:"can_read"`
	}
	if err := engine.request(ctx, http.MethodGet, "/v1/knowledge/kb", nil, "", &rows); err != nil {
		return nil, err
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Owner == rows[j].Owner {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].Owner < rows[j].Owner
	})
	items := []map[string]any{}
	var next any
	for _, row := range rows {
		if !row.CanRead || row.Owner == "" || row.ID == "" || row.Owner < after.Owner || row.Owner == after.Owner && row.ID <= after.KBID {
			continue
		}
		if len(items) >= int(args["limit"].(float64)) {
			encoded, err := common.Marshal(after)
			if err != nil {
				return nil, err
			}
			next, err = Seal(string(encoded), associated)
			if err != nil {
				return nil, err
			}
			break
		}
		items = append(items, map[string]any{"knowledge_base": map[string]any{"kb_id": row.ID, "owner": row.Owner}, "name": row.Name, "can_query": true})
		after = knowledgeListCursor{Owner: row.Owner, KBID: row.ID, Expires: time.Now().Add(24 * time.Hour).Unix()}
	}
	return map[string]any{"items": items, "next_cursor": next}, nil
}

type knowledgeEventCursor struct {
	Position  int    `json:"position"`
	Part      int    `json:"part"`
	Prefix    string `json:"prefix"`
	Execution string `json:"execution"`
	Expires   int64  `json:"expires"`
}

// The cursor authenticates the durable log prefix (including a partially read
// event), not connection-local SSE indexes. It survives archive and restart.
func knowledgePrefix(events []map[string]any, cursor knowledgeEventCursor) (string, error) {
	if cursor.Position < 0 || cursor.Part < 0 || cursor.Position > len(events) || cursor.Part > 0 && cursor.Position == len(events) {
		return "", Failure(410, "CURSOR_EXPIRED", "Result cursor can no longer be resumed")
	}
	end := cursor.Position
	if cursor.Part > 0 {
		end++
	}
	if end > len(events) {
		return "", Failure(410, "CURSOR_EXPIRED", "Result cursor can no longer be resumed")
	}
	encoded, err := common.Marshal(events[:end])
	if err != nil {
		return "", err
	}
	return Digest(string(encoded)), nil
}

func projectKnowledgeEvent(event map[string]any) map[string]any {
	switch event["type"] {
	case "text_delta":
		if text, ok := event["content"].(string); ok {
			return map[string]any{"type": "answer_delta", "text": text}
		}
	case "citation":
		result := map[string]any{"type": "citation"}
		for _, key := range []string{"title", "url", "reference"} {
			if text, ok := event[key].(string); ok {
				result[key] = text
			}
		}
		return result
	case "completed":
		return map[string]any{"type": "status", "status": "completed"}
	case "status_change":
		if knowledgeStatus(event["status"]) {
			return map[string]any{"type": "status", "status": event["status"]}
		}
	case "execution_progress":
		return map[string]any{"type": "progress", "message": "Consultation is running"}
	case "error":
		return map[string]any{"type": "status", "status": "failed", "error_code": "CONSULTATION_FAILED"}
	}
	return nil
}

func (engine *knowledgeClient) results(ctx context.Context, actor *Actor, binding *model.MCPTaskBinding, args map[string]any, progress Progress) (map[string]any, error) {
	associated := fmt.Sprintf("knowledge-events:%s:%d:%s", engine.server.ID, actor.UserID, binding.TaskID)
	cursor := knowledgeEventCursor{}
	if value, _ := args["cursor"].(string); value != "" {
		plain, err := Open(value, associated)
		if err != nil || common.UnmarshalJsonStr(plain, &cursor) != nil || cursor.Expires <= time.Now().Unix() {
			return nil, Failure(410, "CURSOR_EXPIRED", "Result cursor can no longer be resumed")
		}
	}
	deadline := time.Now().Add(time.Duration(args["wait_seconds"].(float64)) * time.Second)
	for {
		current, err := engine.task(ctx, binding)
		if err != nil {
			return nil, err
		}
		execution := binding.TaskID
		if metadata, ok := current["execution_progress"].(map[string]any); ok {
			if id, _ := metadata["execution_id"].(string); id != "" {
				execution = id
			}
		}
		var log struct {
			Events []map[string]any `json:"events"`
		}
		if err = engine.request(ctx, http.MethodGet, "/v1/tasks/"+url.PathEscape(binding.TaskID)+"/events?offset=0", nil, "", &log); err != nil {
			return nil, err
		}
		if log.Events == nil {
			return nil, Failure(502, "INVALID_UPSTREAM_RESULT", "Knowledge engine events are missing")
		}
		prefix, err := knowledgePrefix(log.Events, cursor)
		if err != nil {
			return nil, err
		}
		if cursor.Expires != 0 && (cursor.Prefix != prefix || cursor.Execution != execution) {
			return nil, Failure(410, "CURSOR_EXPIRED", "Result cursor can no longer be resumed")
		}
		output := []map[string]any{}
		size := 0
		for cursor.Position < len(log.Events) && len(output) < int(args["limit"].(float64)) {
			event := projectKnowledgeEvent(log.Events[cursor.Position])
			if event == nil {
				cursor.Position++
				cursor.Part = 0
				continue
			}
			next := cursor
			if event["type"] == "answer_delta" {
				text := []rune(event["text"].(string))
				if cursor.Part > len(text) {
					return nil, Failure(410, "CURSOR_EXPIRED", "Result cursor can no longer be resumed")
				}
				end := min(len(text), cursor.Part+8192)
				event["text"] = string(text[cursor.Part:end])
				next.Part = end
				if end == len(text) {
					next.Position++
					next.Part = 0
				}
			} else {
				next.Position++
				next.Part = 0
			}
			encoded, err := common.Marshal(event)
			if err != nil || len(encoded) > 65536 {
				return nil, Failure(502, "INVALID_UPSTREAM_RESULT", "Knowledge event exceeds the response limit")
			}
			if size+len(encoded) > 1<<20 {
				break
			}
			cursor = next
			output = append(output, event)
			size += len(encoded)
		}
		// Recheck current authorization and execution after reading the log. A task
		// completing during this read is observed on the next call, never marked done early.
		verified, err := engine.task(ctx, binding)
		if err != nil {
			return nil, err
		}
		confirmedExecution := binding.TaskID
		if metadata, ok := verified["execution_progress"].(map[string]any); ok {
			if id, _ := metadata["execution_id"].(string); id != "" {
				confirmedExecution = id
			}
		}
		if confirmedExecution != execution {
			return nil, Failure(410, "CURSOR_EXPIRED", "Consultation execution changed")
		}
		finished := current["status"] == "completed" || current["status"] == "failed" || current["status"] == "cancelled"
		more := cursor.Position < len(log.Events)
		cursor.Execution, cursor.Expires = execution, time.Now().Add(24*time.Hour).Unix()
		cursor.Prefix, err = knowledgePrefix(log.Events, cursor)
		if err != nil {
			return nil, err
		}
		if len(output) > 0 || more || finished || !time.Now().Before(deadline) {
			encoded, err := common.Marshal(cursor)
			if err != nil {
				return nil, err
			}
			next, err := Seal(string(encoded), associated)
			if err != nil {
				return nil, err
			}
			if progress != nil && len(output) > 0 {
				progress("Consultation results are arriving")
			}
			return map[string]any{"task_id": binding.TaskID, "status": current["status"], "events": output, "next_cursor": next, "has_more": more, "task_finished": finished, "done": finished && !more}, nil
		}
		timer := time.NewTimer(min(time.Second, time.Until(deadline)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
