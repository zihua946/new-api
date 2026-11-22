package model

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrMCPInsufficientQuota = errors.New("insufficient MCP wallet or token quota")
	ErrMCPTokenQuota        = errors.New("insufficient MCP token quota")
	ErrMCPBillingState      = errors.New("MCP billing state does not allow this operation")
	ErrMCPBillingPending    = errors.New("MCP funding operation requires reconciliation")
)

// Legacy batch deltas are process-local. Another process (including a restarted
// owner) cannot tell whether a reservation flushed before a crash. Never issue
// an automatic refund against an unconfirmed batch: it could create a credit
// for a debit that was lost. This ownership guard is MCP-only.
var mcpBatchState = "batch:" + rand.Text()[:16]

func ReserveMCPCharge(invocationID string) error {
	err := applyMCPCharge(invocationID, "reserve")
	if errors.Is(err, ErrMCPInsufficientQuota) || errors.Is(err, ErrMCPTokenQuota) {
		if abortErr := AbortMCPReservation(invocationID); abortErr != nil {
			return abortErr
		}
	}
	return err
}

func SettleMCPCharge(invocationID string) error { return applyMCPCharge(invocationID, "settle") }
func RefundMCPCharge(invocationID string) error { return applyMCPCharge(invocationID, "refund") }

func mcpFundingOperationID(call *MCPInvocation, kind string) string {
	if call.FundingAttempt > 0 {
		return call.ID + ":personal:" + kind
	}
	return call.ID + ":" + kind
}

func SwitchMCPToPersonalPayer(invocationID string) error {
	if os.Getenv("BILLING_TEAM_PERSONAL_FALLBACK_ENABLED") != "true" {
		return ErrMCPInsufficientQuota
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		var call MCPInvocation
		if err := lockForUpdate(tx).First(&call, "id = ?", invocationID).Error; err != nil {
			return err
		}
		if call.BillingUnitID == 0 || call.FundingAttempt != 0 || call.BillingStatus != "refunded" {
			return ErrMCPBillingState
		}
		var op MCPBillingOperation
		if err := tx.First(&op, "id = ?", mcpFundingOperationID(&call, "reserve")).Error; err != nil {
			return err
		}
		if op.State != "aborted" {
			return ErrMCPBillingState
		}
		var user User
		if err := lockForUpdate(tx).First(&user, call.UserID).Error; err != nil {
			return err
		}
		if user.Status != common.UserStatusEnabled || !user.GetSetting().PersonalBillingFallback {
			return ErrMCPInsufficientQuota
		}
		return tx.Model(&call).Updates(map[string]any{"payer_user_id": user.Id, "billing_unit_id": 0, "funding_attempt": 1, "billing_status": "reserving", "funding_preference": common.NormalizeBillingPreference(user.GetSetting().BillingPreference)}).Error
	})
}

// MCP records ownership of each invocation's funding operation, but delegates
// balances to the existing model billing APIs. An executing operation is never
// replayed: those APIs may have changed Redis, queued a batch delta, or committed
// SQL before losing their acknowledgement. Applied means the legacy API accepted
// the mutation, not that an enabled in-memory batch has flushed. Such operations require reconciliation
// against the original balances, not another non-idempotent debit or refund.
func applyMCPCharge(invocationID, kind string) error {
	var call MCPInvocation
	if err := DB.First(&call, "id = ?", invocationID).Error; err != nil {
		return err
	}
	if call.ChargeQuota == 0 {
		return nil
	}
	if call.ChargeQuota < 0 || call.ChargeQuota > common.MaxQuota || call.PayerUserID <= 0 {
		return ErrMCPBillingState
	}
	op := MCPBillingOperation{ID: mcpFundingOperationID(&call, kind), InvocationID: invocationID, Kind: kind, Amount: call.ChargeQuota, PayerUserID: call.PayerUserID, TokenID: call.TokenID, State: "pending"}
	if err := DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&op).Error; err != nil {
		return err
	}
	claimed := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&MCPInvocation{}).Where("id = ?", invocationID).UpdateColumn("id", gorm.Expr("id")).Error; err != nil {
			return err
		}
		if err := lockForUpdate(tx).First(&call, "id = ?", invocationID).Error; err != nil {
			return err
		}
		if err := lockForUpdate(tx).First(&op, "id = ?", op.ID).Error; err != nil {
			return err
		}
		if op.ID != mcpFundingOperationID(&call, kind) || op.InvocationID != call.ID || op.Kind != kind || op.Amount != call.ChargeQuota || op.PayerUserID != call.PayerUserID || op.TokenID != call.TokenID {
			return ErrMCPBillingState
		}
		if op.State == "applied" {
			return nil
		}
		if strings.HasPrefix(op.State, "batch:") {
			if op.State != mcpBatchState {
				return ErrMCPBillingPending
			}
			return nil
		}
		if op.State == "executing" {
			return ErrMCPBillingPending
		}
		if op.State != "pending" {
			return ErrMCPBillingState
		}
		allowed := false
		switch kind {
		case "reserve":
			allowed = call.BillingStatus == "reserving"
		case "refund":
			allowed = call.BillingStatus == "reserved" || call.BillingStatus == "refund_pending" || call.BillingStatus == "reconciliation_pending"
		case "settle":
			allowed = call.BillingStatus == "reserved" || call.BillingStatus == "reconciliation_pending"
		}
		if !allowed {
			return ErrMCPBillingState
		}
		if kind != "reserve" {
			var reservation MCPBillingOperation
			if err := tx.First(&reservation, "id = ?", mcpFundingOperationID(&call, "reserve")).Error; err != nil {
				return err
			}
			if strings.HasPrefix(reservation.State, "batch:") {
				if reservation.State != mcpBatchState {
					return ErrMCPBillingPending
				}
			} else if reservation.State != "applied" {
				return ErrMCPBillingPending
			}
		}
		if kind == "settle" {
			var user User
			if err := lockForUpdate(tx.Unscoped()).First(&user, call.PayerUserID).Error; err != nil {
				return err
			}
			if _, err := common.WalletQuotaFromDecimalStrict(decimal.NewFromInt(int64(user.UsedQuota)).Add(decimal.NewFromInt(int64(call.ChargeQuota)))); err != nil {
				return err
			}
			if err := tx.Unscoped().Model(&User{}).Where("id = ?", user.Id).Update("used_quota", gorm.Expr("used_quota + ?", call.ChargeQuota)).Error; err != nil {
				return err
			}
			if err := tx.Model(&call).Updates(map[string]any{"billing_status": "settled", "charged_quota": call.ChargeQuota, "reconcile_at": nil}).Error; err != nil {
				return err
			}
			return tx.Model(&op).Update("state", "applied").Error
		}
		if kind == "refund" {
			if err := tx.Model(&call).Update("billing_status", "refund_pending").Error; err != nil {
				return err
			}
		}
		claimed = true
		return tx.Model(&op).Update("state", "executing").Error
	})
	if err != nil || !claimed {
		return err
	}

	state, billing := "applied", "reserved"
	if kind == "reserve" {
		err = reserveMCPFunding(&call)
		if errors.Is(err, ErrMCPInsufficientQuota) || errors.Is(err, ErrMCPTokenQuota) {
			state, billing = "aborted", "refunded"
		}
	} else {
		billing = "refunded"
		err = refundMCPFunding(&call)
	}
	if err != nil && state != "aborted" {
		common.SysError(fmt.Sprintf("MCP funding requires manual reconciliation: invocation=%s operation=%s error=%v", call.ID, op.ID, err))
		return ErrMCPBillingPending
	}
	fundingErr := err
	if kind == "reserve" && state == "applied" && common.BatchUpdateEnabled {
		state = mcpBatchState
	}
	err = DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&MCPBillingOperation{}).Where("id = ? AND state = ?", op.ID, "executing").Update("state", state)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrMCPBillingState
		}
		return tx.Model(&call).Updates(map[string]any{"billing_status": billing, "charged_quota": 0, "funding_source": call.FundingSource, "subscription_id": call.SubscriptionID}).Error
	})
	if err != nil {
		// A lost commit response can be resolved from the ledger. If SQL is
		// unavailable or still says executing, never repeat the balance mutation.
		var stored MCPBillingOperation
		if readErr := DB.First(&stored, "id = ?", op.ID).Error; readErr != nil || stored.State != state {
			common.SysError(fmt.Sprintf("MCP funding outcome not recorded: invocation=%s operation=%s error=%v", call.ID, op.ID, err))
			return ErrMCPBillingPending
		}
	}
	return fundingErr
}

// AbortMCPReservation cancels only work that never entered the legacy balance
// APIs. An executing operation may already have changed balances and is not replayed.
func AbortMCPReservation(invocationID string) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&MCPInvocation{}).Where("id = ?", invocationID).UpdateColumn("id", gorm.Expr("id")).Error; err != nil {
			return err
		}
		var call MCPInvocation
		if err := lockForUpdate(tx).First(&call, "id = ?", invocationID).Error; err != nil {
			return err
		}
		var op MCPBillingOperation
		if err := lockForUpdate(tx).First(&op, "id = ?", mcpFundingOperationID(&call, "reserve")).Error; err != nil {
			return err
		}
		if op.InvocationID != call.ID || op.Kind != "reserve" || op.Amount != call.ChargeQuota || op.PayerUserID != call.PayerUserID || op.TokenID != call.TokenID {
			return ErrMCPBillingState
		}
		if op.State == "aborted" {
			return nil
		}
		if op.State == "executing" {
			return ErrMCPBillingPending
		}
		if op.State != "pending" || call.BillingStatus != "reserving" {
			return ErrMCPBillingState
		}
		if err := tx.Model(&call).Update("billing_status", "refunded").Error; err != nil {
			return err
		}
		return tx.Model(&op).Update("state", "aborted").Error
	})
}

// ReconcileMCPCharge never repeats an upstream request. Persisted outcomes are
// settled and confirmed reservations are refunded after the deadline. Ambiguous
// legacy balance operations remain pending for manual reconciliation.
func ReconcileMCPCharge(invocationID string, now time.Time) error {
	var call MCPInvocation
	if err := DB.First(&call, "id = ?", invocationID).Error; err != nil {
		return err
	}
	if call.BillingStatus == "free" || call.BillingStatus == "refunded" || call.BillingStatus == "settled" {
		return nil
	}
	if call.Status == "succeeded" && call.Billable {
		return SettleMCPCharge(call.ID)
	}
	if call.Status != "failed" && call.Status != "succeeded" && (call.ReconcileAt == nil || now.Before(*call.ReconcileAt)) {
		return nil
	}
	var reserve MCPBillingOperation
	err := DB.First(&reserve, "id = ?", mcpFundingOperationID(&call, "reserve")).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return DB.Model(&MCPInvocation{}).Where("id = ? AND billing_status = ?", invocationID, "reserving").Update("billing_status", "refunded").Error
	}
	if err != nil {
		return err
	}
	if reserve.State != "applied" && !strings.HasPrefix(reserve.State, "batch:") {
		return AbortMCPReservation(invocationID)
	}
	if err = RefundMCPCharge(invocationID); err != nil {
		return err
	}
	return nil
}

func ClaimMCPInvocation(id string, limit int, timeout time.Duration) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		var call MCPInvocation
		if err := tx.First(&call, "id = ?", id).Error; err != nil {
			return err
		}
		var server MCPServer
		if err := lockForUpdate(tx).First(&server, "id = ?", call.ServerID).Error; err != nil {
			return err
		}
		now := time.Now().UTC()
		var count int64
		if err := tx.Model(&MCPInvocation{}).Where("server_id = ? AND lease_until > ?", call.ServerID, now).Count(&count).Error; err != nil {
			return err
		}
		if count >= int64(limit) {
			return fmt.Errorf("MCP concurrency limit reached")
		}
		until := now.Add(timeout + time.Minute)
		result := tx.Model(&MCPInvocation{}).Where("id = ? AND status = ? AND lease_until IS NULL", id, "accepted").Updates(map[string]any{"status": "running", "lease_until": until})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrMCPBillingState
		}
		return nil
	})
}

// reserveMCPFunding uses the same wallet/token reservation and subscription APIs
// as model calls. It does not invalidate caches, flush other requests' batches,
// or replace the model funding implementation.
func reserveMCPFunding(call *MCPInvocation) error {
	user, err := GetUserById(call.PayerUserID, false)
	if err != nil || user.Status != common.UserStatusEnabled {
		return ErrMCPBillingState
	}
	var token *Token
	if call.TokenID > 0 {
		token, err = GetTokenById(call.TokenID)
		if err != nil || token.UserId != call.UserID || token.Status != common.TokenStatusEnabled || token.ExpiredTime != -1 && token.ExpiredTime <= common.GetTimestamp() {
			return ErrMCPBillingState
		}
	}
	wallet := func() error {
		// Record the intended source before invoking non-idempotent balance APIs.
		call.FundingSource = "wallet"
		if err := DB.Model(call).Update("funding_source", call.FundingSource).Error; err != nil {
			return err
		}
		reserved, err := TryReserveUserQuota(call.PayerUserID, call.ChargeQuota)
		if err != nil {
			return err
		}
		if !reserved {
			return ErrMCPInsufficientQuota
		}
		return nil
	}
	subscription := func() error {
		call.FundingSource = "subscription"
		if err := DB.Model(call).Update("funding_source", call.FundingSource).Error; err != nil {
			return err
		}
		record, err := PreConsumeUserSubscription(mcpFundingOperationID(call, "reserve"), call.PayerUserID, "", 0, int64(call.ChargeQuota))
		if err != nil {
			return err
		}
		call.SubscriptionID = record.UserSubscriptionId
		return nil
	}
	switch call.FundingPreference {
	case "", "wallet_only":
		err = wallet()
	case "subscription_only":
		err = subscription()
	case "wallet_first":
		err = wallet()
		if errors.Is(err, ErrMCPInsufficientQuota) {
			err = subscription()
		}
	default:
		err = subscription()
		if errors.Is(err, ErrNoActiveSubscription) {
			err = wallet()
		} else if errors.Is(err, ErrSubscriptionQuotaInsufficient) {
			var strict int64
			if queryErr := DB.Model(&UserSubscription{}).Where("user_id = ? AND status = ? AND end_time > ? AND allow_wallet_overflow = ?", call.PayerUserID, "active", common.GetTimestamp(), false).Count(&strict).Error; queryErr != nil {
				return queryErr
			}
			if strict == 0 {
				err = wallet()
			}
		}
	}
	if errors.Is(err, ErrNoActiveSubscription) || errors.Is(err, ErrSubscriptionQuotaInsufficient) {
		return ErrMCPInsufficientQuota
	}
	if err != nil || token == nil {
		return err
	}
	reserved, err := TryReserveTokenQuota(token.Id, token.Key, call.ChargeQuota, token.UnlimitedQuota)
	if err != nil {
		// A failed reply is not proof that the token debit did not happen.
		return err
	}
	if reserved {
		return nil
	}
	// A definite token balance rejection has no token delta to compensate.
	if call.FundingSource == "subscription" {
		err = refundMCPSubscription(mcpFundingOperationID(call, "reserve"))
	} else {
		err = IncreaseUserQuota(call.PayerUserID, call.ChargeQuota, false)
	}
	if err != nil {
		return err
	}
	return ErrMCPTokenQuota
}

func refundMCPFunding(call *MCPInvocation) error {
	var err error
	if call.FundingSource == "subscription" {
		err = refundMCPSubscription(mcpFundingOperationID(call, "reserve"))
	} else {
		err = IncreaseUserQuota(call.PayerUserID, call.ChargeQuota, false)
	}
	if err != nil || call.TokenID == 0 {
		return err
	}
	var token Token
	if err := DB.Unscoped().First(&token, call.TokenID).Error; err != nil {
		return err
	}
	return IncreaseTokenQuota(token.Id, token.Key, call.ChargeQuota)
}

// Keep the MCP subscription refund and its idempotency receipt in one
// transaction. The legacy public subscription API is deliberately unchanged;
// its separate nested transaction cannot be used under SQLite WAL snapshots.
func refundMCPSubscription(requestID string) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&SubscriptionPreConsumeRecord{}).Where("request_id = ?", requestID).UpdateColumn("request_id", gorm.Expr("request_id")).Error; err != nil {
			return err
		}
		var record SubscriptionPreConsumeRecord
		if err := lockForUpdate(tx).Where("request_id = ?", requestID).First(&record).Error; err != nil {
			return err
		}
		if record.Status == "refunded" {
			return nil
		}
		if record.PreConsumed > 0 {
			var subscription UserSubscription
			if err := lockForUpdate(tx).First(&subscription, record.UserSubscriptionId).Error; err != nil {
				return err
			}
			if err := tx.Model(&subscription).Update("amount_used", max(subscription.AmountUsed-record.PreConsumed, 0)).Error; err != nil {
				return err
			}
		}
		return tx.Model(&record).Update("status", "refunded").Error
	})
}

// The invocation serializes receipt delivery, including recovery after the
// independent log database committed but the primary transaction did not.
func RecordMCPReceipt(id string) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&MCPInvocation{}).Where("id = ? AND receipt_state = ?", id, "pending").UpdateColumn("id", gorm.Expr("id")).Error; err != nil {
			return err
		}
		var call MCPInvocation
		if err := lockForUpdate(tx).First(&call, "id = ?", id).Error; err != nil {
			return err
		}
		if call.ReceiptState != "pending" || call.Status != "succeeded" || call.BillingStatus != "free" && call.BillingStatus != "settled" {
			return nil
		}
		ids := []int{call.UserID}
		if call.UserID != call.PayerUserID {
			ids = append(ids, call.PayerUserID)
		}
		if err := tx.Unscoped().Model(&User{}).Where("id IN ?", ids).UpdateColumn("request_count", gorm.Expr("request_count + 1")).Error; err != nil {
			return err
		}
		if common.LogConsumeEnabled {
			logs := LOG_DB
			if logs == nil {
				return errors.New("MCP log database unavailable")
			}
			if logs == DB {
				logs = tx
			}
			var count int64
			if err := logs.Model(&Log{}).Where("request_id = ? AND type = ?", id, LogTypeConsume).Count(&count).Error; err != nil {
				return err
			}
			if count == 0 {
				var who User
				if err := tx.Unscoped().Select("username", commonGroupCol).First(&who, call.UserID).Error; err != nil {
					return err
				}
				other, err := common.Marshal(map[string]any{"mcp_server_id": call.ServerID, "mcp_tool_id": call.ToolID, "funding_source": call.FundingSource, "subscription_id": call.SubscriptionID, "currency": call.Currency, "unit_price": call.UnitPrice, "exchange_rate": call.ExchangeRate, "quota_per_unit": call.QuotaPerUnit})
				if err != nil {
					return err
				}
				log := Log{Type: LogTypeConsume, RequestId: id, CreatedAt: call.CreatedAt.Unix(), ModelName: "mcp/" + call.ToolName, Content: "MCP tool call", Username: who.Username, Group: who.Group, UserId: call.UserID, BillingUserId: call.PayerUserID, BillingUnitId: call.BillingUnitID, TokenId: call.TokenID, ChannelId: call.ChannelID, Quota: call.ChargedQuota, Other: string(other)}
				if err = logs.Create(&log).Error; err != nil {
					return err
				}
			}
		}
		return tx.Model(&call).Update("receipt_state", "recorded").Error
	})
}
