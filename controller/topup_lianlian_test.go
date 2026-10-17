package controller

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service/lianlianpay"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const lianlianTestUserID = 77

const lianlianTestCreatePayResponse = `{"ret_code":"0000","ret_msg":"交易成功","payload":"https://qr.alipay.com/bax1","platform_txno":"LL20261016001","txn_seqno":"ignored"}`

type lianlianTestEnv struct {
	db              *gorm.DB
	gateway         *httptest.Server
	merchantPubKey  string
	platformPrivKey string
	queryResponse   string
	lastPath        string
	lastBody        []byte
	lastSignature   string
	requests        int
}

func newTestRSAKeyPEM(t *testing.T) (privateKeyPEM string, publicKeyPEM string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}))
}

func newLianlianTestEnv(t *testing.T, gatewayResponse string) *lianlianTestEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	confirmPaymentComplianceForTest(t)
	initModelListColumnNames(t)

	oldDB, oldLogDB := model.DB, model.LOG_DB
	oldMainType, oldLogType := common.MainDatabaseType(), common.LogDatabaseType()
	oldRedis, oldBatch := common.RedisEnabled, common.BatchUpdateEnabled
	oldQuotaPerUnit := common.QuotaPerUnit
	t.Cleanup(func() {
		model.DB, model.LOG_DB = oldDB, oldLogDB
		common.SetDatabaseTypes(oldMainType, oldLogType)
		common.RedisEnabled, common.BatchUpdateEnabled = oldRedis, oldBatch
		common.QuotaPerUnit = oldQuotaPerUnit
	})

	dir := t.TempDir()
	db, err := gorm.Open(sqlite.Open(filepath.Join(dir, "main.db")), &gorm.Config{})
	require.NoError(t, err)
	logDB, err := gorm.Open(sqlite.Open(filepath.Join(dir, "log.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.TopUp{}))
	require.NoError(t, logDB.AutoMigrate(&model.Log{}))
	model.DB, model.LOG_DB = db, logDB
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.RedisEnabled = false
	common.BatchUpdateEnabled = false
	common.QuotaPerUnit = 500000
	t.Cleanup(func() {
		for _, handle := range []*gorm.DB{db, logDB} {
			native, dbErr := handle.DB()
			if dbErr == nil {
				_ = native.Close()
			}
		}
	})

	require.NoError(t, db.Create(&model.User{
		Id:       lianlianTestUserID,
		Username: "lianlian_topup_user",
		AffCode:  "lianlian-topup-aff",
		Quota:    1000,
		Status:   common.UserStatusEnabled,
	}).Error)

	env := &lianlianTestEnv{db: db}
	merchantPriv, merchantPub := newTestRSAKeyPEM(t)
	platformPriv, platformPub := newTestRSAKeyPEM(t)
	env.merchantPubKey = merchantPub
	env.platformPrivKey = platformPriv

	env.gateway = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.requests++
		env.lastPath = r.URL.Path
		env.lastSignature = r.Header.Get(lianlianpay.HeaderSignatureData)
		body, readErr := io.ReadAll(r.Body)
		require.NoError(t, readErr)
		env.lastBody = body
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, lianlianpay.OrderQueryPath) {
			_, _ = w.Write([]byte(env.queryResponse))
			return
		}
		_, _ = w.Write([]byte(gatewayResponse))
	}))
	t.Cleanup(env.gateway.Close)

	oldLianlian := setting.LianlianEnabled
	oldEnv := setting.LianlianEnv
	oldGateway := setting.LianlianGateway
	oldMchID := setting.LianlianMchId
	oldPrivateKey := setting.LianlianPrivateKey
	oldPublicCert := setting.LianlianPlatformPublicCert
	oldBusiType := setting.LianlianBusiType
	oldMinTopUp := operation_setting.MinTopUp
	oldCallback := operation_setting.CustomCallbackAddress
	oldPrice := operation_setting.Price
	oldExchangeRate := operation_setting.USDExchangeRate
	oldDisplayType := operation_setting.GetGeneralSetting().QuotaDisplayType
	oldRatios := common.TopupGroupRatio2JSONString()
	t.Cleanup(func() {
		setting.LianlianEnabled = oldLianlian
		setting.LianlianEnv = oldEnv
		setting.LianlianGateway = oldGateway
		setting.LianlianMchId = oldMchID
		setting.LianlianPrivateKey = oldPrivateKey
		setting.LianlianPlatformPublicCert = oldPublicCert
		setting.LianlianBusiType = oldBusiType
		operation_setting.MinTopUp = oldMinTopUp
		operation_setting.CustomCallbackAddress = oldCallback
		operation_setting.Price = oldPrice
		operation_setting.USDExchangeRate = oldExchangeRate
		operation_setting.GetGeneralSetting().QuotaDisplayType = oldDisplayType
		require.NoError(t, common.UpdateTopupGroupRatioByJSONString(oldRatios))
	})

	setting.LianlianEnabled = true
	setting.LianlianEnv = lianlianpay.EnvTest
	setting.LianlianGateway = env.gateway.URL + "/mch"
	setting.LianlianMchId = "2020042200284052"
	setting.LianlianPrivateKey = merchantPriv
	setting.LianlianPlatformPublicCert = platformPub
	setting.LianlianBusiType = lianlianpay.DefaultBusiType
	operation_setting.MinTopUp = 1
	operation_setting.CustomCallbackAddress = "https://api.example.test"
	operation_setting.Price = 7.3
	operation_setting.USDExchangeRate = 7.3
	operation_setting.GetGeneralSetting().QuotaDisplayType = operation_setting.QuotaDisplayTypeCNY
	require.NoError(t, common.UpdateTopupGroupRatioByJSONString(`{"default":1}`))
	return env
}

func lianlianTestRouter(userId int) *gin.Engine {
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("id", userId)
	})
	router.POST("/lianlian/pay", RequestLianlianPay)
	router.POST("/lianlian/amount", RequestLianlianAmount)
	router.POST("/lianlian/notify", LianlianNotify)
	router.GET("/lianlian/order/:tradeNo", GetLianlianOrder)
	return router
}

func lianlianTestPost(t *testing.T, router *gin.Engine, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	return recorder
}

func postLianlianNotify(t *testing.T, router *gin.Engine, body string, signature string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/lianlian/notify", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(lianlianpay.HeaderSignatureData, signature)
	router.ServeHTTP(recorder, request)
	return recorder
}

func lianlianNotifyBody(t *testing.T, env *lianlianTestEnv, tradeNo string, amount string) (string, string) {
	t.Helper()
	return lianlianNotifyBodyFor(t, env, tradeNo, amount, setting.LianlianMchId, lianlianpay.PayTypeAlipayNative)
}

func lianlianNotifyBodyFor(t *testing.T, env *lianlianTestEnv, tradeNo string, amount string, mchID string, payType string) (string, string) {
	t.Helper()
	body := fmt.Sprintf(
		`{"mch_id":%q,"txn_seqno":%q,"order_amount":%q,"result_pay":"SUCCESS","platform_txno":"LL2026101600PLAT","pay_method_infos":[{"pay_type":%q,"amount":%q}]}`,
		mchID, tradeNo, amount, payType, amount,
	)
	signature, err := lianlianpay.Sign(env.platformPrivKey, []byte(body))
	require.NoError(t, err)
	return body, signature
}

func TestRequestLianlianPayCreatesOrderAndQRCode(t *testing.T) {
	env := newLianlianTestEnv(t, lianlianTestCreatePayResponse)
	router := lianlianTestRouter(lianlianTestUserID)
	operation_setting.Price = 7.305

	recorder := lianlianTestPost(t, router, "/lianlian/pay", `{"amount":10,"payment_method":"alipay"}`)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), `"message":"success"`)
	require.Contains(t, recorder.Body.String(), `"qr_code":"https://qr.alipay.com/bax1"`)
	assert.NotContains(t, recorder.Body.String(), "money_cents")

	var response struct {
		Data struct {
			TradeNo         string  `json:"trade_no"`
			ProviderTradeNo string  `json:"provider_trade_no"`
			QRCode          string  `json:"qr_code"`
			Money           float64 `json:"money"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.NotEmpty(t, response.Data.TradeNo)
	assert.InDelta(t, 73.05, response.Data.Money, 1e-9)
	assert.Equal(t, "LL20261016001", response.Data.ProviderTradeNo)

	order := model.GetTopUpByTradeNo(response.Data.TradeNo)
	require.NotNil(t, order)
	assert.Equal(t, model.PaymentProviderLianlian, order.PaymentProvider)
	assert.Equal(t, "alipay", order.PaymentMethod)
	assert.Equal(t, int64(10), order.Amount)
	assert.InDelta(t, getPayMoney(10, "default"), order.Money, 1e-9)
	assert.Equal(t, "LL20261016001", order.ProviderTradeNo)
	assert.Equal(t, common.TopUpStatusPending, order.Status)

	assert.Equal(t, "/mch"+lianlianpay.CreatePayPath, env.lastPath)
	assert.NotEmpty(t, env.lastSignature)
	verified, err := lianlianpay.Verify(env.merchantPubKey, env.lastBody, env.lastSignature)
	require.NoError(t, err)
	assert.True(t, verified)
	assert.Contains(t, string(env.lastBody), `"order_amount":"73.05"`)
	assert.Contains(t, string(env.lastBody), `"pay_expire":15`)
	assert.Contains(t, string(env.lastBody), `"pay_type":"ALIPAY_NATIVE"`)
	assert.Contains(t, string(env.lastBody), `"notify_url":"https://api.example.test/api/user/lianlian/notify"`)

	query := httptest.NewRecorder()
	router.ServeHTTP(query, httptest.NewRequest(http.MethodGet, "/lianlian/order/"+response.Data.TradeNo, nil))
	assert.Contains(t, query.Body.String(), `"status":"pending"`)
	assert.Contains(t, query.Body.String(), `"provider_trade_no":"LL20261016001"`)
	assert.NotContains(t, query.Body.String(), "money_cents")
}

func TestRequestLianlianPayMarksOrderFailedWhenGatewayRejects(t *testing.T) {
	env := newLianlianTestEnv(t, `{"ret_code":"1001","ret_msg":"商户号不存在"}`)
	router := lianlianTestRouter(lianlianTestUserID)

	recorder := lianlianTestPost(t, router, "/lianlian/pay", `{"amount":10,"payment_method":"alipay"}`)
	assert.Contains(t, recorder.Body.String(), `"message":"error"`)

	var orders []model.TopUp
	require.NoError(t, env.db.Where("payment_provider = ?", model.PaymentProviderLianlian).Find(&orders).Error)
	require.Len(t, orders, 1)
	assert.Equal(t, common.TopUpStatusFailed, orders[0].Status)
}

func TestRequestLianlianAmountUsesExistingTopupPricing(t *testing.T) {
	newLianlianTestEnv(t, lianlianTestCreatePayResponse)
	router := lianlianTestRouter(lianlianTestUserID)

	recorder := lianlianTestPost(t, router, "/lianlian/amount", `{"amount":10,"payment_method":"wxpay"}`)
	assert.JSONEq(t, `{"message":"success","data":"73.00"}`, recorder.Body.String())

	setting.LianlianEnabled = false
	recorder = lianlianTestPost(t, router, "/lianlian/amount", `{"amount":10,"payment_method":"wxpay"}`)
	assert.Contains(t, recorder.Body.String(), "充值服务未启用")
	setting.LianlianEnabled = true

	recorder = lianlianTestPost(t, router, "/lianlian/amount", `{"amount":0,"payment_method":"wxpay"}`)
	assert.Contains(t, recorder.Body.String(), "充值数量不能小于 1")
	recorder = lianlianTestPost(t, router, "/lianlian/amount", `{"amount":10,"payment_method":"card"}`)
	assert.Contains(t, recorder.Body.String(), "支付方式不存在")
}

func TestRequestLianlianPayRejectsGatewayWithoutMchSuffix(t *testing.T) {
	env := newLianlianTestEnv(t, lianlianTestCreatePayResponse)
	router := lianlianTestRouter(lianlianTestUserID)
	setting.LianlianGateway = "https://openapi.example.test"

	recorder := lianlianTestPost(t, router, "/lianlian/pay", `{"amount":10,"payment_method":"alipay"}`)
	assert.Contains(t, recorder.Body.String(), "支付服务暂时不可用")
	assert.Zero(t, env.requests)

	var orders []model.TopUp
	require.NoError(t, env.db.Find(&orders).Error)
	assert.Empty(t, orders)
}

func TestLianlianNotifyCreditsQuotaAndIsIdempotent(t *testing.T) {
	env := newLianlianTestEnv(t, lianlianTestCreatePayResponse)
	router := lianlianTestRouter(lianlianTestUserID)
	tradeNo := "USR77NONOTIFY"
	require.NoError(t, env.db.Create(&model.TopUp{
		UserId:          lianlianTestUserID,
		Amount:          10,
		Money:           10,
		TradeNo:         tradeNo,
		PaymentMethod:   "alipay",
		PaymentProvider: model.PaymentProviderLianlian,
		Status:          common.TopUpStatusPending,
	}).Error)
	body, signature := lianlianNotifyBody(t, env, tradeNo, "10.00")
	notify := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/lianlian/notify", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(lianlianpay.HeaderSignatureData, signature)
		router.ServeHTTP(recorder, request)
		return recorder
	}

	require.Equal(t, lianlianpay.SuccessResponse, notify().Body.String())
	user, err := model.GetUserById(lianlianTestUserID, false)
	require.NoError(t, err)
	assert.Equal(t, 1000+5000000, user.Quota)
	order := model.GetTopUpByTradeNo(tradeNo)
	require.NotNil(t, order)
	assert.Equal(t, "LL2026101600PLAT", order.ProviderTradeNo)

	require.Equal(t, lianlianpay.SuccessResponse, notify().Body.String())
	user, err = model.GetUserById(lianlianTestUserID, false)
	require.NoError(t, err)
	assert.Equal(t, 1000+5000000, user.Quota)
}

func TestLianlianNotifyRejectsInvalidRequests(t *testing.T) {
	env := newLianlianTestEnv(t, lianlianTestCreatePayResponse)
	router := lianlianTestRouter(lianlianTestUserID)
	tradeNo := "USR77NOINVALID"
	require.NoError(t, env.db.Create(&model.TopUp{
		UserId:          lianlianTestUserID,
		Amount:          10,
		Money:           10,
		TradeNo:         tradeNo,
		PaymentMethod:   "alipay",
		PaymentProvider: model.PaymentProviderLianlian,
		Status:          common.TopUpStatusPending,
	}).Error)

	postSigned := func(body string, signature string) string {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/lianlian/notify", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(lianlianpay.HeaderSignatureData, signature)
		router.ServeHTTP(recorder, request)
		return recorder.Body.String()
	}

	validBody, validSignature := lianlianNotifyBody(t, env, tradeNo, "10.00")
	tampered := strings.Replace(validBody, `"10.00"`, `"9.99"`, 1)
	assert.Equal(t, lianlianpay.FailResponse, postSigned(tampered, validSignature))
	assert.Equal(t, lianlianpay.FailResponse, postSigned(validBody, "AAAA"))

	mismatchBody, mismatchSignature := lianlianNotifyBody(t, env, tradeNo, "9.99")
	assert.Equal(t, lianlianpay.FailResponse, postSigned(mismatchBody, mismatchSignature))
	order := model.GetTopUpByTradeNo(tradeNo)
	require.NotNil(t, order)
	assert.Equal(t, common.TopUpStatusPending, order.Status)

	missingBody, missingSignature := lianlianNotifyBody(t, env, "USR77NOMISSING", "10.00")
	assert.Equal(t, lianlianpay.FailResponse, postSigned(missingBody, missingSignature))

	missingAmountBody := fmt.Sprintf(
		`{"mch_id":%q,"txn_seqno":%q,"result_pay":"SUCCESS","platform_txno":"LLNOAMOUNT","pay_method_infos":[{"pay_type":%q}]}`,
		setting.LianlianMchId,
		tradeNo,
		lianlianpay.PayTypeAlipayNative,
	)
	missingAmountSignature, err := lianlianpay.Sign(env.platformPrivKey, []byte(missingAmountBody))
	require.NoError(t, err)
	assert.Equal(t, lianlianpay.FailResponse, postSigned(missingAmountBody, missingAmountSignature))
}

func TestLianlianNotifyRejectsMerchantMismatch(t *testing.T) {
	env := newLianlianTestEnv(t, lianlianTestCreatePayResponse)
	router := lianlianTestRouter(lianlianTestUserID)
	tradeNo := "USR77NOMCHMISMATCH"
	require.NoError(t, env.db.Create(&model.TopUp{
		UserId:          lianlianTestUserID,
		Amount:          10,
		Money:           10,
		TradeNo:         tradeNo,
		PaymentMethod:   lianlianpay.PayMethodAlipay,
		PaymentProvider: model.PaymentProviderLianlian,
		Status:          common.TopUpStatusPending,
	}).Error)

	body, signature := lianlianNotifyBodyFor(
		t,
		env,
		tradeNo,
		"10.00",
		"another-merchant",
		lianlianpay.PayTypeAlipayNative,
	)
	recorder := postLianlianNotify(t, router, body, signature)

	assert.Equal(t, lianlianpay.FailResponse, recorder.Body.String())
	order := model.GetTopUpByTradeNo(tradeNo)
	require.NotNil(t, order)
	assert.Equal(t, common.TopUpStatusPending, order.Status)
}

func TestLianlianNotifyRejectsPaymentMethodMismatch(t *testing.T) {
	env := newLianlianTestEnv(t, lianlianTestCreatePayResponse)
	router := lianlianTestRouter(lianlianTestUserID)
	tradeNo := "USR77NOPAYMETHODMISMATCH"
	require.NoError(t, env.db.Create(&model.TopUp{
		UserId:          lianlianTestUserID,
		Amount:          10,
		Money:           10,
		TradeNo:         tradeNo,
		PaymentMethod:   lianlianpay.PayMethodAlipay,
		PaymentProvider: model.PaymentProviderLianlian,
		Status:          common.TopUpStatusPending,
	}).Error)

	body, signature := lianlianNotifyBodyFor(
		t,
		env,
		tradeNo,
		"10.00",
		setting.LianlianMchId,
		lianlianpay.PayTypeWechatNative,
	)
	recorder := postLianlianNotify(t, router, body, signature)

	assert.Equal(t, lianlianpay.FailResponse, recorder.Body.String())
	order := model.GetTopUpByTradeNo(tradeNo)
	require.NotNil(t, order)
	assert.Equal(t, common.TopUpStatusPending, order.Status)
}

func TestLianlianNotifyRejectsWhenWebhookDisabled(t *testing.T) {
	env := newLianlianTestEnv(t, lianlianTestCreatePayResponse)
	router := lianlianTestRouter(lianlianTestUserID)
	tradeNo := "USR77NODISABLED"
	require.NoError(t, env.db.Create(&model.TopUp{
		UserId:          lianlianTestUserID,
		Amount:          10,
		Money:           10,
		TradeNo:         tradeNo,
		PaymentMethod:   "alipay",
		PaymentProvider: model.PaymentProviderLianlian,
		Status:          common.TopUpStatusPending,
	}).Error)

	body, signature := lianlianNotifyBody(t, env, tradeNo, "10.00")
	setting.LianlianEnabled = false

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/lianlian/notify", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(lianlianpay.HeaderSignatureData, signature)
	router.ServeHTTP(recorder, request)
	assert.Equal(t, lianlianpay.FailResponse, recorder.Body.String())
}

func TestGetLianlianOrderOnlyReturnsOwnOrders(t *testing.T) {
	env := newLianlianTestEnv(t, lianlianTestCreatePayResponse)
	require.NoError(t, env.db.Create(&model.User{
		Id:       lianlianTestUserID + 1,
		Username: "other_lianlian_user",
		AffCode:  "other-lianlian-aff",
		Status:   common.UserStatusEnabled,
	}).Error)
	tradeNo := "USR77NOOTHER"
	require.NoError(t, env.db.Create(&model.TopUp{
		UserId:          lianlianTestUserID,
		Amount:          10,
		Money:           10,
		TradeNo:         tradeNo,
		PaymentMethod:   "alipay",
		PaymentProvider: model.PaymentProviderLianlian,
		Status:          common.TopUpStatusPending,
	}).Error)

	get := func(userId int) string {
		recorder := httptest.NewRecorder()
		router := lianlianTestRouter(userId)
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/lianlian/order/"+tradeNo, nil))
		return recorder.Body.String()
	}

	assert.Contains(t, get(lianlianTestUserID), `"message":"success"`)
	assert.Contains(t, get(lianlianTestUserID+1), "订单不存在")
}

func TestRequestLianlianPaySkipsWalletCapacityThatWouldOverflow(t *testing.T) {
	env := newLianlianTestEnv(t, lianlianTestCreatePayResponse)
	router := lianlianTestRouter(lianlianTestUserID)
	require.NoError(t, env.db.Model(&model.User{}).
		Where("id = ?", lianlianTestUserID).
		Update("quota", common.MaxWalletQuota-100_000).Error)

	recorder := lianlianTestPost(t, router, "/lianlian/pay", `{"amount":10,"payment_method":"alipay"}`)
	assert.Contains(t, recorder.Body.String(), "top-up quota limit exceeded")
	assert.Zero(t, env.requests)
}

func TestUpdateLianlianProviderTradeNoDoesNotOverwriteSettlementStatus(t *testing.T) {
	env := newLianlianTestEnv(t, lianlianTestCreatePayResponse)
	tradeNo := "USR77NOSETTLEMENTRACE"
	require.NoError(t, env.db.Create(&model.TopUp{
		UserId:          lianlianTestUserID,
		Amount:          10,
		Money:           10,
		TradeNo:         tradeNo,
		PaymentMethod:   lianlianpay.PayMethodAlipay,
		PaymentProvider: model.PaymentProviderLianlian,
		Status:          common.TopUpStatusSuccess,
	}).Error)

	require.NoError(t, model.UpdateTopUpProviderTradeNo(tradeNo, "LLRACE1"))
	order := model.GetTopUpByTradeNo(tradeNo)
	require.NotNil(t, order)
	assert.Equal(t, common.TopUpStatusSuccess, order.Status)
	assert.Equal(t, "LLRACE1", order.ProviderTradeNo)
	assert.ErrorIs(t, model.UpdateTopUpProviderTradeNo(tradeNo, "LLRACE2"), model.ErrLianlianProviderTradeNoMismatch)
}

func TestReconcileLianlianTopUpsSettlesPaidPendingOrder(t *testing.T) {
	env := newLianlianTestEnv(t, lianlianTestCreatePayResponse)
	tradeNo := "USR77NORECONCILE"
	env.queryResponse = fmt.Sprintf(
		`{"ret_code":"0000","ret_msg":"成功","mch_id":%q,"pay_type":"ALIPAY_NATIVE","txn_seqno":%q,"platform_txno":"LLRECON1","order_amount":"10.00","txn_status":"SUCCESS","pay_method_infos":[{"pay_type":"ALIPAY_NATIVE","amount":"10.00"}]}`,
		setting.LianlianMchId,
		tradeNo,
	)
	require.NoError(t, env.db.Create(&model.TopUp{
		UserId:          lianlianTestUserID,
		Amount:          10,
		Money:           10,
		TradeNo:         tradeNo,
		PaymentMethod:   lianlianpay.PayMethodAlipay,
		PaymentProvider: model.PaymentProviderLianlian,
		CreateTime:      time.Now().Add(-10 * time.Minute).Unix(),
		Status:          common.TopUpStatusPending,
	}).Error)

	summary, err := ReconcileLianlianTopUpsOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Scanned)
	assert.Equal(t, 1, summary.Settled)
	assert.Equal(t, 0, summary.Failed)
	assert.Equal(t, "/query"+lianlianpay.OrderQueryPath, env.lastPath)

	order := model.GetTopUpByTradeNo(tradeNo)
	require.NotNil(t, order)
	assert.Equal(t, common.TopUpStatusSuccess, order.Status)
	assert.Equal(t, "LLRECON1", order.ProviderTradeNo)
	user, err := model.GetUserById(lianlianTestUserID, false)
	require.NoError(t, err)
	assert.Equal(t, 1000+5000000, user.Quota)
}
