package epusdt

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/v03413/bepusdt/app/model"
)

func TestVerifyTransactionMarksOrderConfirming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	if err := model.Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	order := model.Order{
		OrderId:      "merchant-order",
		TradeId:      "local-order",
		TradeType:    model.UsdtTrc20,
		Fiat:         model.CNY,
		Crypto:       model.USDT,
		Rate:         "7.00",
		Amount:       "65.94",
		Money:        "461.58",
		Address:      "TKNUJShSXfCDii9bxdguPwgZsKXG6rBLC6",
		MatchAddress: "TKNUJShSXfCDii9bxdguPwgZsKXG6rBLC6",
		Status:       model.OrderStatusWaiting,
		ApiType:      model.OrderApiTypeEpusdt,
		ExpiredAt:    now.Add(10 * time.Minute),
		ConfirmedAt:  &now,
		AutoTimeAt:   model.AutoTimeAt{CreatedAt: (*model.Datetime)(&now), UpdatedAt: (*model.Datetime)(&now)},
	}
	if err := model.Db.Create(&order).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}

	originalVerifier := verifyAndClaimSubmittedPayment
	verifyAndClaimSubmittedPayment = func(_ context.Context, o *model.Order, hash string) (bool, error) {
		return model.ClaimPaymentConfirmation(o, model.PaymentConfirmation{
			BlockNum: 100,
			From:     "TJ5usJLLwjwn7Pw3TPbdzreG7dvgKzfQ5y",
			Hash:     hash,
			At:       now.Add(time.Second),
			Amount:   decimal.RequireFromString("65.94"),
		})
	}
	t.Cleanup(func() { verifyAndClaimSubmittedPayment = originalVerifier })

	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/pay/verify-transaction", bytes.NewBufferString(`{"trade_id":"local-order","tx_hash":"303245e65da6dca3e7aed8dc5386cd0cbbc7f67e4c21ff4ee0d9330055a41983"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	new(Epusdt).VerifyTransaction(ctx)

	var response struct {
		StatusCode int `json:"status_code"`
		Data       struct {
			Status    int    `json:"status"`
			TradeHash string `json:"trade_hash"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.StatusCode != http.StatusOK || response.Data.Status != model.OrderStatusConfirming {
		t.Fatalf("unexpected response: %s", w.Body.String())
	}
	if response.Data.TradeHash == "" {
		t.Fatalf("response should return the verified transaction hash: %s", w.Body.String())
	}

	var refreshed model.Order
	if err := model.Db.First(&refreshed, order.ID).Error; err != nil {
		t.Fatalf("load updated order: %v", err)
	}
	if refreshed.Status != model.OrderStatusConfirming || refreshed.RefHash != response.Data.TradeHash {
		t.Fatalf("order was not marked confirming: %+v", refreshed)
	}
}
