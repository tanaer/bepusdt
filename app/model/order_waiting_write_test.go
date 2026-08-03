package model

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestRebuildOrderRejectsNonWaitingOrder(t *testing.T) {
	now := time.Now().UTC()
	order := newWaitingWriteTestOrder("rebuild-non-waiting", now)
	order.Status = OrderStatusConfirming
	order.RefHash = "confirmed-transaction"

	_, err := RebuildOrder(order, OrderParams{
		Money:     decimal.RequireFromString(order.Money),
		OrderId:   order.OrderId,
		TradeType: order.TradeType,
		Fiat:      order.Fiat,
	})
	if !errors.Is(err, ErrOrderNoLongerReceivable) {
		t.Fatalf("rebuild confirming order error = %v, want ErrOrderNoLongerReceivable", err)
	}
}

func TestApplyRebuiltWaitingOrderDoesNotOverwriteConfirmedPayment(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	confirmedAt := now.Add(-time.Second)
	stored := newWaitingWriteTestOrder("stale-rebuild", now)
	stored.Status = OrderStatusConfirming
	stored.RefHash = "confirmed-transaction"
	stored.FromAddress = "confirmed-sender"
	stored.ConfirmedAt = &confirmedAt
	stored.RefBlockNum = 98765
	if err := Db.Create(&stored).Error; err != nil {
		t.Fatalf("create confirmed order: %v", err)
	}

	stale := stored
	stale.Status = OrderStatusWaiting
	stale.RefHash = ""
	stale.FromAddress = ""
	stale.RefBlockNum = 0
	zero := time.Unix(0, 0)
	stale.ConfirmedAt = &zero
	err := applyRebuiltWaitingOrder(&stale, OrderParams{
		Money:             decimal.RequireFromString("500.00"),
		OrderId:           stored.OrderId,
		TradeType:         UsdtBep20,
		Fiat:              USD,
		Timeout:           600,
		TradeTypeReselect: true,
	}, Trade{
		Wallet: Wallet{MatchAddr: "new-payment-address", TradeType: string(UsdtBep20)},
		Crypto: USDT,
		Rate:   decimal.RequireFromString("6.25"),
		Amount: "80.00",
	})
	if !errors.Is(err, ErrOrderNoLongerReceivable) {
		t.Fatalf("apply stale rebuild error = %v, want ErrOrderNoLongerReceivable", err)
	}

	var refreshed Order
	if err := Db.First(&refreshed, stored.ID).Error; err != nil {
		t.Fatalf("reload order: %v", err)
	}
	assertConfirmedPaymentFields(t, refreshed, confirmedAt)
}

func TestSetExpiredDoesNotOverwriteConfirmedPayment(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	confirmedAt := now.Add(-time.Second)
	stored := newWaitingWriteTestOrder("stale-expire", now)
	stored.Status = OrderStatusConfirming
	stored.RefHash = "confirmed-transaction"
	stored.FromAddress = "confirmed-sender"
	stored.ConfirmedAt = &confirmedAt
	stored.RefBlockNum = 98765
	if err := Db.Create(&stored).Error; err != nil {
		t.Fatalf("create confirmed order: %v", err)
	}

	stale := stored
	stale.Status = OrderStatusWaiting
	stale.RefHash = ""
	stale.FromAddress = ""
	stale.RefBlockNum = 0
	zero := time.Unix(0, 0)
	stale.ConfirmedAt = &zero
	if err := stale.SetExpired(); !errors.Is(err, ErrOrderNoLongerReceivable) {
		t.Fatalf("expire confirmed order error = %v, want ErrOrderNoLongerReceivable", err)
	}

	var refreshed Order
	if err := Db.First(&refreshed, stored.ID).Error; err != nil {
		t.Fatalf("reload order: %v", err)
	}
	assertConfirmedPaymentFields(t, refreshed, confirmedAt)
}

func TestSetCanceledDoesNotOverwriteConfirmedPayment(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	confirmedAt := now.Add(-time.Second)
	stored := newWaitingWriteTestOrder("stale-cancel", now)
	stored.Status = OrderStatusConfirming
	stored.RefHash = "confirmed-transaction"
	stored.FromAddress = "confirmed-sender"
	stored.ConfirmedAt = &confirmedAt
	stored.RefBlockNum = 98765
	if err := Db.Create(&stored).Error; err != nil {
		t.Fatalf("create confirmed order: %v", err)
	}

	stale := stored
	stale.Status = OrderStatusWaiting
	stale.RefHash = ""
	stale.FromAddress = ""
	stale.RefBlockNum = 0
	zero := time.Unix(0, 0)
	stale.ConfirmedAt = &zero
	if err := stale.SetCanceled(); !errors.Is(err, ErrOrderNoLongerReceivable) {
		t.Fatalf("cancel confirmed order error = %v, want ErrOrderNoLongerReceivable", err)
	}

	var refreshed Order
	if err := Db.First(&refreshed, stored.ID).Error; err != nil {
		t.Fatalf("reload order: %v", err)
	}
	assertConfirmedPaymentFields(t, refreshed, confirmedAt)
}

func TestSetCanceledTransitionsWaitingOrder(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	order := newWaitingWriteTestOrder("cancel-waiting", time.Now().UTC())
	if err := Db.Create(&order).Error; err != nil {
		t.Fatalf("create waiting order: %v", err)
	}

	if err := order.SetCanceled(); err != nil {
		t.Fatalf("cancel waiting order: %v", err)
	}
	if order.Status != OrderStatusCanceled {
		t.Fatalf("in-memory status = %d, want canceled", order.Status)
	}

	var refreshed Order
	if err := Db.First(&refreshed, order.ID).Error; err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if refreshed.Status != OrderStatusCanceled {
		t.Fatalf("database status = %d, want canceled", refreshed.Status)
	}
}

func TestSetSuccessDoesNotOverwriteFinalizedPayment(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	confirmedAt := now.Add(-time.Second)
	stored := newWaitingWriteTestOrder("stale-success", now)
	stored.Status = OrderStatusSuccess
	stored.RefHash = "confirmed-transaction"
	stored.FromAddress = "confirmed-sender"
	stored.ConfirmedAt = &confirmedAt
	stored.RefBlockNum = 98765
	if err := Db.Create(&stored).Error; err != nil {
		t.Fatalf("create successful order: %v", err)
	}

	stale := stored
	stale.Status = OrderStatusConfirming
	stale.RefHash = ""
	stale.FromAddress = ""
	stale.RefBlockNum = 0
	zero := time.Unix(0, 0)
	stale.ConfirmedAt = &zero
	if err := stale.SetSuccess(); !errors.Is(err, ErrOrderNoLongerReceivable) {
		t.Fatalf("complete finalized order error = %v, want ErrOrderNoLongerReceivable", err)
	}

	var refreshed Order
	if err := Db.First(&refreshed, stored.ID).Error; err != nil {
		t.Fatalf("reload order: %v", err)
	}
	assertPaymentFields(t, refreshed, OrderStatusSuccess, confirmedAt)
}

func TestSetSuccessTransitionsConfirmingOrder(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	confirmedAt := now.Add(-time.Second)
	order := newWaitingWriteTestOrder("success-confirming", now)
	order.Status = OrderStatusConfirming
	order.RefHash = "confirmed-transaction"
	order.FromAddress = "confirmed-sender"
	order.ConfirmedAt = &confirmedAt
	order.RefBlockNum = 98765
	if err := Db.Create(&order).Error; err != nil {
		t.Fatalf("create confirming order: %v", err)
	}

	if err := order.SetSuccess(); err != nil {
		t.Fatalf("complete confirming order: %v", err)
	}

	var refreshed Order
	if err := Db.First(&refreshed, order.ID).Error; err != nil {
		t.Fatalf("reload order: %v", err)
	}
	assertPaymentFields(t, refreshed, OrderStatusSuccess, confirmedAt)
}

func TestSetFailedDoesNotOverwriteFinalizedPayment(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	confirmedAt := now.Add(-time.Second)
	stored := newWaitingWriteTestOrder("stale-failed", now)
	stored.Status = OrderStatusSuccess
	stored.RefHash = "confirmed-transaction"
	stored.FromAddress = "confirmed-sender"
	stored.ConfirmedAt = &confirmedAt
	stored.RefBlockNum = 98765
	if err := Db.Create(&stored).Error; err != nil {
		t.Fatalf("create successful order: %v", err)
	}

	stale := stored
	stale.Status = OrderStatusConfirming
	stale.RefHash = ""
	stale.FromAddress = ""
	stale.RefBlockNum = 0
	zero := time.Unix(0, 0)
	stale.ConfirmedAt = &zero
	if err := stale.SetFailed(); !errors.Is(err, ErrOrderNoLongerReceivable) {
		t.Fatalf("fail finalized order error = %v, want ErrOrderNoLongerReceivable", err)
	}

	var refreshed Order
	if err := Db.First(&refreshed, stored.ID).Error; err != nil {
		t.Fatalf("reload order: %v", err)
	}
	assertPaymentFields(t, refreshed, OrderStatusSuccess, confirmedAt)
}

func TestSetFailedTransitionsConfirmingOrder(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	confirmedAt := now.Add(-time.Second)
	order := newWaitingWriteTestOrder("failed-confirming", now)
	order.Status = OrderStatusConfirming
	order.RefHash = "confirmed-transaction"
	order.FromAddress = "confirmed-sender"
	order.ConfirmedAt = &confirmedAt
	order.RefBlockNum = 98765
	if err := Db.Create(&order).Error; err != nil {
		t.Fatalf("create confirming order: %v", err)
	}

	if err := order.SetFailed(); err != nil {
		t.Fatalf("fail confirming order: %v", err)
	}

	var refreshed Order
	if err := Db.First(&refreshed, order.ID).Error; err != nil {
		t.Fatalf("reload order: %v", err)
	}
	assertPaymentFields(t, refreshed, OrderStatusFailed, confirmedAt)
}

func TestSetExpiredDoesNotExpireOrderWhoseDeadlineWasExtended(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	stored := newWaitingWriteTestOrder("stale-expiry-deadline", now)
	stored.ExpiredAt = now.Add(-time.Minute)
	if err := Db.Create(&stored).Error; err != nil {
		t.Fatalf("create expired waiting order: %v", err)
	}

	stale := stored
	extendedDeadline := now.Add(time.Hour)
	if err := Db.Model(&Order{}).Where("id = ?", stored.ID).Update("expired_at", extendedDeadline).Error; err != nil {
		t.Fatalf("extend order deadline: %v", err)
	}
	if err := stale.SetExpired(); !errors.Is(err, ErrOrderNoLongerReceivable) {
		t.Fatalf("expire after deadline extension error = %v, want ErrOrderNoLongerReceivable", err)
	}

	var refreshed Order
	if err := Db.First(&refreshed, stored.ID).Error; err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if refreshed.Status != OrderStatusWaiting || !refreshed.ExpiredAt.Equal(extendedDeadline) {
		t.Fatalf("extended order was expired: %+v", refreshed)
	}
}

func TestSetExpiredTransitionsExpiredWaitingOrder(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	order := newWaitingWriteTestOrder("expire-waiting", time.Now())
	order.ExpiredAt = time.Now().Add(-time.Minute)
	if err := Db.Create(&order).Error; err != nil {
		t.Fatalf("create expired waiting order: %v", err)
	}

	if err := order.SetExpired(); err != nil {
		t.Fatalf("expire waiting order: %v", err)
	}
	if order.Status != OrderStatusExpired {
		t.Fatalf("in-memory status = %d, want expired", order.Status)
	}

	var refreshed Order
	if err := Db.First(&refreshed, order.ID).Error; err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if refreshed.Status != OrderStatusExpired {
		t.Fatalf("database status = %d, want expired", refreshed.Status)
	}
}

func assertConfirmedPaymentFields(t *testing.T, order Order, confirmedAt time.Time) {
	t.Helper()
	assertPaymentFields(t, order, OrderStatusConfirming, confirmedAt)
}

func assertPaymentFields(t *testing.T, order Order, wantStatus int, confirmedAt time.Time) {
	t.Helper()
	if order.Status != wantStatus || order.RefHash != "confirmed-transaction" || order.FromAddress != "confirmed-sender" || order.RefBlockNum != 98765 {
		t.Fatalf("confirmed order was overwritten: %+v", order)
	}
	if order.ConfirmedAt == nil || !order.ConfirmedAt.Equal(confirmedAt) {
		t.Fatalf("confirmed_at = %v, want %v", order.ConfirmedAt, confirmedAt)
	}
}

func newWaitingWriteTestOrder(tradeID string, now time.Time) Order {
	confirmedAt := now.Add(-time.Minute)
	createdAt := Datetime(now.Add(-2 * time.Minute))
	updatedAt := Datetime(now)
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
		ConfirmedAt:  &confirmedAt,
		AutoTimeAt:   AutoTimeAt{CreatedAt: &createdAt, UpdatedAt: &updatedAt},
	}
}
