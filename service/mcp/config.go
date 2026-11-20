package mcp

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/shopspring/decimal"
)

const MaxResultBytes = 4 << 20
const MaxArgumentBytes = 1 << 20

type Error struct {
	Status     int    `json:"-"`
	Code       string `json:"code"`
	Message    string `json:"message"`
	quotaClamp *common.QuotaClamp
}

func (e *Error) Error() string { return e.Message }

func Failure(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

func Enabled() bool { return os.Getenv("MCP_ENABLED") == "true" }

func Digest(value string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(value))) }

// AEAD associated data binds ciphertext to its server, invocation or login flow.
// A deployment must supply a durable key; a process-random fallback would make
// credentials and paid result replays unreadable after restart.
func encryptionKey() (cipher.AEAD, error) {
	key, err := base64.StdEncoding.DecodeString(os.Getenv("MCP_ENCRYPTION_KEY"))
	if err != nil || len(key) != 32 {
		return nil, Failure(503, "MCP_NOT_CONFIGURED", "MCP encryption key is not configured")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func Seal(value, associatedData string) (string, error) {
	aead, err := encryptionKey()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nonce, nonce, []byte(value), []byte(associatedData))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func Open(value, associatedData string) (string, error) {
	aead, err := encryptionKey()
	if err != nil {
		return "", err
	}
	sealed, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(sealed) < aead.NonceSize() {
		return "", Failure(503, "CREDENTIAL_UNAVAILABLE", "MCP credential is unavailable")
	}
	plain, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], []byte(associatedData))
	if err != nil {
		return "", Failure(503, "CREDENTIAL_UNAVAILABLE", "MCP credential is unavailable")
	}
	return string(plain), nil
}

type PriceQuote struct {
	UnitPrice        string  `json:"unit_price"`
	Currency         string  `json:"currency"`
	ExchangeRate     string  `json:"exchange_rate"`
	Mode             string  `json:"mode"`
	UnitPriceUSD     string  `json:"unit_price_usd"`
	ChargeQuota      int     `json:"charge_quota"`
	EffectiveCredits string  `json:"effective_credits"`
	QuotaPerUnit     float64 `json:"quota_per_unit"`
	PriceRevision    int64   `json:"price_revision"`
}

var pricePattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,9})(\.[0-9]{1,12})?$`)

func toolQuote(tool *model.MCPTool) (*PriceQuote, error) {
	if tool.Currency == "" {
		return QuotePrice(tool.PriceMode, tool.UnitPriceUSD)
	}
	return QuotePrice(tool.PriceMode, tool.UnitPrice, tool.Currency)
}

func QuotePrice(mode, price string, currencies ...string) (*PriceQuote, error) {
	invalid := Failure(http.StatusUnprocessableEntity, "INVALID_PRICE", "Price must be non-negative, representable and within the per-call quota limit")
	if !pricePattern.MatchString(price) || math.IsNaN(common.QuotaPerUnit) || math.IsInf(common.QuotaPerUnit, 0) || common.QuotaPerUnit <= 0 {
		return nil, invalid
	}
	currency := "USD"
	if len(currencies) > 0 {
		currency = currencies[0]
	}
	rate := operation_setting.USDExchangeRate
	if currency != "USD" && currency != "CNY" || rate <= 0 || math.IsInf(rate, 0) || math.IsNaN(rate) {
		return nil, invalid
	}
	original, err := decimal.NewFromString(price)
	usd := original
	if err != nil || usd.IsNegative() {
		return nil, invalid
	}
	if mode != "free" && mode != "per_success" || mode == "free" && !usd.IsZero() {
		return nil, invalid
	}
	unit := decimal.NewFromFloat(common.QuotaPerUnit)
	divisor := decimal.NewFromInt(1)
	if currency == "CNY" {
		divisor = decimal.NewFromFloat(rate)
		usd = original.DivRound(divisor, 16)
	}
	numerator := original.Mul(unit)
	if original.IsPositive() && numerator.LessThan(divisor) {
		return nil, invalid
	}
	quota, clamp := common.QuotaFromDecimalChecked(numerator.DivRound(divisor, 0))
	if clamp != nil || quota < 0 {
		invalid.quotaClamp = clamp
		return nil, invalid
	}
	return &PriceQuote{UnitPrice: original.String(), Currency: currency, ExchangeRate: decimal.NewFromFloat(rate).String(), Mode: mode, UnitPriceUSD: usd.String(), ChargeQuota: quota, QuotaPerUnit: common.QuotaPerUnit,
		EffectiveCredits: decimal.NewFromInt(int64(quota)).Div(unit).String()}, nil
}

func PublicBaseURL() string { return strings.TrimRight(os.Getenv("MCP_PUBLIC_BASE_URL"), "/") }
