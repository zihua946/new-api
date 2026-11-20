package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrQuotaWalletInsufficient = errors.New("wallet quota insufficient")
	ErrQuotaTokenInsufficient  = errors.New("token quota insufficient")
	ErrQuotaCacheUnavailable   = errors.New("quota cache could not be synchronized")
	ErrQuotaOperationConflict  = errors.New("quota operation is no longer pending")
)

// QuotaOperation records model wallet/token mutations independently of MCP
// invocations and consume statistics. State and SQL balances commit together.
// A pending reservation is never replayed by a worker; a pending adjustment is
// a durable obligation (e.g. a refund) which the worker can retry by this ID.
type QuotaOperation struct {
	ID        string `gorm:"primaryKey;size:36"`
	UserID    int    `gorm:"index"`
	TokenID   int    `gorm:"index"`
	Delta     int
	Kind      string    `gorm:"size:16"`
	State     string    `gorm:"size:16;index"`
	CreatedAt time.Time `gorm:"index"`
	UpdatedAt time.Time
}

// QuotaOperationPending identifies an ambiguous SQL result without inviting a
// second, unrelated debit/refund. The durable operation is the source of truth.
type QuotaOperationPending struct {
	ID    string
	Cause error
}

func (e *QuotaOperationPending) Error() string {
	return fmt.Sprintf("quota operation %s requires reconciliation: %v", e.ID, e.Cause)
}
func (e *QuotaOperationPending) Unwrap() error { return e.Cause }

// SQLite has no SELECT FOR UPDATE. A write before the read also fences its
// WAL readers that are about to publish a balance snapshot into Redis.
func lockQuotaUser(tx *gorm.DB, id int, unscoped bool) (*User, error) {
	if unscoped {
		tx = tx.Unscoped()
	}
	if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
		if err := tx.Model(&User{}).Where("id = ?", id).UpdateColumn("id", gorm.Expr("id")).Error; err != nil {
			return nil, err
		}
	}
	var user User
	if err := lockForUpdate(tx).First(&user, id).Error; err != nil {
		return nil, err
	}
	return &user, nil
}
func lockQuotaToken(tx *gorm.DB, id int, unscoped bool) (*Token, error) {
	if unscoped {
		tx = tx.Unscoped()
	}
	if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
		if err := tx.Model(&Token{}).Where("id = ?", id).UpdateColumn("id", gorm.Expr("id")).Error; err != nil {
			return nil, err
		}
	}
	var token Token
	if err := lockForUpdate(tx).First(&token, id).Error; err != nil {
		return nil, err
	}
	return &token, nil
}

// Redis is checked before a debit and invalidated while the SQL account locks
// are held. It never receives an uncommitted monetary increment. Hydration
// takes the same locks, so a crash, lost DEL reply, or rollback is safe: readers
// can only reconstruct the committed SQL balance. No Redis-error SQL fallback.
const invalidateQuotaBalancesScript = `
local function checked(key, field, delta)
 local raw=redis.call('HGET',key,field)
 if not raw or not string.match(raw,'^%-?%d+$') then return false end
 local value=tonumber(raw)
 return value and math.abs(value)<=9007199254740991 and math.abs(value+delta)<=9007199254740991
end
local delta=tonumber(ARGV[1])
local reserve=ARGV[2]=='1'
if ARGV[3]=='1' then
 if not checked(KEYS[1],'Quota',delta) then return -3 end
 if reserve and tonumber(redis.call('HGET',KEYS[1],'Quota')) < -delta then return 0 end
end
if ARGV[4]=='1' then
 if redis.call('EXISTS',KEYS[3])==1 then return -1 end
 if not checked(KEYS[2],'RemainQuota',delta) or not checked(KEYS[2],'UsedQuota',-delta) then return -3 end
 if reserve and ARGV[5]~='1' and tonumber(redis.call('HGET',KEYS[2],'RemainQuota')) < -delta then return 2 end
end
if ARGV[3]=='1' then redis.call('DEL',KEYS[1]) end
if ARGV[4]=='1' then redis.call('DEL',KEYS[2]) end
return 1`

func mutateQuotaInTx(tx *gorm.DB, userID, tokenID, delta int, reserve bool) error {
	if delta == 0 {
		return nil
	}
	if userID < 0 || tokenID < 0 || userID == 0 && tokenID == 0 || delta > common.MaxWalletQuota || delta < -common.MaxWalletQuota || reserve && delta > 0 {
		return ErrWalletQuotaLimitExceeded
	}
	var user *User
	var token *Token
	var err error
	// Always acquire a wallet before its token, including MCP's two-account operation.
	if userID > 0 {
		user, err = lockQuotaUser(tx, userID, !reserve)
		if err != nil {
			return err
		}
		if reserve && user.Status != common.UserStatusEnabled {
			return ErrQuotaOperationConflict
		}
		if reserve && user.Quota < -delta {
			return ErrQuotaWalletInsufficient
		}
		if _, err = common.WalletQuotaFromDecimalStrict(decimal.NewFromInt(int64(user.Quota)).Add(decimal.NewFromInt(int64(delta)))); err != nil {
			return ErrWalletQuotaLimitExceeded
		}
	}
	if tokenID > 0 {
		token, err = lockQuotaToken(tx, tokenID, !reserve)
		if err != nil {
			return err
		}
		if reserve && (token.Status != common.TokenStatusEnabled || token.ExpiredTime != -1 && token.ExpiredTime <= common.GetTimestamp()) {
			return ErrQuotaOperationConflict
		}
		if reserve && !token.UnlimitedQuota && token.RemainQuota < -delta {
			return ErrQuotaTokenInsufficient
		}
		if _, err = common.WalletQuotaFromDecimalStrict(decimal.NewFromInt(int64(token.RemainQuota)).Add(decimal.NewFromInt(int64(delta)))); err != nil {
			return ErrWalletQuotaLimitExceeded
		}
		if _, err = common.WalletQuotaFromDecimalStrict(decimal.NewFromInt(int64(token.UsedQuota)).Sub(decimal.NewFromInt(int64(delta)))); err != nil {
			return ErrWalletQuotaLimitExceeded
		}
	}
	if common.RedisEnabled {
		hasUser, hasToken, unlimited := "0", "0", "0"
		userKey, tokenKey, fenceKey := "quota:unused:user", "quota:unused:token", "quota:unused:fence"
		if user != nil {
			hasUser, userKey = "1", getUserCacheKey(user.Id)
			if err = writeUserCache(user.ToBaseUser(), true); err != nil {
				return fmt.Errorf("%w: %v", ErrQuotaCacheUnavailable, err)
			}
		}
		if token != nil {
			hasToken, tokenKey, fenceKey = "1", getTokenCacheKey(token.Key), getTokenCacheFenceKey(token.Key)
			code, err := writeTokenCache(*token)
			if err != nil || code == 0 {
				return ErrQuotaCacheUnavailable
			}
			if token.UnlimitedQuota {
				unlimited = "1"
			}
		}
		checkBalance := "0"
		if reserve {
			checkBalance = "1"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result, err := common.RDB.Eval(ctx, invalidateQuotaBalancesScript, []string{userKey, tokenKey, fenceKey}, delta, checkBalance, hasUser, hasToken, unlimited).Int()
		if err != nil || result == -1 {
			return ErrQuotaCacheUnavailable
		}
		switch result {
		case 0:
			return ErrQuotaWalletInsufficient
		case 2:
			return ErrQuotaTokenInsufficient
		case 1:
		default:
			return ErrWalletQuotaLimitExceeded
		}
	}
	if user != nil {
		result := tx.Unscoped().Model(&User{}).Where("id = ?", user.Id).Update("quota", gorm.Expr("quota + ?", delta))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
	}
	if token != nil {
		result := tx.Unscoped().Model(&Token{}).Where("id = ?", token.Id).Updates(map[string]any{"remain_quota": gorm.Expr("remain_quota + ?", delta), "used_quota": gorm.Expr("used_quota - ?", delta), "accessed_time": common.GetTimestamp()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
	}
	return nil
}

func applyQuotaOperation(id string) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		// A no-op write obtains SQLite's transaction write lock before reading.
		if err := tx.Model(&QuotaOperation{}).Where("id = ?", id).UpdateColumn("id", gorm.Expr("id")).Error; err != nil {
			return err
		}
		var op QuotaOperation
		if err := lockForUpdate(tx).First(&op, "id = ?", id).Error; err != nil {
			return err
		}
		if op.State == "applied" {
			return nil
		}
		if op.State != "pending" {
			return ErrQuotaOperationConflict
		}
		if err := mutateQuotaInTx(tx, op.UserID, op.TokenID, op.Delta, op.Kind == "reserve"); err != nil {
			return err
		}
		return tx.Model(&op).Update("state", "applied").Error
	})
}

func runQuotaOperation(userID, tokenID, delta int, reserve bool) error {
	if delta == 0 {
		return nil
	}
	if delta > common.MaxWalletQuota || delta < -common.MaxWalletQuota {
		return ErrWalletQuotaLimitExceeded
	}
	kind := "change"
	if reserve {
		kind = "reserve"
	}
	op := QuotaOperation{ID: uuid.NewString(), UserID: userID, TokenID: tokenID, Delta: delta, Kind: kind, State: "pending", CreatedAt: time.Now().UTC()}
	if err := DB.Create(&op).Error; err != nil {
		return err
	}
	err := applyQuotaOperation(op.ID)
	if err == nil {
		return nil
	}
	// Resolve a lost COMMIT acknowledgement by reading the operation, never by
	// calling an ordinary, non-idempotent increment again.
	var stored QuotaOperation
	if readErr := DB.First(&stored, "id = ?", op.ID).Error; readErr == nil && stored.State == "applied" {
		return nil
	}
	// Ordinary callers have no reusable business key. Cancel only a still
	// pending intent, waiting for a possible concurrent commit on the same row.
	cancelled := DB.Model(&QuotaOperation{}).Where("id = ? AND state = ?", op.ID, "pending").Update("state", "cancelled")
	if cancelled.Error == nil {
		if cancelled.RowsAffected == 1 {
			return err
		}
		if readErr := DB.First(&stored, "id = ?", op.ID).Error; readErr == nil && stored.State == "applied" {
			return nil
		}
	}

	return &QuotaOperationPending{ID: op.ID, Cause: err}
}

// AdjustQuotaWithID records a retryable adjustment. Callers must reuse the ID
// for the same business operation, including after a process restart. A normal
// Increase/Decrease call has no stable business identity, so its failed pending
// operation is cancelled instead of being replayed behind the caller's back.
func AdjustQuotaWithID(id string, userID, tokenID, delta int) error {
	if _, err := uuid.Parse(id); err != nil {
		return ErrQuotaOperationConflict
	}
	if delta == 0 {
		return nil
	}
	if userID < 0 || tokenID < 0 || userID == 0 && tokenID == 0 || delta > common.MaxWalletQuota || delta < -common.MaxWalletQuota {
		return ErrWalletQuotaLimitExceeded
	}
	op := QuotaOperation{ID: id, UserID: userID, TokenID: tokenID, Delta: delta, Kind: "adjustment", State: "pending", CreatedAt: time.Now().UTC()}
	if err := DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&op).Error; err != nil {
		return err
	}
	var stored QuotaOperation
	if err := DB.First(&stored, "id = ?", id).Error; err != nil {
		return &QuotaOperationPending{ID: id, Cause: err}
	}
	if stored.UserID != userID || stored.TokenID != tokenID || stored.Delta != delta || stored.Kind != "adjustment" {
		return ErrQuotaOperationConflict
	}
	if err := applyQuotaOperation(id); err != nil {
		if readErr := DB.First(&stored, "id = ?", id).Error; readErr == nil && stored.State == "applied" {
			return nil
		}
		return &QuotaOperationPending{ID: id, Cause: err}
	}
	return nil
}

// RecoverQuotaOperations also runs when MCP is disabled. Adjustment intents
// survive process loss; abandoned model reservations never start charging later.
func RecoverQuotaOperations(ctx context.Context, now time.Time) (int, error) {
	var operations []QuotaOperation
	if err := DB.WithContext(ctx).Where("state = ? AND (kind = ? OR created_at <= ?)", "pending", "adjustment", now.Add(-10*time.Minute)).Order("created_at").Limit(100).Find(&operations).Error; err != nil {
		return 0, err
	}
	count := 0
	var combined error
	for _, op := range operations {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		var err error
		if op.Kind != "adjustment" {
			err = DB.WithContext(ctx).Model(&QuotaOperation{}).Where("id = ? AND state = ?", op.ID, "pending").Update("state", "cancelled").Error
		} else {
			err = applyQuotaOperation(op.ID)
		}
		if err != nil {
			combined = errors.Join(combined, err)
		} else {
			count++
		}
	}
	return count, combined
}
