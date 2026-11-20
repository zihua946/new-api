package mcp

import (
	"context"
	"errors"
	"fmt"
	"github.com/QuantumNous/new-api/constant"
	"math/rand/v2"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/google/uuid"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"gorm.io/gorm"
)

type InvocationOptions struct {
	RequestID            string
	IdempotencyKey       string
	Test                 bool
	ConfirmPrice         bool
	ExpectedToolRevision int64
	MaxChargeQuota       *int
}

type PreparedCall struct {
	Invocation *model.MCPInvocation
	Created    bool
	server     *model.MCPServer
	tool       *model.MCPTool
	actor      *Actor
	args       map[string]any
	options    InvocationOptions
}

var idempotencyPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

func PrepareCall(serverID, toolName string, actor *Actor, arguments map[string]any, options InvocationOptions) (*PreparedCall, error) {
	server, err := model.GetMCPServer(serverID)
	if err != nil {
		return nil, err
	}
	if server.Kind == "knowledge" {
		return nil, Failure(410, "SERVICE_RETIRED", "Service type retired")
	}
	var tool model.MCPTool
	if err = model.DB.Where("server_id = ? AND name = ?", serverID, toolName).First(&tool).Error; err != nil || tool.Name != toolName {
		return nil, Failure(404, "TOOL_NOT_FOUND", "Tool is unavailable")
	}
	if !CanUseTool(server, &tool, actor, options.Test) || !actor.CanCall {
		return nil, Failure(403, "TOOL_ACCESS_DENIED", "This tool is not available to the current identity")
	}
	if _, err = encryptionKey(); err != nil {
		return nil, err
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	if server.Kind != "remote_mcp" && server.Kind != "public_knowledge" {
		arguments, err = ValidateBuiltinArguments(toolName, arguments)
	} else {
		err = ValidateToolSchema(tool.InputSchema, arguments)
	}
	if err != nil {
		return nil, err
	}
	encoded, err := common.Marshal(arguments)
	if err != nil || len(encoded) > MaxArgumentBytes {
		return nil, Failure(422, "INVALID_ARGUMENTS", "Tool arguments are too large")
	}
	key := options.IdempotencyKey
	if toolName == "create_consultation" && (server.Kind == "knowledge" || server.Kind == "public_knowledge") {
		key = arguments["request_id"].(string)
	}
	if (options.Test || options.ConfirmPrice) && options.IdempotencyKey == "" {
		return nil, Failure(422, "IDEMPOTENCY_KEY_REQUIRED", "Test calls require an Idempotency-Key")
	}

	if key == "" {
		key = uuid.NewString()
	}
	if !idempotencyPattern.MatchString(key) {
		return nil, Failure(422, "INVALID_IDEMPOTENCY_KEY", "Idempotency key is invalid")
	}
	prefix := fmt.Sprintf("%d\x00%s\x00%s\x00", actor.UserID, serverID, tool.ID)
	hash := Digest(prefix + key)
	lookup := []string{hash, Digest(prefix + "true\x00" + key), Digest(prefix + "false\x00" + key)}
	argumentsHash := Digest(string(encoded))
	prepared := &PreparedCall{server: server, tool: &tool, actor: actor, args: arguments, options: options}
	var prior model.MCPInvocation
	err = model.DB.Where("idempotency_hash IN ?", lookup).Order("created_at").First(&prior).Error
	if err == nil {
		if prior.ArgumentsHash != argumentsHash {
			return nil, model.ErrMCPIdempotencyConflict
		}
		prepared.Invocation = &prior
		// Engine creation is explicitly idempotent. A retry may recover a lost
		// task response; generic remote tools are never replayed this way.
		if (server.Kind == "knowledge" || server.Kind == "public_knowledge") && tool.Name == "create_consultation" && prior.Status == "unknown" && (prior.BillingStatus == "reserved" || prior.BillingStatus == "free") && prior.ReconcileAt != nil && time.Now().Before(*prior.ReconcileAt) {
			updated := model.DB.Model(&model.MCPInvocation{}).Where("id = ? AND status = ?", prior.ID, "unknown").Updates(map[string]any{"status": "accepted", "lease_until": nil})
			if updated.Error != nil {
				return nil, updated.Error
			}
			prepared.Created = updated.RowsAffected == 1
		}
		return prepared, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	if (options.Test || options.ConfirmPrice) && options.ExpectedToolRevision != tool.Revision {
		return nil, model.ErrMCPRevisionConflict
	}
	quote, err := toolQuote(&tool)
	if err != nil {
		var failure *Error
		if errors.As(err, &failure) && failure.quotaClamp != nil {
			requestID := options.RequestID
			if requestID == "" {
				requestID = common.NewRequestId()
			}
			clamp := failure.quotaClamp
			ctx := context.WithValue(context.Background(), common.RequestIdKey, requestID)
			logger.LogWarn(ctx, "MCP quota saturation: op=%s kind=%s user=%d service=%s tool=%s", clamp.Op, clamp.Kind, actor.UserID, server.ID, tool.ID)
			other := model.NewLogOther()
			other.SetAdmin("quota_saturation", clamp.AuditMap())
			other.SetPublic("mcp_server_id", server.ID)
			other.SetPublic("mcp_tool_id", tool.ID)
			entry := model.Log{Type: model.LogTypeError, UserId: actor.UserID, TokenId: actor.TokenID, Group: actor.Group, ModelName: "mcp/" + tool.Name,
				RequestId: requestID, CreatedAt: common.GetTimestamp(), Content: "MCP tool price exceeds quota limit", Other: other.JSONString()}
			if model.LOG_DB != nil {
				if logErr := model.LOG_DB.Create(&entry).Error; logErr != nil {
					return nil, logErr
				}
			}
		}
		return nil, err
	}
	if (options.Test || options.ConfirmPrice) && (options.MaxChargeQuota == nil || *options.MaxChargeQuota < quote.ChargeQuota) {
		return nil, Failure(409, "PRICE_CONFIRMATION_REQUIRED", "Confirm the current maximum charge before testing")
	}
	unit, err := model.ResolveBillingUnit(actor.UserID)
	if err != nil {
		return nil, Failure(403, "BILLING_IDENTITY_UNAVAILABLE", "Billing identity is unavailable")
	}
	payer, unitID := actor.UserID, 0
	if unit != nil {
		payer, unitID = unit.PayerUserId, unit.Id
	}
	payingUser, err := model.GetUserById(payer, false)
	if err != nil || payingUser.Status != common.UserStatusEnabled {
		return nil, Failure(403, "BILLING_IDENTITY_UNAVAILABLE", "Billing account is unavailable")
	}
	status := "reserving"
	if quote.ChargeQuota == 0 {
		status = "free"
	}
	deadline := time.Now().UTC().Add(time.Duration(server.Limits.TimeoutSeconds)*time.Second + 10*time.Minute)
	channelID := server.Connection.ChannelID
	if server.Catalog && server.Kind == "deepseek" {
		channels, err := model.GetChannelsByType(0, -1, false, constant.ChannelTypeDeepSeek)
		if err != nil {
			return nil, err
		}
		if len(channels) == 0 {
			return nil, Failure(503, "DEEPSEEK_CHANNEL_NOT_CONFIGURED", "DeepSeek channel is not configured")
		}
		group, err := model.GetUserGroup(actor.UserID, true)
		if err != nil {
			return nil, err
		}
		channels = slices.DeleteFunc(channels, func(channel *model.Channel) bool {
			return channel.Status != common.ChannelStatusEnabled || !slices.Contains(channel.GetGroups(), group)
		})
		if len(channels) == 0 {
			return nil, Failure(503, "CHANNEL_UNAVAILABLE", "No enabled DeepSeek channel is available for the current user group")
		}
		channel := channels[rand.IntN(len(channels))]
		configured := *server
		configured.Connection.ChannelID = channel.Id
		configured.Connection.ConfirmedBaseURL = strings.TrimSuffix(strings.TrimRight(channel.GetBaseURL(), "/"), "/v1")
		server = &configured
		prepared.server = server
		channelID = channel.Id
	}
	call := &model.MCPInvocation{ID: uuid.NewString(), ServerID: serverID, ToolID: tool.ID, ToolName: tool.Name, UserID: actor.UserID, PayerUserID: payer, BillingUnitID: unitID, TokenID: actor.TokenID,
		ChannelID: channelID, IsTest: options.Test, IdempotencyHash: hash, ArgumentsHash: argumentsHash, Status: "accepted", BillingStatus: status,
		FundingPreference: common.NormalizeBillingPreference(payingUser.GetSetting().BillingPreference), UnitPrice: quote.UnitPrice, Currency: quote.Currency, ExchangeRate: quote.ExchangeRate, UnitPriceUSD: quote.UnitPriceUSD, QuotaPerUnit: strconv.FormatFloat(common.QuotaPerUnit, 'f', -1, 64), PriceRevision: tool.PriceRevision, ChargeQuota: quote.ChargeQuota, ReconcileAt: &deadline}
	if server.Kind == "knowledge" || server.Kind == "public_knowledge" {
		sealed, sealErr := Seal(string(encoded), "arguments:"+call.ID)
		if sealErr != nil {
			return nil, sealErr
		}
		call.ArgumentsCiphertext = model.MCPEncryptedResult(sealed)
	}
	prepared.Invocation, prepared.Created, err = model.BeginMCPInvocation(call)
	return prepared, err
}

func (call *PreparedCall) Run(ctx context.Context, progress Progress) (*mcpsdk.CallToolResult, error) {
	if !call.Created {
		return AuthorizedInvocationResult(ctx, call.Invocation, call.actor, call.options.Test)
	}
	timeout := time.Duration(call.server.Limits.TimeoutSeconds) * time.Second

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if !call.actor.ExpiresAt.IsZero() {
		var expireCancel context.CancelFunc
		ctx, expireCancel = context.WithDeadline(ctx, call.actor.ExpiresAt)
		defer expireCancel()
	}
	if err := model.ClaimMCPInvocation(call.Invocation.ID, call.server.Limits.MaxConcurrency, timeout); err != nil {
		return call.fail(Failure(429, "CONCURRENCY_LIMIT", "MCP server is busy"), false)
	}
	// Do not carry stale enable/ACL decisions from a queued admin test into an
	// upstream call. Price remains the frozen accepted quote.
	current, err := model.GetMCPServer(call.server.ID)
	if err != nil {
		return call.fail(err, false)
	}
	var currentTool model.MCPTool
	if err = model.DB.First(&currentTool, "id = ?", call.tool.ID).Error; err != nil {
		return call.fail(err, false)
	}
	if !CanUseTool(current, &currentTool, call.actor, call.options.Test) || currentTool.SchemaHash != call.tool.SchemaHash || current.ConnectionRevision != call.server.ConnectionRevision {
		return call.fail(Failure(409, "CONFIGURATION_CHANGED", "MCP configuration changed before execution"), false)
	}
	if call.actor.InstallationID != "" {
		var row model.MCPInstallation
		if err := model.DB.Where("id = ? AND user_id = ? AND server_id = ?", call.actor.InstallationID, call.actor.UserID, current.ID).First(&row).Error; err != nil {
			return call.fail(Failure(403, "INSTALLATION_UNAVAILABLE", "MCP installation is unavailable"), false)
		}
		if !row.Admin && (!row.Authorized || row.ConnectionRevision != current.ConnectionRevision) {
			return call.fail(Failure(403, "INSTALLATION_DISCONNECTED", "Enable this MCP installation before use"), false)
		}
		if current.Kind == "remote_mcp" {
			token, err := InstallationToken(ctx, current, &row)
			if err != nil {
				return call.fail(err, false)
			}
			identity := *call.actor
			identity.AccessToken = token
			call.actor = &identity
		}
	}
	if call.Invocation.ChargeQuota > 0 {
		err = model.ReserveMCPCharge(call.Invocation.ID)
		if errors.Is(err, model.ErrMCPInsufficientQuota) && call.Invocation.BillingUnitID != 0 {
			if switchErr := model.SwitchMCPToPersonalPayer(call.Invocation.ID); switchErr == nil {
				err = model.ReserveMCPCharge(call.Invocation.ID)
			}
		}
		if err != nil {
			if errors.Is(err, model.ErrMCPInsufficientQuota) || errors.Is(err, model.ErrMCPTokenQuota) {
				return call.fail(Failure(402, "INSUFFICIENT_CREDITS", "Wallet or API token quota is insufficient"), false)
			}
			return call.fail(Failure(503, "BILLING_RECONCILIATION_PENDING", "Funding could not be confirmed; the upstream was not called"), true)
		}
	}
	// Requests lasting over 30 seconds recheck OAuth grant state while active.
	// A failed check cancels only the transport, never the already-created task.
	if call.actor.AccessToken != "" && (!call.server.Catalog || call.server.Kind != "remote_mcp") {
		go call.monitorAuthorization(ctx, cancel)
	}
	outcome, err := ExecuteAdapter(ctx, call.server, call.tool, call.actor, call.args, call.options.IdempotencyKey, progress)
	if err != nil {
		return call.fail(err, outcome.Unknown)
	}
	if outcome.Result == nil {
		return call.fail(Failure(502, "INVALID_UPSTREAM_RESULT", "Upstream returned no tool result"), false)
	}
	if !outcome.Result.IsError && len(call.tool.OutputSchema) > 0 {
		encoded, err := common.Marshal(outcome.Result.StructuredContent)
		var structured map[string]any
		if err != nil || common.Unmarshal(encoded, &structured) != nil || structured == nil || ValidateToolSchema(call.tool.OutputSchema, structured) != nil {
			return call.fail(Failure(502, "INVALID_UPSTREAM_RESULT", "Tool result does not match the reviewed output schema"), false)
		}
	}
	encoded, err := common.Marshal(outcome.Result)
	if err != nil || len(encoded) > MaxResultBytes {
		return call.fail(Failure(502, "INVALID_UPSTREAM_RESULT", "Tool result cannot be stored safely"), false)
	}
	sealed, err := Seal(string(encoded), "result:"+call.Invocation.ID)
	if err != nil {
		return call.fail(err, true)
	}
	expires := time.Now().UTC().Add(24 * time.Hour)
	state := "succeeded"
	if outcome.Result.IsError {
		state = "failed"
	}
	receipt := ""
	if !outcome.Result.IsError {
		receipt = "pending"
	}
	// Persist the usable result before declaring success or finalizing a charge.
	if err = model.DB.Model(&model.MCPInvocation{}).Where("id = ?", call.Invocation.ID).Updates(map[string]any{"status": state, "receipt_state": receipt, "billable": !outcome.Result.IsError, "result_ciphertext": sealed, "result_expires_at": expires, "lease_until": nil}).Error; err != nil {
		return nil, Failure(503, "RESULT_PERSISTENCE_PENDING", "The result could not be durably recorded")
	}
	if call.Invocation.ChargeQuota > 0 {
		if !outcome.Result.IsError {
			err = model.SettleMCPCharge(call.Invocation.ID)
		} else {
			err = model.RefundMCPCharge(call.Invocation.ID)
		}
		if err != nil && !errors.Is(err, model.ErrMCPBillingState) {
			return nil, Failure(503, "BILLING_RECONCILIATION_PENDING", "The result is recorded and payment is being reconciled")
		}
	}
	if err = model.DB.First(call.Invocation, "id = ?", call.Invocation.ID).Error; err != nil {
		return nil, err
	}
	if receipt == "pending" {
		if err = model.RecordMCPReceipt(call.Invocation.ID); err != nil {
			return nil, Failure(503, "BILLING_RECONCILIATION_PENDING", "Payment is recorded and its receipt is being reconciled")
		}
	}
	attachBilling(outcome.Result, call.Invocation)
	return outcome.Result, nil
}

func (call *PreparedCall) monitorAuthorization(ctx context.Context, cancel context.CancelFunc) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := AuthenticateOAuth(ctx, call.server, call.actor.AccessToken); err != nil {
				cancel()
				return
			}
		}
	}
}

func (call *PreparedCall) fail(err error, unknown bool) (*mcpsdk.CallToolResult, error) {
	failure := ErrorResponse(err)
	state := "failed"
	updates := map[string]any{"error_code": failure.Code, "lease_until": nil}
	if unknown {
		state = "unknown"
		updates["reconcile_at"] = time.Now().UTC().Add(10 * time.Minute)
	}
	updates["status"] = state
	if dbErr := model.DB.Model(&model.MCPInvocation{}).Where("id = ?", call.Invocation.ID).Updates(updates).Error; dbErr != nil {
		return nil, Failure(503, "MCP_UNAVAILABLE", "Invocation state could not be saved")
	}
	if !unknown && call.Invocation.ChargeQuota > 0 {
		if reconcileErr := model.ReconcileMCPCharge(call.Invocation.ID, time.Now().UTC()); reconcileErr != nil {
			return nil, Failure(503, "BILLING_RECONCILIATION_PENDING", "Payment is being reconciled")
		}
	}
	return nil, failure
}

func InvocationResult(call *model.MCPInvocation, actorID int) (*mcpsdk.CallToolResult, error) {
	if call.UserID != actorID {
		return nil, Failure(404, "RESOURCE_NOT_FOUND", "Invocation is unavailable")
	}
	if call.Status == "accepted" || call.Status == "running" {
		return nil, Failure(409, "INVOCATION_IN_PROGRESS", "This invocation is still running")
	}
	if call.Status == "unknown" {
		return nil, Failure(503, "UPSTREAM_RESULT_UNKNOWN", "The upstream result could not be confirmed")
	}
	if call.ResultExpiresAt != nil && time.Now().After(*call.ResultExpiresAt) {
		return nil, Failure(410, "RESULT_EXPIRED", "The original result expired; this invocation will not be executed again")
	}
	if call.ResultCiphertext == "" {
		code := call.ErrorCode
		if code == "" {
			code = "TOOL_EXECUTION_FAILED"
		}
		return nil, Failure(422, code, "The original invocation did not return a result")
	}
	if call.ResultExpiresAt == nil || time.Now().After(*call.ResultExpiresAt) {
		return nil, Failure(410, "RESULT_EXPIRED", "The original result expired; this invocation will not be executed again")
	}
	plain, err := Open(string(call.ResultCiphertext), "result:"+call.ID)
	if err != nil {
		return nil, err
	}
	var result mcpsdk.CallToolResult
	if err = common.UnmarshalJsonStr(plain, &result); err != nil {
		return nil, Failure(503, "RESULT_UNAVAILABLE", "Stored result is unavailable")
	}
	attachBilling(&result, call)
	return &result, nil
}

// Cached results retain the same authorization boundary as a fresh call.
// Knowledge ACL is always checked by the engine before any answer is replayed.
func AuthorizedInvocationResult(ctx context.Context, call *model.MCPInvocation, actor *Actor, test bool) (*mcpsdk.CallToolResult, error) {
	server, err := model.GetMCPServer(call.ServerID)
	if err != nil {
		return nil, err
	}
	var tool model.MCPTool
	if err = model.DB.First(&tool, "id = ?", call.ToolID).Error; err != nil {
		return nil, err
	}
	if !CanUseTool(server, &tool, actor, test) || !actor.CanCall {
		return nil, Failure(403, "TOOL_ACCESS_DENIED", "Tool access is no longer available")
	}
	result, err := InvocationResult(call, actor.UserID)
	if err == nil && server.Kind == "public_knowledge" {
		if _, err = publicLibraryClient(ctx, server, actor); err != nil {
			return nil, err
		}
		values, ok := result.StructuredContent.(map[string]any)
		if !ok {
			return nil, Failure(502, "INVALID_UPSTREAM_RESULT", "Invalid stored consultation")
		}
		taskID, _ := values["task_id"].(string)
		if _, err = knowledgeBinding(server, actor, taskID); err != nil {
			return nil, err
		}
		return result, nil
	}
	if err != nil || server.Kind != "knowledge" {
		return result, err
	}
	plain, err := Open(string(call.ArgumentsCiphertext), "arguments:"+call.ID)
	if err != nil {
		return nil, err
	}
	var args map[string]any
	if err = common.UnmarshalJsonStr(plain, &args); err != nil {
		return nil, err
	}
	if tool.Name == "list_knowledge_bases" {
		// Discovery reflects today's accessible resources, including revoked shares.
		outcome, err := executeKnowledge(ctx, server, &tool, actor, args, nil)
		if err != nil {
			return nil, err
		}
		attachBilling(outcome.Result, call)
		return outcome.Result, nil
	}
	taskID, _ := args["task_id"].(string)
	if tool.Name == "create_consultation" {
		data, _ := result.StructuredContent.(map[string]any)
		taskID, _ = data["task_id"].(string)
	}
	var binding model.MCPTaskBinding
	if taskID == "" || model.DB.Where("server_id = ? AND user_id = ? AND task_id = ?", server.ID, actor.UserID, taskID).First(&binding).Error != nil {
		return nil, Failure(404, "RESOURCE_NOT_FOUND", "Consultation is unavailable")
	}
	server, err = providerConfig(server)
	if err != nil {
		return nil, err
	}
	client, err := NewHTTPClient(server.Connection.EngineBaseURL, 15*time.Second)
	if err != nil {
		return nil, err
	}
	defer client.CloseIdleConnections()
	token, err := engineToken(ctx, server, actor, client)
	if err != nil {
		return nil, err
	}
	engine := knowledgeClient{client: client, server: server, token: token}
	if _, err := engine.task(ctx, &binding); err != nil {
		return nil, err
	}
	return result, nil
}

func attachBilling(result *mcpsdk.CallToolResult, call *model.MCPInvocation) {
	result.Meta = mcpsdk.Meta{"knowyet.com/billing": map[string]any{"invocation_id": call.ID, "status": call.BillingStatus, "charged_quota": call.ChargedQuota, "unit_price": call.UnitPrice, "currency": call.Currency, "exchange_rate": call.ExchangeRate, "funding_source": call.FundingSource, "unit_price_usd": call.UnitPriceUSD, "quota_per_unit": call.QuotaPerUnit, "price_revision": call.PriceRevision}}
}

func Recover(ctx context.Context) (int, error) {
	now := time.Now().UTC()
	// Accepted work lost with a process is never blindly re-executed.
	if err := model.DB.WithContext(ctx).Model(&model.MCPInvocation{}).Where("status IN ? AND ((lease_until IS NOT NULL AND lease_until < ?) OR (lease_until IS NULL AND created_at < ?))", []string{"accepted", "running"}, now, now.Add(-3*time.Minute)).Updates(map[string]any{"status": "unknown", "error_code": "EXECUTION_INTERRUPTED", "lease_until": nil}).Error; err != nil {
		return 0, err
	}
	var calls []model.MCPInvocation
	if err := model.DB.WithContext(ctx).Where("billing_status IN ? AND (status IN ? OR reconcile_at <= ?)", []string{"reserving", "reserved", "refund_pending", "reconciliation_pending"}, []string{"succeeded", "failed"}, now).Order("created_at").Limit(100).Find(&calls).Error; err != nil {
		return 0, err
	}
	processed := 0
	var recoveryErrors error
	for _, call := range calls {
		if ctx.Err() != nil {
			return processed, ctx.Err()
		}
		if err := model.ReconcileMCPCharge(call.ID, now); err != nil {
			recoveryErrors = errors.Join(recoveryErrors, err)
			continue
		}
		processed++
	}
	var receipts []string
	if err := model.DB.WithContext(ctx).Model(&model.MCPInvocation{}).Where("receipt_state = ? AND billing_status IN ?", "pending", []string{"free", "settled"}).Order("created_at").Limit(100).Pluck("id", &receipts).Error; err != nil {
		return processed, err
	}
	for _, id := range receipts {
		if err := model.RecordMCPReceipt(id); err != nil {
			recoveryErrors = errors.Join(recoveryErrors, err)
		}
	}
	if err := model.DB.WithContext(ctx).Model(&model.MCPInvocation{}).Where("(result_expires_at <= ?) OR (result_expires_at IS NULL AND created_at <= ?)", now, now.Add(-24*time.Hour)).Updates(map[string]any{"result_ciphertext": "", "arguments_ciphertext": ""}).Error; err != nil {
		return processed, err
	}
	if err := model.DB.WithContext(ctx).Where("expires_at <= ?", now).Delete(&model.MCPDiscovery{}).Error; err != nil {
		return processed, err
	}
	if err := model.DB.WithContext(ctx).Where("expires_at <= ?", now).Delete(&model.MCPTestAuthorization{}).Error; err != nil {
		return processed, err
	}
	if err := model.DB.WithContext(ctx).Where("expires_at <= ?", now).Delete(&model.MCPAuthorizationFlow{}).Error; err != nil {
		return processed, err
	}
	if err := model.DB.WithContext(ctx).Model(&model.MCPChatTest{}).Where("expires_at <= ? AND result <> ?", now, "").Update("result", "").Error; err != nil {
		return processed, err
	}
	return processed, recoveryErrors
}
