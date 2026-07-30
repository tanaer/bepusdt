package model

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestClaimPaymentConfirmationPreventsHashReuseAndIsIdempotent(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	first := newPaymentClaimTestOrder("payment-claim-first", now)
	second := newPaymentClaimTestOrder("payment-claim-second", now)
	if err := Db.Create(&first).Error; err != nil {
		t.Fatalf("create first order: %v", err)
	}
	if err := Db.Create(&second).Error; err != nil {
		t.Fatalf("create second order: %v", err)
	}

	payment := PaymentConfirmation{
		BlockNum: 100,
		From:     "TJ5usJLLwjwn7Pw3TPbdzreG7dvgKzfQ5y",
		Hash:     "303245E65DA6DCA3E7AED8DC5386CD0CBBC7F67E4C21FF4EE0D9330055A41983",
		At:       now.Add(time.Second),
		Amount:   decimal.RequireFromString("65.94"),
	}

	already, err := ClaimPaymentConfirmation(&first, payment)
	if err != nil || already {
		t.Fatalf("first claim = already:%t err:%v, want new successful claim", already, err)
	}
	if first.Status != OrderStatusConfirming {
		t.Fatalf("first order status = %d, want confirming", first.Status)
	}

	already, err = ClaimPaymentConfirmation(&first, payment)
	if err != nil || !already {
		t.Fatalf("same order repeat = already:%t err:%v, want idempotent success", already, err)
	}

	if _, err := ClaimPaymentConfirmation(&second, payment); !errors.Is(err, ErrPaymentHashAlreadyClaimed) {
		t.Fatalf("claiming same hash for second order error = %v, want ErrPaymentHashAlreadyClaimed", err)
	}
}

func TestClaimPaymentConfirmationProtectsLegacyOrderHash(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	legacy := newPaymentClaimTestOrder("legacy-payment-claim", now)
	legacy.Status = OrderStatusConfirming
	legacy.RefHash = "303245e65da6dca3e7aed8dc5386cd0cbbc7f67e4c21ff4ee0d9330055a41983"
	if err := Db.Create(&legacy).Error; err != nil {
		t.Fatalf("create legacy order: %v", err)
	}

	newOrder := newPaymentClaimTestOrder("new-payment-claim", now)
	if err := Db.Create(&newOrder).Error; err != nil {
		t.Fatalf("create new order: %v", err)
	}
	_, err := ClaimPaymentConfirmation(&newOrder, PaymentConfirmation{
		BlockNum: 101,
		From:     "TJ5usJLLwjwn7Pw3TPbdzreG7dvgKzfQ5y",
		Hash:     legacy.RefHash,
		At:       now.Add(time.Second),
		Amount:   decimal.RequireFromString("65.94"),
	})
	if !errors.Is(err, ErrPaymentHashAlreadyClaimed) {
		t.Fatalf("legacy hash reuse error = %v, want ErrPaymentHashAlreadyClaimed", err)
	}
}

func TestClaimPaymentConfirmationPreservesCaseSensitiveSolanaSignature(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	order := newPaymentClaimTestOrder("solana-payment-claim", now)
	order.TradeType = UsdtSolana
	if err := Db.Create(&order).Error; err != nil {
		t.Fatalf("create order: %v", err)
	}

	const signature = "5GTnZsNBcpPxBb2fEA5u2SwaNVry97ouHsa5oFKKVNEt4Da8YzakDTky6byNLGZDQdLrx9sDXeNr7XYDgNDiSJJQ"
	_, err := ClaimPaymentConfirmation(&order, PaymentConfirmation{
		BlockNum: 435577393,
		From:     "H1NwZnujLy6q2q9Mj813xCMSzkmYE6p6PzyC2zswJSmK",
		Hash:     signature,
		At:       time.Unix(1785171892, 0),
		Amount:   decimal.RequireFromString("1.32"),
	})
	if err != nil {
		t.Fatalf("claim Solana payment: %v", err)
	}
	if order.RefHash != signature {
		t.Fatalf("stored Solana signature = %q, want original %q", order.RefHash, signature)
	}
	var claim PaymentHashClaim
	if err := Db.Where("order_id = ?", order.ID).First(&claim).Error; err != nil {
		t.Fatalf("load Solana payment claim: %v", err)
	}
	if claim.Hash != signature {
		t.Fatalf("Solana claim key = %q, want case-sensitive signature %q", claim.Hash, signature)
	}
}

func newPaymentClaimTestOrder(tradeID string, now time.Time) Order {
	return Order{
		OrderId:      tradeID,
		TradeId:      tradeID,
		TradeType:    UsdtTrc20,
		Fiat:         CNY,
		Crypto:       USDT,
		Rate:         "7.00",
		Amount:       "65.94",
		Money:        "461.58",
		Address:      "TKNUJShSXfCDii9bxdguPwgZsKXG6rBLC6",
		MatchAddress: "TKNUJShSXfCDii9bxdguPwgZsKXG6rBLC6",
		Status:       OrderStatusWaiting,
		ApiType:      OrderApiTypeEpusdt,
		ExpiredAt:    now.Add(10 * time.Minute),
		ConfirmedAt:  &now,
		AutoTimeAt:   AutoTimeAt{CreatedAt: (*Datetime)(&now), UpdatedAt: (*Datetime)(&now)},
	}
}
