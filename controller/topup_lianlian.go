package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/lianlianpay"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

// lianlianOrderTTLSeconds 是扫码订单的有效期（秒），超时后前端停止轮询。
const lianlianOrderTTLSeconds int64 = lianlianpay.PayExpireMinutes * 60

// lianlianResponseBodyLimit 限制读取连连应答的大小，避免异常应答拖垮进程。
const lianlianResponseBodyLimit = 64 * 1024

type LianlianPayRequest struct {
	// Amount 是充值面值，保持既有整数美元/元或 TOKENS 口径。
	Amount        int64  `json:"amount"`
	PaymentMethod string `json:"payment_method"`
}

// lianlianHTTPClient 是调用连连下单接口的客户端，超时时间与连连建议一致。
var lianlianHTTPClient = &http.Client{Timeout: lianlianpay.TimeoutSeconds * time.Second}

// RequestLianlianAmount 返回本次充值需要支付的金额（元，两位小数），供前端确认展示。
func RequestLianlianAmount(c *gin.Context) {
	if !isLianlianTopUpEnabled() {
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "充值服务未启用"})
		return
	}
	var req LianlianPayRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "参数错误"})
		return
	}
	if !lianlianpay.SupportsPayMethod(req.PaymentMethod) {
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "支付方式不存在"})
		return
	}
	if req.Amount < getMinTopup() {
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": fmt.Sprintf("充值数量不能小于 %d", getMinTopup())})
		return
	}

	id := c.GetInt("id")
	group, err := model.GetUserGroup(id, true)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "获取用户分组失败"})
		return
	}
	if rejectInvalidTopUpQuota(c, id, req.Amount) {
		return
	}
	payMoney := getPayMoney(req.Amount, group)
	if payMoney < 0.01 {
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "充值金额过低"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "success", "data": decimal.NewFromFloat(payMoney).StringFixed(2)})
}

// RequestLianlianPay 创建本地订单并向连连下单，返回扫码串供前端渲染二维码。
func RequestLianlianPay(c *gin.Context) {
	if !isLianlianTopUpEnabled() {
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "充值服务未启用"})
		return
	}
	// 网关地址是管理员配置项：先校验再建单，避免留下注定失败的订单，
	// 具体原因只写入服务端日志，前端统一返回中性提示。
	if _, err := lianlianpay.GatewayURL(setting.LianlianEnv, setting.LianlianGateway); err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("连连 网关地址配置错误 user_id=%d gateway=%q error=%q", c.GetInt("id"), setting.LianlianGateway, err.Error()))
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "支付服务暂时不可用"})
		return
	}
	var req LianlianPayRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "参数错误"})
		return
	}
	if !lianlianpay.SupportsPayMethod(req.PaymentMethod) {
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "支付方式不存在"})
		return
	}
	if req.Amount < getMinTopup() {
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": fmt.Sprintf("充值数量不能小于 %d", getMinTopup())})
		return
	}

	id := c.GetInt("id")
	user, err := model.GetUserById(id, false)
	if err != nil || user == nil {
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "用户不存在"})
		return
	}
	group, err := model.GetUserGroup(id, true)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "获取用户分组失败"})
		return
	}

	if rejectInvalidTopUpQuota(c, id, req.Amount) {
		return
	}
	payMoney := getPayMoney(req.Amount, group)
	if payMoney < 0.01 {
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "充值金额过低"})
		return
	}
	money := decimal.NewFromFloat(payMoney).Round(2).StringFixed(2)

	tradeNo := fmt.Sprintf("USR%dNO%s%d", id, common.GetRandomString(6), time.Now().Unix())
	if len(tradeNo) > lianlianpay.CreatePayTradeNoLimit {
		tradeNo = tradeNo[:lianlianpay.CreatePayTradeNoLimit]
	}

	topUp := &model.TopUp{
		UserId:          id,
		Amount:          normalizeLianlianTopUpAmount(req.Amount),
		Money:           payMoney,
		TradeNo:         tradeNo,
		PaymentMethod:   req.PaymentMethod,
		PaymentProvider: model.PaymentProviderLianlian,
		CreateTime:      time.Now().Unix(),
		Status:          common.TopUpStatusPending,
	}
	if err := topUp.Insert(); err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("连连 创建充值订单失败 user_id=%d trade_no=%s amount=%d error=%q", id, tradeNo, req.Amount, err.Error()))
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "创建订单失败"})
		return
	}

	signed, err := lianlianpay.BuildCreatePayRequest(lianlianpay.CreatePayRequest{
		Env:        setting.LianlianEnv,
		Gateway:    setting.LianlianGateway,
		PrivateKey: setting.LianlianPrivateKey,
		MerchantID: setting.LianlianMchId,
		BusiType:   setting.LianlianBusiType,
		UserID:     strconv.Itoa(id),
		TradeNo:    tradeNo,
		Money:      money,
		GoodsName:  fmt.Sprintf("NewAPI 充值 %s", money),
		NotifyURL:  service.GetCallbackAddress() + "/api/user/lianlian/notify",
		ClientIP:   c.ClientIP(),
		PayMethod:  req.PaymentMethod,
		Timestamp:  lianlianpay.NowTimestamp(),
	})
	if err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("连连 构造下单报文失败 user_id=%d trade_no=%s error=%q", id, tradeNo, err.Error()))
		markTopUpFailed(topUp)
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "拉起支付失败"})
		return
	}

	result, err := createLianlianPayment(c, signed)
	if err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("连连 拉起支付失败 user_id=%d trade_no=%s payment_method=%s amount=%d error=%q", id, tradeNo, req.PaymentMethod, req.Amount, err.Error()))
		markTopUpFailed(topUp)
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "拉起支付失败"})
		return
	}

	providerTradeNo := strings.TrimSpace(result.PlatformTxNo)
	if len(providerTradeNo) > 128 {
		logger.LogWarn(c.Request.Context(), fmt.Sprintf("连连 上游订单号超长已忽略 trade_no=%s provider_trade_no=%q", tradeNo, providerTradeNo))
		providerTradeNo = ""
	}
	if providerTradeNo != "" {
		if err := model.UpdateTopUpProviderTradeNo(tradeNo, providerTradeNo); err != nil {
			logger.LogError(c.Request.Context(), fmt.Sprintf("连连 保存上游订单号失败 trade_no=%s provider_trade_no=%s error=%q", tradeNo, providerTradeNo, err.Error()))
		}
	}

	logger.LogInfo(c.Request.Context(), fmt.Sprintf("连连 充值订单创建成功 user_id=%d trade_no=%s provider_trade_no=%s payment_method=%s amount=%d money=%.2f", id, tradeNo, providerTradeNo, req.PaymentMethod, topUp.Amount, payMoney))
	c.JSON(http.StatusOK, gin.H{
		"message": "success",
		"data": gin.H{
			"trade_no":          tradeNo,
			"provider_trade_no": providerTradeNo,
			"qr_code":           result.QRCode,
			"expires_at":        time.Now().Unix() + lianlianOrderTTLSeconds,
			"money":             payMoney,
		},
	})
}

// normalizeLianlianTopUpAmount 沿用其他充值渠道的历史口径：计价模式保存面值，
// TOKENS 模式折算为整数美元面值。
func normalizeLianlianTopUpAmount(amount int64) int64 {
	if operation_setting.GetQuotaDisplayType() != operation_setting.QuotaDisplayTypeTokens {
		return amount
	}
	return decimal.NewFromInt(amount).Div(decimal.NewFromFloat(common.QuotaPerUnit)).IntPart()
}

// createLianlianPayment 发送下单请求并解析应答。
func createLianlianPayment(c *gin.Context, signed *lianlianpay.SignedRequest) (*lianlianpay.CreatePayResult, error) {
	body, err := postSignedLianlianRequest(c.Request.Context(), signed)
	if err != nil {
		return nil, err
	}
	return lianlianpay.ParseCreatePayResponse(body)
}

func postSignedLianlianRequest(ctx context.Context, signed *lianlianpay.SignedRequest) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, signed.URL, bytes.NewReader(signed.Body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", lianlianpay.HeaderContentTypeValue)
	request.Header.Set(lianlianpay.HeaderMchID, signed.MerchantID)
	request.Header.Set(lianlianpay.HeaderSignatureType, lianlianpay.SignTypeRSA)
	request.Header.Set(lianlianpay.HeaderSignatureData, signed.Signature)
	request.Header.Set(lianlianpay.HeaderTimestamp, signed.Timestamp)

	response, err := lianlianHTTPClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = response.Body.Close()
	}()

	body, err := io.ReadAll(io.LimitReader(response.Body, lianlianResponseBodyLimit))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("连连接口返回 HTTP %d: %s", response.StatusCode, common.LocalLogPreview(string(body)))
	}
	return body, nil
}

// markTopUpFailed 把下单失败的订单标记为失败，避免留下永远 pending 的订单。
func markTopUpFailed(topUp *model.TopUp) {
	topUp.Status = common.TopUpStatusFailed
	if err := topUp.Update(); err != nil {
		common.SysError("failed to mark lianlian topup as failed: " + err.Error())
	}
}

// GetLianlianOrder 返回当前用户的订单状态，供前端轮询支付结果。
func GetLianlianOrder(c *gin.Context) {
	tradeNo := c.Param("tradeNo")
	if tradeNo == "" {
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "参数错误"})
		return
	}
	topUp := model.GetTopUpByTradeNo(tradeNo)
	if topUp == nil || topUp.PaymentProvider != model.PaymentProviderLianlian {
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "订单不存在"})
		return
	}
	userId := c.GetInt("id")
	if topUp.UserId != userId && topUp.OperatorUserId != userId {
		// 不暴露他人订单的存在性细节，统一按订单不存在处理。
		logger.LogWarn(c.Request.Context(), fmt.Sprintf("连连 订单查询越权拒绝 user_id=%d trade_no=%s client_ip=%s", userId, tradeNo, c.ClientIP()))
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "订单不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"message": "success",
		"data": gin.H{
			"trade_no":          topUp.TradeNo,
			"provider_trade_no": topUp.ProviderTradeNo,
			"status":            topUp.Status,
			"money":             topUp.Money,
			"amount":            topUp.Amount,
			"create_time":       topUp.CreateTime,
		},
	})
}

// LianlianNotify 处理连连异步支付通知：验签 → 校验订单与金额 → 入账 → 返回 Success。
func LianlianNotify(c *gin.Context) {
	ctx := c.Request.Context()
	if !isLianlianWebhookEnabled() {
		logger.LogWarn(ctx, fmt.Sprintf("连连 webhook 被拒绝 reason=webhook_disabled path=%q client_ip=%s", c.Request.URL.Path, c.ClientIP()))
		writeLianlianNotifyResponse(c, lianlianpay.FailResponse)
		return
	}

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		logger.LogError(ctx, fmt.Sprintf("连连 webhook 读取请求体失败 path=%q client_ip=%s error=%q", c.Request.URL.Path, c.ClientIP(), err.Error()))
		writeLianlianNotifyResponse(c, lianlianpay.FailResponse)
		return
	}
	if len(body) == 0 {
		logger.LogWarn(ctx, fmt.Sprintf("连连 webhook 请求体为空 path=%q client_ip=%s", c.Request.URL.Path, c.ClientIP()))
		writeLianlianNotifyResponse(c, lianlianpay.FailResponse)
		return
	}

	signature := c.GetHeader(lianlianpay.HeaderSignatureData)
	verified, verifyErr := lianlianpay.Verify(setting.GetLianlianPlatformPublicKey(), body, signature)
	if verifyErr != nil {
		logger.LogError(ctx, fmt.Sprintf("连连 webhook 验签异常 path=%q client_ip=%s error=%q", c.Request.URL.Path, c.ClientIP(), verifyErr.Error()))
		writeLianlianNotifyResponse(c, lianlianpay.FailResponse)
		return
	}
	if !verified {
		logger.LogWarn(ctx, fmt.Sprintf("连连 webhook 验签失败 path=%q client_ip=%s", c.Request.URL.Path, c.ClientIP()))
		writeLianlianNotifyResponse(c, lianlianpay.FailResponse)
		return
	}

	notify, err := lianlianpay.ParseNotifyPayload(body)
	if err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("连连 webhook 报文解析失败 path=%q client_ip=%s error=%q", c.Request.URL.Path, c.ClientIP(), err.Error()))
		writeLianlianNotifyResponse(c, lianlianpay.FailResponse)
		return
	}
	if strings.TrimSpace(notify.MchID) != strings.TrimSpace(setting.LianlianMchId) {
		logger.LogWarn(ctx, fmt.Sprintf("连连 webhook 商户号不匹配 mch_id=%s client_ip=%s", notify.MchID, c.ClientIP()))
		writeLianlianNotifyResponse(c, lianlianpay.FailResponse)
		return
	}
	actualPaymentMethod := lianlianpay.PayMethodForPayType(notify.PayTypeValue())
	if actualPaymentMethod == "" {
		logger.LogWarn(ctx, fmt.Sprintf("连连 webhook 支付方式无效 pay_type=%s trade_no=%s client_ip=%s", notify.PayTypeValue(), notify.MerchantTradeNo(), c.ClientIP()))
		writeLianlianNotifyResponse(c, lianlianpay.FailResponse)
		return
	}
	tradeNo := notify.MerchantTradeNo()
	logger.LogInfo(ctx, fmt.Sprintf("连连 webhook 验签成功 trade_no=%s provider_trade_no=%s result=%s client_ip=%s", tradeNo, notify.ProviderTradeNo(), notify.PayStatusValue(), c.ClientIP()))

	if !lianlianpay.IsPaidStatus(notify.PayStatusValue()) {
		logger.LogInfo(ctx, fmt.Sprintf("连连 webhook 忽略非成功状态 trade_no=%s result=%s client_ip=%s", tradeNo, notify.PayStatusValue(), c.ClientIP()))
		writeLianlianNotifyResponse(c, lianlianpay.SuccessResponse)
		return
	}

	// 进程内锁只是优化；重复/并发回调的正确性由 RechargeLianlian 的
	// 数据库行锁 + 事务内状态校验保证（多实例部署下同样安全）。
	LockOrder(tradeNo)
	defer UnlockOrder(tradeNo)
	alreadyDone, err := model.RechargeLianlian(
		tradeNo,
		actualPaymentMethod,
		c.ClientIP(),
		notify.PaidAmount(),
		notify.ProviderTradeNo(),
	)
	if err != nil {
		switch {
		case errors.Is(err, model.ErrTopUpNotFound):
			logger.LogWarn(ctx, fmt.Sprintf("连连 回调订单不存在 trade_no=%s client_ip=%s", tradeNo, c.ClientIP()))
		case errors.Is(err, model.ErrPaymentMethodMismatch):
			logger.LogWarn(ctx, fmt.Sprintf("连连 订单支付方式不匹配 trade_no=%s client_ip=%s", tradeNo, c.ClientIP()))
		case errors.Is(err, model.ErrTopUpStatusInvalid):
			logger.LogWarn(ctx, fmt.Sprintf("连连 订单状态非法 trade_no=%s client_ip=%s", tradeNo, c.ClientIP()))
		case errors.Is(err, model.ErrLianlianAmountMismatch):
			logger.LogError(ctx, fmt.Sprintf("连连 回调金额与订单不一致 trade_no=%s paid=%s client_ip=%s", tradeNo, notify.PaidAmount(), c.ClientIP()))
		case errors.Is(err, model.ErrLianlianProviderTradeNoMismatch):
			logger.LogWarn(ctx, fmt.Sprintf("连连 上游订单号不一致 trade_no=%s provider_trade_no=%s client_ip=%s", tradeNo, notify.ProviderTradeNo(), c.ClientIP()))
		default:
			logger.LogError(ctx, fmt.Sprintf("连连 充值处理失败 trade_no=%s client_ip=%s error=%q", tradeNo, c.ClientIP(), err.Error()))
		}
		writeLianlianNotifyResponse(c, lianlianpay.FailResponse)
		return
	}
	if alreadyDone {
		logger.LogInfo(ctx, fmt.Sprintf("连连 重复回调幂等忽略 trade_no=%s client_ip=%s", tradeNo, c.ClientIP()))
	} else {
		logger.LogInfo(ctx, fmt.Sprintf("连连 充值成功 trade_no=%s client_ip=%s", tradeNo, c.ClientIP()))
	}
	writeLianlianNotifyResponse(c, lianlianpay.SuccessResponse)
}

// writeLianlianNotifyResponse 按连连要求返回纯文本结果。
func writeLianlianNotifyResponse(c *gin.Context, text string) {
	c.String(http.StatusOK, text)
}

// lianlianPayMethods 返回钱包页展示的连连支付方式（支付宝/微信扫码）。
func lianlianPayMethods() []map[string]string {
	return []map[string]string{
		{"name": "支付宝", "type": lianlianpay.PayMethodAlipay, "color": "#1677FF"},
		{"name": "微信", "type": lianlianpay.PayMethodWxpay, "color": "#07C160"},
	}
}
