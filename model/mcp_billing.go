package model

import (
	"errors"
	"fmt"
	"os"
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

func applyMCPCharge(invocationID, kind string) error {
	var initial MCPInvocation
	if err := DB.First(&initial, "id = ?", invocationID).Error; err != nil {
		return err
	}
	if initial.ChargeQuota == 0 {
		return nil
	}
	if initial.ChargeQuota < 0 || initial.ChargeQuota > common.MaxQuota || initial.PayerUserID <= 0 {
		return ErrMCPBillingState
	}
	op := MCPBillingOperation{ID: mcpFundingOperationID(&initial, kind), InvocationID: invocationID, Kind: kind, Amount: initial.ChargeQuota, PayerUserID: initial.PayerUserID, TokenID: initial.TokenID, State: "pending"}
	if err := DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&op).Error; err != nil {
		return err
	}
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&MCPInvocation{}).Where("id = ?", invocationID).UpdateColumn("id", gorm.Expr("id")).Error; err != nil {
			return err
		}
		var call MCPInvocation
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
		if op.State != "pending" || op.Amount != call.ChargeQuota {
			return ErrMCPBillingState
		}
		if kind == "reserve" && call.BillingStatus != "reserving" || kind != "reserve" && call.BillingStatus != "reserved" && call.BillingStatus != "reconciliation_pending" && call.BillingStatus != "refund_pending" {
			return ErrMCPBillingState
		}
		user, err := lockQuotaUser(tx, call.PayerUserID, true)
		if err != nil {
			return err
		}
		if call.TokenID > 0 {
			token, err := lockQuotaToken(tx, call.TokenID, true)
			if err != nil {
				return err
			}
			if token.UserId != call.UserID {
				return ErrMCPBillingState
			}
		}
		if kind == "settle" {
			if _, err := common.WalletQuotaFromDecimalStrict(decimal.NewFromInt(int64(user.UsedQuota)).Add(decimal.NewFromInt(int64(call.ChargeQuota)))); err != nil {
				return err
			}
			if err := tx.Unscoped().Model(&User{}).Where("id = ?", user.Id).Update("used_quota", gorm.Expr("used_quota + ?", call.ChargeQuota)).Error; err != nil {
				return err
			}
			if err := tx.Model(&MCPInvocation{}).Where("id = ?", call.ID).Updates(map[string]any{"billing_status": "settled", "charged_quota": call.ChargeQuota, "reconcile_at": nil}).Error; err != nil {
				return err
			}
			return tx.Model(&op).Update("state", "applied").Error
		}
		delta := call.ChargeQuota
		if kind == "reserve" {
			delta = -delta
		}
		walletID := call.PayerUserID
		if kind == "reserve" && call.FundingPreference != "" {
			err = reserveMCPFunding(tx, &call, delta)
		} else {
			if call.FundingSource == "subscription" {
				if err = refundSubscription(tx, mcpFundingOperationID(&call, "reserve")); err != nil {
					return err
				}
				walletID = 0
			}
			err = mutateQuotaInTx(tx, walletID, call.TokenID, delta, kind == "reserve")
		}
		if err != nil {
			switch {
			case errors.Is(err, ErrQuotaWalletInsufficient), errors.Is(err, ErrNoActiveSubscription), errors.Is(err, ErrSubscriptionQuotaInsufficient):
				return ErrMCPInsufficientQuota
			case errors.Is(err, ErrQuotaTokenInsufficient):
				return ErrMCPTokenQuota
			case errors.Is(err, ErrQuotaCacheUnavailable):
				return ErrMCPBillingPending
			default:
				return err
			}
		}
		state := "reserved"
		if kind == "refund" {
			state = "refunded"
		}
		if err := tx.Model(&MCPInvocation{}).Where("id = ?", call.ID).Updates(map[string]any{"billing_status": state, "charged_quota": 0, "funding_source": call.FundingSource, "subscription_id": call.SubscriptionID}).Error; err != nil {
			return err
		}
		return tx.Model(&op).Update("state", "applied").Error
	})

	return err
}

// AbortMCPReservation cancels an uncommitted reservation. No cache monetary
// compensation is needed: SQL balances and operation state commit atomically.
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
// settled; everything unresolved beyond the deadline is released to the user.
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
	if reserve.State != "applied" {
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

// Wallet and subscription mutations share the invocation transaction with
// the API-token mutation. A failed token check rolls back either funding source.
func reserveMCPFunding(tx *gorm.DB, call *MCPInvocation, delta int) error {
	wallet := func() error {
		call.FundingSource = "wallet"
		return mutateQuotaInTx(tx, call.PayerUserID, call.TokenID, delta, true)
	}
	subscription := func() error {
		record, err := preConsumeSubscription(tx, mcpFundingOperationID(call, "reserve"), call.PayerUserID, int64(call.ChargeQuota), common.GetTimestamp())
		if err != nil {
			return err
		}
		call.FundingSource, call.SubscriptionID = "subscription", record.UserSubscriptionId
		if call.TokenID == 0 {
			return nil
		}
		return mutateQuotaInTx(tx, 0, call.TokenID, delta, true)
	}
	switch call.FundingPreference {
	case "subscription_only":
		return subscription()
	case "wallet_only":
		return wallet()
	case "wallet_first":
		err := wallet()
		if errors.Is(err, ErrQuotaWalletInsufficient) {
			return subscription()
		}
		return err
	default:
		err := subscription()
		if errors.Is(err, ErrNoActiveSubscription) {
			return wallet()
		}
		if !errors.Is(err, ErrSubscriptionQuotaInsufficient) {
			return err
		}
		var strict int64
		if err = tx.Model(&UserSubscription{}).Where("user_id = ? AND status = ? AND end_time > ? AND allow_wallet_overflow = ?", call.PayerUserID, "active", common.GetTimestamp(), false).Count(&strict).Error; err != nil {
			return err
		}
		if strict != 0 {
			return ErrSubscriptionQuotaInsufficient
		}
		return wallet()
	}
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
