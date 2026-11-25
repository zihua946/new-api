package mcp

import (
	"context"
	"regexp"
	"slices"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/google/uuid"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"gorm.io/gorm"
)

type ToolView struct {
	model.MCPTool
	Pricing *PriceQuote `json:"pricing"`
}

var toolNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,128}$`)

func ListTools(serverID string) ([]ToolView, error) {
	var tools []model.MCPTool
	if err := model.DB.Where("server_id = ?", serverID).Order("name").Find(&tools).Error; err != nil {
		return nil, err
	}
	views := make([]ToolView, 0, len(tools))
	for _, tool := range tools {
		quote, _ := toolQuote(&tool)
		if quote != nil {
			quote.PriceRevision = tool.PriceRevision
		}
		views = append(views, ToolView{MCPTool: tool, Pricing: quote})
	}
	return views, nil
}

type ToolPatch struct {
	ExpectedRevision   int64            `json:"expected_revision"`
	Description        *string          `json:"description"`
	Enabled            *bool            `json:"enabled"`
	AllowedUserGroups  *model.MCPGroups `json:"allowed_user_groups"`
	AcceptedSchemaHash string           `json:"accepted_schema_hash"`
}

func PatchTool(serverID, toolID string, input ToolPatch, sensitive bool) error {
	server, err := model.GetMCPServer(serverID)
	if err != nil {
		return err
	}
	var tool model.MCPTool
	if err = model.DB.Where("id = ? AND server_id = ?", toolID, serverID).First(&tool).Error; err != nil {
		return err
	}
	updates := map[string]any{}
	if input.Description != nil {
		if len(*input.Description) > 4096 {
			return Failure(422, "INVALID_DESCRIPTION", "Tool description is too long")
		}
		updates["description"] = *input.Description
	}
	if input.AllowedUserGroups != nil {
		if !sensitive {
			return Failure(403, "PERMISSION_DENIED", "Sensitive MCP permission is required to change tool access")
		}
		for _, group := range *input.AllowedUserGroups {
			if !slices.Contains(server.AllowedUserGroups, group) {
				return Failure(422, "INVALID_GROUPS", "Tool groups must be a subset of server groups")
			}
		}
		groups := *input.AllowedUserGroups
		if len(groups) == 0 {
			groups = nil // An empty override restores inheritance from the server.
		}
		updates["allowed_user_groups"] = groups
	}
	if input.AcceptedSchemaHash != "" {
		if input.AcceptedSchemaHash != tool.SchemaHash {
			return Failure(409, "SCHEMA_CHANGED", "Tool schema changed; review it again")
		}
		updates["review_required"] = false
		tool.ReviewRequired = false
	}
	if input.Enabled != nil {
		if *input.Enabled {
			if tool.Removed || tool.ReviewRequired {
				return Failure(409, "TOOL_REVIEW_REQUIRED", "Review the current tool schema before enabling it")
			}
			if _, err = toolQuote(&tool); err != nil {
				return err
			}
		}
		updates["enabled"] = *input.Enabled
	}
	return model.UpdateMCPTool(serverID, toolID, input.ExpectedRevision, updates)
}

func SetToolPrice(serverID, toolID string, revision int64, mode, price string, currency ...string) (*PriceQuote, error) {
	if _, err := model.GetMCPServer(serverID); err != nil {
		return nil, err
	}
	quote, err := QuotePrice(mode, price, currency...)
	if err != nil {
		return nil, err
	}
	var tool model.MCPTool
	if err = model.DB.Where("id = ? AND server_id = ?", toolID, serverID).First(&tool).Error; err != nil {
		return nil, err
	}
	if err = model.UpdateMCPTool(serverID, toolID, revision, map[string]any{"price_mode": mode, "unit_price_usd": quote.UnitPriceUSD, "unit_price": quote.UnitPrice, "currency": quote.Currency, "price_revision": tool.PriceRevision + 1}); err != nil {
		return nil, err
	}
	quote.PriceRevision = tool.PriceRevision + 1
	return quote, nil
}

func DiscoverTools(ctx context.Context, serverID string, userID int) (map[string]any, error) {
	server, err := model.GetMCPServer(serverID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(server.Limits.TimeoutSeconds)*time.Second)
	defer cancel()
	// Native tool schemas are local metadata. Discovering them does not grant
	// access to knowledge content; execution validates the caller's OAuth grant.
	tools := BuiltinTools(server.Kind)
	if server.Kind == "remote_mcp" {
		token, err := discoveryToken(ctx, server, userID)
		if err != nil {
			return nil, err
		}
		session, closeSession, err := connectRemote(ctx, server, token)
		if err != nil {
			return nil, err
		}
		defer closeSession()
		tools, err = readUpstreamTools(ctx, session)
		if err != nil {
			return nil, err
		}
	}
	var existing []model.MCPTool
	if err = model.DB.Where("server_id = ?", serverID).Find(&existing).Error; err != nil {
		return nil, err
	}
	byName := map[string]model.MCPTool{}
	for _, tool := range existing {
		byName[tool.Name] = tool
	}
	added, changed, removed := []string{}, []string{}, []string{}
	seen := map[string]bool{}
	for _, tool := range tools {
		if seen[tool.Name] {
			return nil, Failure(422, "DUPLICATE_TOOL", "Upstream returned duplicate tool names")
		}
		seen[tool.Name] = true
		old, exists := byName[tool.Name]
		if !exists {
			added = append(added, tool.Name)
		} else if old.SchemaHash != tool.SchemaHash || old.Removed {
			changed = append(changed, tool.Name)
		}
	}
	for _, tool := range existing {
		if !seen[tool.Name] {
			removed = append(removed, tool.Name)
		}
	}
	encoded, err := common.Marshal(tools)
	if err != nil {
		return nil, err
	}
	if len(encoded) > MaxResultBytes {
		return nil, Failure(422, "INVALID_TOOL_DIRECTORY", "Remote tool directory exceeds supported limits")
	}
	discovery := model.MCPDiscovery{ID: uuid.NewString(), ServerID: serverID, UserID: userID, ServerRevision: server.Revision, ToolsJSON: model.MCPEncryptedResult(encoded), ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}
	if err = model.DB.Create(&discovery).Error; err != nil {
		return nil, err
	}
	return map[string]any{"discovery_id": discovery.ID, "server_revision": server.Revision, "added": added, "changed": changed, "removed": removed, "tools": tools}, nil
}

func SyncTools(serverID, discoveryID string, userID int, revision int64) error {
	return model.DB.Transaction(func(tx *gorm.DB) error {
		// A conditional revision increment serializes the full directory update
		// on every supported database, without dialect-specific SQL locks here.
		updated := tx.Model(&model.MCPServer{}).Where("id = ? AND revision = ? AND archived = ?", serverID, revision, false).Update("revision", gorm.Expr("revision + 1"))
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return model.ErrMCPRevisionConflict
		}
		var discovery model.MCPDiscovery
		if err := tx.Where("id = ? AND server_id = ? AND user_id = ? AND expires_at > ?", discoveryID, serverID, userID, time.Now().UTC()).First(&discovery).Error; err != nil {
			return err
		}
		if discovery.ServerRevision != revision {
			return model.ErrMCPRevisionConflict
		}
		var tools []model.MCPTool
		if err := common.UnmarshalJsonStr(string(discovery.ToolsJSON), &tools); err != nil {
			return err
		}
		var existing []model.MCPTool
		if err := tx.Where("server_id = ?", serverID).Find(&existing).Error; err != nil {
			return err
		}
		byName := map[string]model.MCPTool{}
		for _, tool := range existing {
			byName[tool.Name] = tool
		}
		seen := map[string]bool{}
		for _, tool := range tools {
			seen[tool.Name] = true
			old, exists := byName[tool.Name]
			if !exists {
				tool.ID, tool.ServerID, tool.Revision = uuid.NewString(), serverID, 1
				tool.Enabled = true
				tool.PriceMode, tool.UnitPriceUSD, tool.UnitPrice, tool.Currency, tool.PriceRevision = "free", "0", "0", "USD", 1
				if err := tx.Create(&tool).Error; err != nil {
					return err
				}
				continue
			}
			if old.SchemaHash != tool.SchemaHash || old.Removed {
				if err := tx.Model(&model.MCPTool{}).Where("id = ?", old.ID).Updates(map[string]any{"input_schema": tool.InputSchema, "output_schema": tool.OutputSchema, "schema_hash": tool.SchemaHash, "review_required": true, "removed": false, "enabled": false, "revision": gorm.Expr("revision + 1")}).Error; err != nil {
					return err
				}
			}
		}
		for _, old := range existing {
			if !seen[old.Name] {
				if err := tx.Model(&old).Updates(map[string]any{"removed": true, "enabled": false, "revision": gorm.Expr("revision + 1")}).Error; err != nil {
					return err
				}
			}
		}
		return tx.Delete(&discovery).Error
	})
}

func discoveryToken(ctx context.Context, server *model.MCPServer, userID int) (string, error) {
	if !server.Catalog || server.AuthMode == "none" {
		return "", nil
	}
	row, err := Install(server.ID, userID, true)
	if err != nil {
		return "", Failure(401, "REAUTHORIZATION_REQUIRED", "Authorize discovery for this service")
	}
	return InstallationToken(ctx, server, row)
}

func readUpstreamTools(ctx context.Context, session *mcpsdk.ClientSession) ([]model.MCPTool, error) {
	tools := make([]model.MCPTool, 0)
	names := make(map[string]bool)
	cursor := ""
	seenCursors := map[string]bool{}
	for {
		if seenCursors[cursor] || len(seenCursors) >= 256 {
			return nil, Failure(502, "DISCOVERY_FAILED", "Remote tool pagination did not advance")
		}
		seenCursors[cursor] = true
		page, err := session.ListTools(ctx, &mcpsdk.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, Failure(502, "DISCOVERY_FAILED", "Remote tool discovery failed")
		}
		for _, remote := range page.Tools {
			if remote == nil || len(tools) >= 256 || !toolNamePattern.MatchString(remote.Name) || len(remote.Description) > 4096 {
				return nil, Failure(422, "INVALID_TOOL_DIRECTORY", "Remote tool directory exceeds supported limits")
			}
			if names[remote.Name] {
				return nil, Failure(502, "DUPLICATE_TOOL", "Duplicate upstream tool name")
			}
			names[remote.Name] = true
			inputBytes, err := common.Marshal(remote.InputSchema)
			if err != nil {
				return nil, err
			}
			var input, output model.MCPJSON
			if common.Unmarshal(inputBytes, &input) != nil || input["type"] != "object" {
				return nil, Failure(422, "INVALID_TOOL_SCHEMA", "Remote tool input must be an object schema")
			}
			if err = ValidateToolSchema(input, nil); err != nil {
				return nil, err
			}
			if remote.OutputSchema != nil {
				encoded, err := common.Marshal(remote.OutputSchema)
				if err != nil || common.Unmarshal(encoded, &output) != nil || output["type"] != "object" {
					return nil, Failure(422, "INVALID_TOOL_SCHEMA", "Remote output schema is invalid")
				}
				if err = ValidateToolSchema(output, nil); err != nil {
					return nil, err
				}
			}
			hash, err := SchemaHash(input, output)
			if err != nil {
				return nil, err
			}
			tools = append(tools, model.MCPTool{Name: remote.Name, Description: remote.Description, InputSchema: input, OutputSchema: output, SchemaHash: hash})
		}
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == cursor {
			return nil, Failure(502, "DISCOVERY_FAILED", "Remote tool pagination did not advance")
		}
		cursor = page.NextCursor
	}
	return tools, nil
}
