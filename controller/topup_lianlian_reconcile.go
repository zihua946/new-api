package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service/lianlianpay"
	"github.com/QuantumNous/new-api/setting"
)

const (
	lianlianReconcileMinAge    = 2 * time.Minute
	lianlianReconcileLookback  = 24 * time.Hour
	lianlianReconcileBatchSize = 50
)

// LianlianReconcileSummary describes one compensation pass.
type LianlianReconcileSummary struct {
	Scanned int `json:"scanned"`
	Settled int `json:"settled"`
	Skipped int `json:"skipped"`
	Failed  int `json:"failed"`
}

// ReconcileLianlianTopUpsOnce queries Lianlian for pending orders that are old
// enough that a callback should already have arrived. Query results go through
// the same amount, payment-method and idempotent settlement path as callbacks.
func ReconcileLianlianTopUpsOnce(ctx context.Context) (LianlianReconcileSummary, error) {
	summary := LianlianReconcileSummary{}
	if !isLianlianTopUpEnabled() {
		return summary, nil
	}

	now := time.Now()
	topUps, err := model.FindPendingTopUpsByProvider(
		model.PaymentProviderLianlian,
		now.Add(-lianlianReconcileMinAge).Unix(),
		now.Add(-lianlianReconcileLookback).Unix(),
		lianlianReconcileBatchSize,
	)
	if err != nil {
		return summary, err
	}
	summary.Scanned = len(topUps)

	for i := range topUps {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		topUp := &topUps[i]
		result, queryErr := queryLianlianOrder(ctx, topUp.TradeNo)
		if queryErr != nil {
			summary.Failed++
			logger.LogWarn(ctx, fmt.Sprintf("连连 订单查询失败 trade_no=%s error=%q", topUp.TradeNo, queryErr.Error()))
			continue
		}
		if strings.TrimSpace(result.MchID) != strings.TrimSpace(setting.LianlianMchId) {
			summary.Failed++
			logger.LogWarn(ctx, fmt.Sprintf("连连 查询商户号不匹配 trade_no=%s mch_id=%s", topUp.TradeNo, result.MchID))
			continue
		}
		if strings.TrimSpace(result.TxnSeqno) != topUp.TradeNo {
			summary.Failed++
			logger.LogWarn(ctx, fmt.Sprintf("连连 查询订单号不匹配 trade_no=%s query_txn_seqno=%s", topUp.TradeNo, result.TxnSeqno))
			continue
		}
		if strings.TrimSpace(result.OrderAmount) == "" {
			summary.Failed++
			logger.LogWarn(ctx, fmt.Sprintf("连连 查询缺少订单金额 trade_no=%s", topUp.TradeNo))
			continue
		}
		status := strings.ToUpper(strings.TrimSpace(result.TxnStatus))
		if status == "" {
			summary.Skipped++
			continue
		}
		if !lianlianpay.IsPaidStatus(status) {
			summary.Skipped++
			continue
		}
		paymentMethod := lianlianpay.PayMethodForPayType(result.PayTypeValue())
		if paymentMethod == "" {
			summary.Failed++
			logger.LogWarn(ctx, fmt.Sprintf("连连 查询支付方式无效 trade_no=%s pay_type=%s", topUp.TradeNo, result.PayTypeValue()))
			continue
		}

		alreadyDone, settleErr := model.RechargeLianlian(
			topUp.TradeNo,
			paymentMethod,
			"lianlian-reconcile",
			result.OrderAmount,
			result.PlatformTxNo,
		)
		if settleErr != nil {
			summary.Failed++
			logger.LogError(ctx, fmt.Sprintf("连连 查询补偿入账失败 trade_no=%s error=%q", topUp.TradeNo, settleErr.Error()))
			continue
		}
		if alreadyDone {
			summary.Skipped++
			continue
		}
		summary.Settled++
		logger.LogInfo(ctx, fmt.Sprintf("连连 查询补偿入账成功 trade_no=%s", topUp.TradeNo))
	}

	return summary, nil
}

func queryLianlianOrder(ctx context.Context, tradeNo string) (*lianlianpay.OrderQueryResult, error) {
	signed, err := lianlianpay.BuildOrderQueryRequest(lianlianpay.OrderQueryRequest{
		Env:        setting.LianlianEnv,
		Gateway:    setting.LianlianGateway,
		PrivateKey: setting.LianlianPrivateKey,
		MerchantID: setting.LianlianMchId,
		TradeNo:    tradeNo,
	})
	if err != nil {
		return nil, err
	}
	body, err := postSignedLianlianRequest(ctx, signed)
	if err != nil {
		return nil, err
	}
	return lianlianpay.ParseOrderQueryResponse(body)
}
