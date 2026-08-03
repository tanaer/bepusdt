package task

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/v03413/bepusdt/app/model"
)

func TestMarkFinalConfirmedRejectsOrderWhoseStatusChanged(t *testing.T) {
	if err := model.Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	confirmedAt := now.Add(-time.Second)
	createdAt := model.Datetime(now.Add(-time.Minute))
	stored := model.Order{
		OrderId:      "finalized-before-confirmation",
		TradeId:      "finalized-before-confirmation",
		TradeType:    model.UsdtTrc20,
		Fiat:         model.CNY,
		Crypto:       model.USDT,
		Rate:         "7.00",
		Amount:       "1.00",
		Money:        "7.00",
		Address:      "TTestAddress1234567890",
		MatchAddress: "TTestAddress1234567890",
		Status:       model.OrderStatusSuccess,
		ApiType:      model.OrderApiTypeEpusdt,
		RefHash:      "confirmed-transaction",
		RefBlockNum:  98765,
		FromAddress:  "confirmed-sender",
		ExpiredAt:    now.Add(time.Minute),
		ConfirmedAt:  &confirmedAt,
		AutoTimeAt:   model.AutoTimeAt{CreatedAt: &createdAt, UpdatedAt: &createdAt},
	}
	if err := model.Db.Create(&stored).Error; err != nil {
		t.Fatalf("create successful order: %v", err)
	}

	stale := stored
	stale.Status = model.OrderStatusConfirming
	stale.RefHash = ""
	stale.RefBlockNum = 0
	stale.FromAddress = ""
	if err := markFinalConfirmed(stale); !errors.Is(err, model.ErrOrderNoLongerReceivable) {
		t.Fatalf("mark finalized order error = %v, want ErrOrderNoLongerReceivable", err)
	}

	var refreshed model.Order
	if err := model.Db.First(&refreshed, stored.ID).Error; err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if refreshed.Status != model.OrderStatusSuccess || refreshed.RefHash != "confirmed-transaction" || refreshed.RefBlockNum != 98765 || refreshed.FromAddress != "confirmed-sender" {
		t.Fatalf("finalized order was overwritten: %+v", refreshed)
	}
}

func TestMarkConfirmingOrderFailedRejectsOrderWhoseStatusChanged(t *testing.T) {
	if err := model.Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	confirmedAt := now.Add(-time.Second)
	createdAt := model.Datetime(now.Add(-time.Minute))
	stored := model.Order{
		OrderId:      "finalized-before-failure",
		TradeId:      "finalized-before-failure",
		TradeType:    model.UsdtTrc20,
		Fiat:         model.CNY,
		Crypto:       model.USDT,
		Rate:         "7.00",
		Amount:       "1.00",
		Money:        "7.00",
		Address:      "TTestAddress1234567890",
		MatchAddress: "TTestAddress1234567890",
		Status:       model.OrderStatusSuccess,
		ApiType:      model.OrderApiTypeEpusdt,
		RefHash:      "confirmed-transaction",
		RefBlockNum:  98765,
		FromAddress:  "confirmed-sender",
		ExpiredAt:    now.Add(time.Minute),
		ConfirmedAt:  &confirmedAt,
		AutoTimeAt:   model.AutoTimeAt{CreatedAt: &createdAt, UpdatedAt: &createdAt},
	}
	if err := model.Db.Create(&stored).Error; err != nil {
		t.Fatalf("create successful order: %v", err)
	}

	stale := stored
	stale.Status = model.OrderStatusConfirming
	stale.RefHash = ""
	stale.RefBlockNum = 0
	stale.FromAddress = ""
	if err := markConfirmingOrderFailed(&stale); !errors.Is(err, model.ErrOrderNoLongerReceivable) {
		t.Fatalf("fail finalized order error = %v, want ErrOrderNoLongerReceivable", err)
	}

	var refreshed model.Order
	if err := model.Db.First(&refreshed, stored.ID).Error; err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if refreshed.Status != model.OrderStatusSuccess || refreshed.RefHash != "confirmed-transaction" || refreshed.RefBlockNum != 98765 || refreshed.FromAddress != "confirmed-sender" {
		t.Fatalf("finalized order was overwritten: %+v", refreshed)
	}
}
