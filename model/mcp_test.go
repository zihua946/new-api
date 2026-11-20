package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/alicebob/miniredis/v2"
	"github.com/glebarez/sqlite"
	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupMCPDatabase(t *testing.T) {
	t.Helper()
	previousDB, previousType := DB, common.MainDatabaseType()
	var dialect gorm.Dialector = sqlite.Open(filepath.Join(t.TempDir(), "mcp.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	databaseType := common.DatabaseTypeSQLite
	if dsn := os.Getenv("MCP_TEST_MYSQL_DSN"); dsn != "" {
		require.True(t, strings.Contains(dsn, "127.0.0.1:23306") && strings.Contains(dsn, "/mcp_test?"), "MCP tests require the isolated local test database")
		dialect, databaseType = mysql.Open(dsn), common.DatabaseTypeMySQL
	}
	if dsn := os.Getenv("MCP_TEST_POSTGRES_DSN"); dsn != "" {
		require.True(t, strings.Contains(dsn, "host=127.0.0.1") && strings.Contains(dsn, "port=25432") && strings.Contains(dsn, "dbname=mcp_test"), "MCP tests require the isolated local test database")
		dialect, databaseType = postgres.Open(dsn), common.DatabaseTypePostgreSQL
	}
	db, err := gorm.Open(dialect, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	DB = db
	common.SetMainDatabaseType(databaseType)
	initCol()
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(8)
	if databaseType != common.DatabaseTypeSQLite {
		require.NoError(t, db.Migrator().DropTable(&MCPServer{}, &MCPTool{}, &MCPDiscovery{}, &MCPInvocation{}, &MCPBillingOperation{}, &MCPTaskBinding{}, &MCPTestAuthorization{}, &QuotaOperation{}, &Token{}, &User{}))
		var version string
		require.NoError(t, db.Raw("SELECT version()").Scan(&version).Error)
		t.Logf("database: %s", version)
	}
	t.Cleanup(func() {
		require.NoError(t, sqlDB.Close())
		DB = previousDB
		common.SetMainDatabaseType(previousType)
		initCol()
	})
	require.NoError(t, MigrateMCP(db))
}

func setupMCPFunds(t *testing.T, useRedis bool) (User, Token) {
	t.Helper()
	setupMCPDatabase(t)
	previousRedis, previousClient, previousBatch := common.RedisEnabled, common.RDB, common.BatchUpdateEnabled
	common.RedisEnabled, common.BatchUpdateEnabled = useRedis, false
	t.Cleanup(func() {
		common.RedisEnabled, common.RDB, common.BatchUpdateEnabled = previousRedis, previousClient, previousBatch
	})
	if useRedis {
		address := os.Getenv("MCP_TEST_REDIS_ADDR")
		if address == "" {
			address = miniredis.RunT(t).Addr()
		} else {
			require.Equal(t, "127.0.0.1:26379", address)
		}
		client := redis.NewClient(&redis.Options{Addr: address, DB: 14})
		common.RDB = client
		require.NoError(t, client.FlushDB(context.Background()).Err())
		t.Cleanup(func() { _ = client.Close() })
	}
	require.NoError(t, DB.AutoMigrate(&User{}, &Token{}, &QuotaOperation{}, &UserSubscription{}, &SubscriptionPreConsumeRecord{}, &SubscriptionPlan{}))
	user := User{Id: 51, Username: "mcp-funds", Quota: 1000, Status: common.UserStatusEnabled, AuthVersion: 1}
	token := Token{Id: 61, UserId: user.Id, Key: "mcp-funds-token", RemainQuota: 1000, Status: common.TokenStatusEnabled, ExpiredTime: -1}
	require.NoError(t, DB.Create(&user).Error)
	require.NoError(t, DB.Create(&token).Error)
	return user, token
}

func TestMCPFundingCrashRecovery(t *testing.T) {
	for _, stage := range []string{"reserve", "reserve_evicted_user", "refund"} {
		t.Run(stage, func(t *testing.T) {
			user, token := setupMCPFunds(t, true)
			deadline := time.Now().UTC().Add(-time.Minute)
			call := MCPInvocation{ID: "crash-call", IdempotencyHash: "crash-key", UserID: user.Id, PayerUserID: user.Id, TokenID: token.Id, ChargeQuota: 300, Status: "unknown", BillingStatus: "reserving", ReconcileAt: &deadline}
			require.NoError(t, DB.Create(&call).Error)
			if stage == "refund" {
				require.NoError(t, ReserveMCPCharge(call.ID))
			}
			injected := false
			require.NoError(t, DB.Callback().Update().Before("gorm:update").Register("mcp_test_crash", func(tx *gorm.DB) {
				if !injected && tx.Statement.Table == "mcp_billing_operations" {
					injected = true
					tx.AddError(errors.New("simulated SQL failure before operation commit"))
				}
			}))
			if stage == "refund" {
				require.Error(t, RefundMCPCharge(call.ID))
			} else {
				require.Error(t, ReserveMCPCharge(call.ID))
			}
			require.True(t, injected)
			require.NoError(t, DB.Callback().Update().Remove("mcp_test_crash"))
			if stage == "reserve_evicted_user" {
				require.NoError(t, common.RDB.Del(context.Background(), getUserCacheKey(user.Id)).Err())
			}
			for range 2 {
				require.NoError(t, ReconcileMCPCharge(call.ID, time.Now().UTC()))
			}
			require.NoError(t, DB.First(&user, user.Id).Error)
			require.NoError(t, DB.First(&token, token.Id).Error)
			assert.Equal(t, 1000, user.Quota)
			assert.Equal(t, 1000, token.RemainQuota)
			assert.Zero(t, token.UsedQuota)
			cached, err := GetUserCache(user.Id)
			require.NoError(t, err)
			assert.Equal(t, 1000, cached.Quota)
			cachedToken, err := GetTokenByKey(token.Key, true)
			require.NoError(t, err)
			assert.Equal(t, 1000, cachedToken.RemainQuota)
			// A late success never recharges an invocation released at its deadline.
			require.NoError(t, DB.Model(&call).Updates(map[string]any{"status": "succeeded", "billable": true}).Error)
			require.NoError(t, ReconcileMCPCharge(call.ID, time.Now().UTC()))
			require.NoError(t, DB.First(&call, "id = ?", call.ID).Error)
			assert.Equal(t, "refunded", call.BillingStatus)
			assert.Zero(t, call.ChargedQuota)
		})
	}
}

func TestMCPAndModelConcurrentReservation(t *testing.T) {
	for _, useRedis := range []bool{false, true} {
		t.Run(map[bool]string{false: "sql", true: "redis"}[useRedis], func(t *testing.T) {
			user, token := setupMCPFunds(t, useRedis)
			call := MCPInvocation{ID: "concurrent-call", IdempotencyHash: "concurrent-key", UserID: user.Id, PayerUserID: user.Id, TokenID: token.Id, ChargeQuota: 700, Status: "running", BillingStatus: "reserving"}
			require.NoError(t, DB.Create(&call).Error)
			if useRedis {
				require.NoError(t, populateUserCache(user))
				_, err := cacheInitToken(token)
				require.NoError(t, err)
			}
			start := make(chan struct{})
			var winners atomic.Int32
			var wg sync.WaitGroup
			var mcpErr, legacyErr error
			wg.Go(func() {
				<-start
				mcpErr = ReserveMCPCharge(call.ID)
				if mcpErr == nil {
					winners.Add(1)
				}
			})
			wg.Go(func() {
				<-start
				var reserved bool
				reserved, legacyErr = TryReserveUserQuota(user.Id, 700)
				if reserved {
					winners.Add(1)
				}
			})
			close(start)
			wg.Wait()
			require.NoError(t, legacyErr)
			if mcpErr != nil {
				require.ErrorIs(t, mcpErr, ErrMCPInsufficientQuota)
			}
			assert.EqualValues(t, 1, winners.Load())
			require.NoError(t, DB.First(&user, user.Id).Error)
			assert.Equal(t, 300, user.Quota)
			if useRedis {
				cached, err := GetUserCache(user.Id)
				require.NoError(t, err)
				assert.Equal(t, 300, cached.Quota)
			}
		})
	}
}

func TestMCPBillingReserveSettleAndRefund(t *testing.T) {
	for _, useRedis := range []bool{false, true} {
		t.Run(map[bool]string{false: "sql", true: "redis"}[useRedis], func(t *testing.T) {
			setupMCPDatabase(t)
			previousRedis, previousClient, previousBatch := common.RedisEnabled, common.RDB, common.BatchUpdateEnabled
			t.Cleanup(func() {
				common.RedisEnabled, common.RDB, common.BatchUpdateEnabled = previousRedis, previousClient, previousBatch
			})
			common.RedisEnabled, common.BatchUpdateEnabled = useRedis, false
			if useRedis {
				server := miniredis.RunT(t)
				common.RDB = redis.NewClient(&redis.Options{Addr: server.Addr()})
				t.Cleanup(func() { _ = common.RDB.Close() })
			}
			require.NoError(t, DB.AutoMigrate(&User{}, &Token{}, &QuotaOperation{}, &UserSubscription{}, &SubscriptionPreConsumeRecord{}, &SubscriptionPlan{}))
			user := User{Id: 31, Username: "mcp-user", Quota: 1000, Status: common.UserStatusEnabled, AuthVersion: 1}
			token := Token{Id: 41, UserId: user.Id, Key: "mcp-test-token", RemainQuota: 700, Status: common.TokenStatusEnabled, ExpiredTime: -1}
			require.NoError(t, DB.Create(&user).Error)
			require.NoError(t, DB.Create(&token).Error)
			for _, id := range []string{"settle", "refund", "insufficient"} {
				call := MCPInvocation{ID: id, IdempotencyHash: id, UserID: user.Id, PayerUserID: user.Id, TokenID: token.Id, ChargeQuota: 300, Status: "accepted", BillingStatus: "reserving"}
				if id == "insufficient" {
					call.ChargeQuota = 800
				}
				require.NoError(t, DB.Create(&call).Error)
			}
			require.NoError(t, ReserveMCPCharge("settle"))
			require.NoError(t, ReserveMCPCharge("settle"))
			require.NoError(t, SettleMCPCharge("settle"))
			require.NoError(t, SettleMCPCharge("settle"))
			require.NoError(t, ReserveMCPCharge("refund"))
			require.NoError(t, RefundMCPCharge("refund"))
			require.NoError(t, RefundMCPCharge("refund"))
			assert.ErrorIs(t, ReserveMCPCharge("insufficient"), ErrMCPInsufficientQuota)
			require.NoError(t, DB.First(&user, user.Id).Error)
			require.NoError(t, DB.First(&token, token.Id).Error)
			assert.Equal(t, 700, user.Quota)
			assert.Equal(t, 300, user.UsedQuota)
			assert.Equal(t, 400, token.RemainQuota)
			assert.Equal(t, 300, token.UsedQuota)
			if useRedis {
				cached, err := GetUserCache(user.Id)
				require.NoError(t, err)
				assert.Equal(t, 700, cached.Quota)
			}
			// An already settled operation cannot be converted into a refund by a
			// late failure or a repeated timeout worker.
			assert.ErrorIs(t, RefundMCPCharge("settle"), ErrMCPBillingState)
		})
	}
}

func TestMCPConfigurationRevisionAndArchive(t *testing.T) {
	setupMCPDatabase(t)
	server := &MCPServer{ID: "server-one", Slug: "deepseek-web", Name: "Search", Kind: "deepseek", Revision: 1,
		AllowedUserGroups: MCPGroups{"default"}, Connection: MCPConnection{ChannelID: 12}, Limits: MCPLimits{TimeoutSeconds: 30, MaxConcurrency: 10}}
	require.NoError(t, DB.Create(server).Error)
	tool := &MCPTool{ID: "tool-one", ServerID: server.ID, Name: "web_search", Revision: 1, PriceMode: "per_success", UnitPriceUSD: "0.001",
		InputSchema: MCPJSON{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}}}
	require.NoError(t, DB.Create(tool).Error)

	require.NoError(t, UpdateMCPServer(server.ID, 1, map[string]any{"name": "Renamed"}))
	assert.ErrorIs(t, UpdateMCPServer(server.ID, 1, map[string]any{"name": "Stale"}), ErrMCPRevisionConflict)
	got, err := GetMCPServer(server.ID)
	require.NoError(t, err)
	assert.Equal(t, "Renamed", got.Name)
	assert.EqualValues(t, 2, got.Revision)
	assert.Equal(t, MCPGroups{"default"}, got.AllowedUserGroups)
	assert.Equal(t, 12, got.Connection.ChannelID)

	for range 2 {
		require.NoError(t, MigrateMCP(DB))
	}
	var preserved MCPTool
	require.NoError(t, DB.First(&preserved, "id = ?", tool.ID).Error)
	assert.Equal(t, "0.001", preserved.UnitPriceUSD)
	assert.Equal(t, "0.001", preserved.UnitPrice)
	assert.Equal(t, "USD", preserved.Currency)
	assert.Equal(t, "object", preserved.InputSchema["type"])
	assert.Error(t, DB.Create(&MCPServer{ID: "server-two", Slug: server.Slug, Kind: "deepseek", Revision: 1}).Error)

	require.NoError(t, ArchiveMCPServer(server.ID, 2))
	_, err = GetMCPServer(server.ID)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
	assert.NoError(t, DB.First(&preserved, "id = ?", tool.ID).Error)
}

func TestMCPInvocationIdempotency(t *testing.T) {
	setupMCPDatabase(t)
	call := &MCPInvocation{ID: "call-one", ServerID: "server-one", ToolID: "tool-one", UserID: 7,
		IdempotencyHash: "stable-key", ArgumentsHash: "request-a", Status: "accepted", BillingStatus: "free"}
	first, created, err := BeginMCPInvocation(call)
	require.NoError(t, err)
	require.True(t, created)
	assert.Equal(t, call.ID, first.ID)
	retry := *call
	retry.ID = "call-two"
	prior, created, err := BeginMCPInvocation(&retry)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, call.ID, prior.ID)
	retry.ArgumentsHash = "request-b"
	_, _, err = BeginMCPInvocation(&retry)
	assert.ErrorIs(t, err, ErrMCPIdempotencyConflict)
	var count int64
	require.NoError(t, DB.Model(&MCPInvocation{}).Count(&count).Error)
	assert.EqualValues(t, 1, count)
}

func TestMCPFundingRejectsInvalidAccumulation(t *testing.T) {
	for _, field := range []string{"sql_token", "redis_token", "settlement", "operation_owner"} {
		t.Run(field, func(t *testing.T) {
			user, token := setupMCPFunds(t, true)
			call := MCPInvocation{ID: "bounded-call", IdempotencyHash: "bounded-key", UserID: user.Id, PayerUserID: user.Id, TokenID: token.Id, ChargeQuota: 300, Status: "running", BillingStatus: "reserving"}
			require.NoError(t, DB.Create(&call).Error)
			switch field {
			case "sql_token":
				require.NoError(t, DB.Model(&token).Update("used_quota", common.MaxWalletQuota).Error)
			case "redis_token":
				require.NoError(t, populateUserCache(user))
				_, err := cacheInitToken(token)
				require.NoError(t, err)
				require.NoError(t, common.RDB.HSet(context.Background(), getTokenCacheKey(token.Key), "UsedQuota", common.MaxWalletQuota).Err())
			case "operation_owner":
				require.NoError(t, DB.Create(&MCPBillingOperation{ID: call.ID + ":reserve", InvocationID: call.ID, Kind: "reserve", Amount: 300, PayerUserID: user.Id + 1, TokenID: token.Id, State: "pending"}).Error)
			case "settlement":
				require.NoError(t, ReserveMCPCharge(call.ID))
				require.NoError(t, DB.Model(&user).Update("used_quota", common.MaxWalletQuota).Error)
				require.Error(t, SettleMCPCharge(call.ID))
				require.NoError(t, DB.First(&user, user.Id).Error)
				assert.Equal(t, common.MaxWalletQuota, user.UsedQuota)
				return
			}
			require.Error(t, ReserveMCPCharge(call.ID))
			require.NoError(t, DB.First(&user, user.Id).Error)
			assert.Equal(t, 1000, user.Quota)
			cached, err := GetUserCache(user.Id)
			require.NoError(t, err)
			assert.Equal(t, 1000, cached.Quota, "all cache balances must be checked before any mutation")
		})
	}
}

func TestMCPNullableConfiguration(t *testing.T) {
	var groups MCPGroups
	var object MCPJSON
	var connection MCPConnection
	var limits MCPLimits
	assert.NoError(t, groups.Scan(nil))
	assert.NoError(t, object.Scan(nil))
	assert.NoError(t, connection.Scan(nil))
	assert.NoError(t, limits.Scan(nil))
}

func TestMCPBillingWithModelBatchAndPersonalFallback(t *testing.T) {
	for _, useRedis := range []bool{false, true} {
		t.Run(map[bool]string{false: "sql", true: "redis"}[useRedis], func(t *testing.T) {
			user, token := setupMCPFunds(t, useRedis)
			resetBatchUpdateTestState(t)
			common.BatchUpdateEnabled = true
			require.NoError(t, DB.Model(&user).Update("setting", `{"personal_billing_fallback":true}`).Error)
			team := User{Username: "mcp-team", AffCode: "mcp-team", Quota: 100, Status: common.UserStatusEnabled, AuthVersion: 1}
			require.NoError(t, DB.Create(&team).Error)
			call := MCPInvocation{ID: "team-call", IdempotencyHash: "team-key", UserID: user.Id, PayerUserID: team.Id, BillingUnitID: 7, TokenID: token.Id, ChargeQuota: 300, Status: "running", BillingStatus: "reserving"}
			require.NoError(t, DB.Create(&call).Error)
			require.ErrorIs(t, ReserveMCPCharge(call.ID), ErrMCPInsufficientQuota)
			t.Setenv("BILLING_TEAM_PERSONAL_FALLBACK_ENABLED", "false")
			require.ErrorIs(t, SwitchMCPToPersonalPayer(call.ID), ErrMCPInsufficientQuota)
			t.Setenv("BILLING_TEAM_PERSONAL_FALLBACK_ENABLED", "true")
			require.NoError(t, SwitchMCPToPersonalPayer(call.ID))
			require.NoError(t, ReserveMCPCharge(call.ID))
			require.NoError(t, SettleMCPCharge(call.ID))
			reserved, err := TryReserveUserQuota(user.Id, 400)
			require.NoError(t, err)
			require.True(t, reserved)
			reserved, err = TryReserveTokenQuota(token.Id, token.Key, 400, false)
			require.NoError(t, err)
			require.True(t, reserved)
			other := MCPInvocation{ID: "batch-call", IdempotencyHash: "batch-key", UserID: user.Id, PayerUserID: user.Id, TokenID: token.Id, ChargeQuota: 400, Status: "running", BillingStatus: "reserving"}
			require.NoError(t, DB.Create(&other).Error)
			require.ErrorIs(t, ReserveMCPCharge(other.ID), ErrMCPInsufficientQuota)
			batchUpdate()
			require.NoError(t, DB.First(&user, user.Id).Error)
			require.NoError(t, DB.First(&token, token.Id).Error)
			require.NoError(t, DB.First(&team, team.Id).Error)
			assert.Equal(t, 300, user.Quota)
			assert.Equal(t, 300, user.UsedQuota)
			assert.Equal(t, 300, token.RemainQuota)
			assert.Equal(t, 700, token.UsedQuota)
			assert.Equal(t, 100, team.Quota)
		})
	}
}

func TestMCPDoesNotSpendModelBatchDeductionsAfterCacheEviction(t *testing.T) {
	user, token := setupMCPFunds(t, true)
	resetBatchUpdateTestState(t)
	common.BatchUpdateEnabled = true
	reserved, err := TryReserveUserQuota(user.Id, 700)
	require.NoError(t, err)
	require.True(t, reserved)
	require.NoError(t, common.RDB.Del(context.Background(), getUserCacheKey(user.Id)).Err())
	call := MCPInvocation{ID: "eviction-call", IdempotencyHash: "eviction-key", UserID: user.Id, PayerUserID: user.Id, TokenID: token.Id, ChargeQuota: 700, Status: "running", BillingStatus: "reserving"}
	require.NoError(t, DB.Create(&call).Error)
	require.ErrorIs(t, ReserveMCPCharge(call.ID), ErrMCPInsufficientQuota)
	batchUpdate()
	require.NoError(t, DB.First(&user, user.Id).Error)
	assert.Equal(t, 300, user.Quota)
}

// The same contracts run on the isolated SQLite/MySQL/PostgreSQL fixtures.
func TestSharedQuotaDurableRecovery(t *testing.T) {
	for _, useRedis := range []bool{false, true} {
		t.Run(map[bool]string{false: "sql", true: "redis"}[useRedis], func(t *testing.T) {
			user, token := setupMCPFunds(t, useRedis)
			common.BatchUpdateEnabled = true
			ok, err := TryReserveUserQuota(user.Id, 700)
			require.NoError(t, err)
			require.True(t, ok)
			ok, err = TryReserveTokenQuota(token.Id, token.Key, 700, false)
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, 300, getUserQuotaFromDB(t, user.Id))
			assert.Equal(t, 300, getTokenFromDB(t, token.Id).RemainQuota)

			operationID := uuid.NewString()
			require.NoError(t, DB.Callback().Update().Before("gorm:update").Register("quota_completion_failure", func(tx *gorm.DB) {
				updates, _ := tx.Statement.Dest.(map[string]any)
				if tx.Statement.Table == "quota_operations" && updates["state"] == "applied" {
					tx.AddError(errors.New("connection interrupted before commit"))
				}
			}))
			err = AdjustQuotaWithID(operationID, user.Id, token.Id, 300)
			var pending *QuotaOperationPending
			require.ErrorAs(t, err, &pending)
			assert.Equal(t, operationID, pending.ID)
			require.NoError(t, DB.Callback().Update().Remove("quota_completion_failure"))
			assert.Equal(t, 300, getUserQuotaFromDB(t, user.Id), "rolled back wallet and token together")
			assert.Equal(t, 300, getTokenFromDB(t, token.Id).RemainQuota)
			if useRedis {
				require.NoError(t, common.RDB.Del(context.Background(), getUserCacheKey(user.Id), getTokenCacheKey(token.Key)).Err())
			}
			// Recovery needs only persisted state, not an in-memory billing session.
			for range 2 {
				_, err = RecoverQuotaOperations(context.Background(), time.Now().UTC())
				require.NoError(t, err)
			}
			require.NoError(t, AdjustQuotaWithID(operationID, user.Id, token.Id, 300))
			require.ErrorIs(t, AdjustQuotaWithID(operationID, user.Id, token.Id, 301), ErrQuotaOperationConflict)
			assert.Equal(t, 600, getUserQuotaFromDB(t, user.Id))
			assert.Equal(t, 600, getTokenFromDB(t, token.Id).RemainQuota)
			assert.Equal(t, 400, getTokenFromDB(t, token.Id).UsedQuota)
			batchUpdate()
			assert.Equal(t, 600, getUserQuotaFromDB(t, user.Id), "statistics flush cannot apply the charge twice")

			// A reservation interrupted before its transaction must never become
			// a new debit after the caller has already rejected the request.
			orphan := QuotaOperation{ID: uuid.NewString(), UserID: user.Id, Delta: -500, Kind: "reserve", State: "pending", CreatedAt: time.Now().UTC().Add(-11 * time.Minute)}
			require.NoError(t, DB.Create(&orphan).Error)
			_, err = RecoverQuotaOperations(context.Background(), time.Now().UTC())
			require.NoError(t, err)
			require.NoError(t, DB.First(&orphan, "id = ?", orphan.ID).Error)
			assert.Equal(t, "cancelled", orphan.State)
			assert.Equal(t, 600, getUserQuotaFromDB(t, user.Id))
		})
	}
}

type lostQuotaCacheReply struct{ lost bool }

func (h *lostQuotaCacheReply) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	return ctx, nil
}
func (h *lostQuotaCacheReply) AfterProcess(ctx context.Context, cmd redis.Cmder) error {
	args := cmd.Args()
	if !h.lost && len(args) > 1 && args[0] == "eval" && args[1] == invalidateQuotaBalancesScript {
		h.lost = true
		return errors.New("lost Redis reply after successful invalidation")
	}
	return nil
}
func (h *lostQuotaCacheReply) BeforeProcessPipeline(ctx context.Context, cmds []redis.Cmder) (context.Context, error) {
	return ctx, nil
}
func (h *lostQuotaCacheReply) AfterProcessPipeline(ctx context.Context, cmds []redis.Cmder) error {
	return nil
}

func TestSharedQuotaLostCacheReplyAndStaleHydration(t *testing.T) {
	user, token := setupMCPFunds(t, true)
	hook := &lostQuotaCacheReply{}
	common.RDB.AddHook(hook)
	ok, err := TryReserveUserQuota(user.Id, 700)
	require.ErrorIs(t, err, ErrQuotaCacheUnavailable)
	require.True(t, hook.lost)
	assert.False(t, ok)
	assert.Equal(t, 1000, getUserQuotaFromDB(t, user.Id))
	cached, err := GetUserCache(user.Id)
	require.NoError(t, err)
	assert.Equal(t, 1000, cached.Quota)

	ok, err = TryReserveUserQuota(user.Id, 700)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = TryReserveTokenQuota(token.Id, token.Key, 700, false)
	require.NoError(t, err)
	require.True(t, ok)
	// Explicitly publish the pre-charge snapshots after the commit. Hydration
	// must re-read SQL, including when the old cache has been evicted.
	require.NoError(t, populateUserCache(user))
	_, err = cacheInitToken(token)
	require.NoError(t, err)
	cached, err = GetUserCache(user.Id)
	require.NoError(t, err)
	assert.Equal(t, 300, cached.Quota)
	cachedToken, err := GetTokenByKey(token.Key, true)
	require.NoError(t, err)
	assert.Equal(t, 300, cachedToken.RemainQuota)
	// A late post-commit credit notification must not add a credit twice.
	require.NoError(t, DB.Model(&user).Update("quota", gorm.Expr("quota + ?", 100)).Error)
	require.NoError(t, invalidateUserCache(user.Id))
	_, err = GetUserCache(user.Id)
	require.NoError(t, err)
	syncCreditUserQuotaCache(user.Id, 100, "test credit")
	cached, err = GetUserCache(user.Id)
	require.NoError(t, err)
	assert.Equal(t, 400, cached.Quota)
}

// Delegate to the real database, then lose only the acknowledgement. This is
// different from a rollback: retrying a plain decrement would double charge.
type lostQuotaCommitPool struct {
	*sql.DB
	commits int
	lost    bool
}
type lostQuotaCommitTx struct {
	*sql.Tx
	pool *lostQuotaCommitPool
}

func (p *lostQuotaCommitPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := p.DB.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &lostQuotaCommitTx{Tx: tx, pool: p}, nil
}
func (tx *lostQuotaCommitTx) Commit() error {
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	tx.pool.commits++
	if tx.pool.commits == 2 {
		tx.pool.lost = true
		return errors.New("lost acknowledgement of committed quota transaction")
	}
	return nil
}
func TestSharedQuotaLostSQLCommitAcknowledgement(t *testing.T) {
	user, _ := setupMCPFunds(t, true)
	sqlDB, err := DB.DB()
	require.NoError(t, err)
	pool := &lostQuotaCommitPool{DB: sqlDB}
	previousPool, previousStatementPool := DB.ConnPool, DB.Statement.ConnPool
	DB.ConnPool, DB.Statement.ConnPool = pool, pool
	t.Cleanup(func() { DB.ConnPool, DB.Statement.ConnPool = previousPool, previousStatementPool })
	ok, err := TryReserveUserQuota(user.Id, 700)
	require.True(t, pool.lost, "test must lose the quota commit acknowledgement, not the intent insert")
	require.NoError(t, err, "the committed operation resolves the uncertain result")
	require.True(t, ok)
	assert.Equal(t, 300, getUserQuotaFromDB(t, user.Id))
	_, err = RecoverQuotaOperations(context.Background(), time.Now().UTC().Add(11*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 300, getUserQuotaFromDB(t, user.Id))
}

func TestMCPSubscriptionFundingMatchesPreference(t *testing.T) {
	user, token := setupMCPFunds(t, false)
	require.NoError(t, DB.AutoMigrate(&SubscriptionPlan{}, &UserSubscription{}, &SubscriptionPreConsumeRecord{}))
	plan := SubscriptionPlan{Title: "Test subscription"}
	require.NoError(t, DB.Create(&plan).Error)
	sub := UserSubscription{UserId: user.Id, PlanId: plan.Id, AmountTotal: 500, Status: "active", EndTime: time.Now().Add(time.Hour).Unix()}
	require.NoError(t, DB.Create(&sub).Error)
	for i, test := range []struct {
		pref   string
		amount int
		want   string
		refund bool
	}{{"subscription_only", 200, "subscription", true}, {"wallet_first", 200, "wallet", false}, {"subscription_first", 400, "subscription", false}, {"subscription_first", 200, "", false}} {
		call := MCPInvocation{ID: fmt.Sprintf("sub-call-%d", i), IdempotencyHash: fmt.Sprintf("sub-key-%d", i), PayerUserID: user.Id, UserID: user.Id, TokenID: token.Id, FundingPreference: test.pref, BillingStatus: "reserving", ChargeQuota: test.amount}
		require.NoError(t, DB.Create(&call).Error)
		err := ReserveMCPCharge(call.ID)
		if test.want == "" {
			require.ErrorIs(t, err, ErrMCPInsufficientQuota)
			continue
		}
		require.NoError(t, err)
		require.NoError(t, DB.First(&call, "id = ?", call.ID).Error)
		assert.Equal(t, test.want, call.FundingSource)
		if test.refund {
			require.NoError(t, RefundMCPCharge(call.ID))
			require.NoError(t, RefundMCPCharge(call.ID))
		} else {
			require.NoError(t, SettleMCPCharge(call.ID))
		}
	}
	require.NoError(t, DB.First(&sub, sub.Id).Error)
	assert.EqualValues(t, 400, sub.AmountUsed)
	require.NoError(t, DB.First(&user, user.Id).Error)
	assert.Equal(t, 800, user.Quota)
	require.NoError(t, DB.First(&token, token.Id).Error)
	assert.Equal(t, 400, token.RemainQuota)
}

func TestMCPAuthenticationColumnMigration(t *testing.T) {
	setupMCPDatabase(t)
	if DB.Dialector.Name() == "sqlite" {
		var version string
		require.NoError(t, DB.Raw("SELECT sqlite_version()").Scan(&version).Error)
		t.Logf("database: SQLite %s", version)
	}
	server := MCPServer{ID: "migration-server", Slug: "migration-server", Name: "Saved service", Catalog: true, Kind: "remote_mcp", Revision: 3, Connection: MCPConnection{URL: "https://example.test/mcp", Transport: "streamable_http", ClientID: "registered"}}
	require.NoError(t, DB.Create(&server).Error)
	require.NoError(t, DB.Exec("ALTER TABLE mcp_servers ADD COLUMN auth_mode VARCHAR(24)").Error)
	require.NoError(t, DB.Model(&MCPServer{}).Where("id = ?", server.ID).UpdateColumn("auth_mode", "oauth2").Error)
	for range 2 {
		require.NoError(t, MigrateMCP(DB))
		assert.False(t, DB.Migrator().HasColumn("mcp_servers", "auth_mode"))
		saved, err := GetMCPServer(server.ID)
		require.NoError(t, err)
		assert.Equal(t, server.Connection, saved.Connection)
		assert.Equal(t, "oauth2", saved.AuthMode)
		assert.EqualValues(t, 3, saved.Revision)
	}
}
