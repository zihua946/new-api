package model

import (
	"errors"
)

// TryReserveUserQuota synchronously persists the wallet reservation. Redis
// failures fail closed; an insufficient balance never creates an upstream call.
func TryReserveUserQuota(id int, quota int) (bool, error) {
	if quota < 0 {
		return false, errors.New("quota 不能为负数！")
	}
	err := runQuotaOperation(id, 0, -quota, true)
	if errors.Is(err, ErrQuotaWalletInsufficient) {
		return false, nil
	}
	return err == nil, err
}

// The current SQL token decides unlimited status, so stale authentication cache
// cannot bypass a subsequently imposed token limit.
func TryReserveTokenQuota(id int, key string, quota int, unlimited bool) (bool, error) {
	if quota < 0 {
		return false, errors.New("quota 不能为负数！")
	}
	err := runQuotaOperation(0, id, -quota, true)
	if errors.Is(err, ErrQuotaTokenInsufficient) {
		return false, nil
	}
	return err == nil, err
}
