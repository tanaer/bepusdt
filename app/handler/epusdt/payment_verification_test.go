package epusdt

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/v03413/bepusdt/app/model"
	"github.com/v03413/bepusdt/app/task"
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

func TestVerifyTransactionUsesChainSpecificIdempotencyHashComparison(t *testing.T) {
	gin.SetMode(gin.TestMode)
	if err := model.Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	for _, tc := range []struct {
		name           string
		tradeType      model.TradeType
		status         int
		storedHash     string
		submittedHash  string
		wantIdempotent bool
		wantStatusCode int
		wantMessage    string
	}{
		{
			name:           "Solana case variant is not idempotent for successful order",
			tradeType:      model.UsdcSolana,
			status:         model.OrderStatusSuccess,
			storedHash:     "2gqC3gYGfdNMQkF5xhZXmofbjuu3RbdZZrKz7pYvuArMpqgHSvvrQb25AuDVvtsswxkfjWZbDDouH1FBFWwgYkD4",
			submittedHash:  "2GqC3gYGfdNMQkF5xhZXmofbjuu3RbdZZrKz7pYvuArMpqgHSvvrQb25AuDVvtsswxkfjWZbDDouH1FBFWwgYkD4",
			wantIdempotent: false,
			wantStatusCode: http.StatusBadRequest,
			wantMessage:    "the current order status does not allow transaction verification",
		},
		{
			name:           "Solana case variant is not idempotent for confirming order",
			tradeType:      model.UsdcSolana,
			status:         model.OrderStatusConfirming,
			storedHash:     "2gqC3gYGfdNMQkF5xhZXmofbjuu3RbdZZrKz7pYvuArMpqgHSvvrQb25AuDVvtsswxkfjWZbDDouH1FBFWwgYkD4",
			submittedHash:  "2GqC3gYGfdNMQkF5xhZXmofbjuu3RbdZZrKz7pYvuArMpqgHSvvrQb25AuDVvtsswxkfjWZbDDouH1FBFWwgYkD4",
			wantIdempotent: false,
			wantStatusCode: http.StatusBadRequest,
			wantMessage:    "the current order status does not allow transaction verification",
		},
		{
			name:           "BSC optional prefix is idempotent",
			tradeType:      model.UsdtBep20,
			status:         model.OrderStatusSuccess,
			storedHash:     "0x" + strings.Repeat("a", 64),
			submittedHash:  strings.Repeat("A", 64),
			wantIdempotent: true,
			wantStatusCode: http.StatusOK,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			order := model.Order{
				OrderId:      "merchant-" + tc.name,
				TradeId:      "local-" + tc.name,
				TradeType:    tc.tradeType,
				Fiat:         model.CNY,
				Crypto:       model.USDC,
				Rate:         "7.00",
				Amount:       "2.65",
				Money:        "18.55",
				Address:      "DAzEQJ8TzdmrAgphrcGGZie4fwiXmRYXRCKX4wQh2oLf",
				MatchAddress: "DAzEQJ8TzdmrAgphrcGGZie4fwiXmRYXRCKX4wQh2oLf",
				Status:       tc.status,
				ApiType:      model.OrderApiTypeEpusdt,
				RefHash:      tc.storedHash,
				ExpiredAt:    now.Add(time.Minute),
				ConfirmedAt:  &now,
				AutoTimeAt:   model.AutoTimeAt{CreatedAt: (*model.Datetime)(&now), UpdatedAt: (*model.Datetime)(&now)},
			}
			if err := model.Db.Create(&order).Error; err != nil {
				t.Fatalf("create order: %v", err)
			}

			called := false
			originalVerifier := verifyAndClaimSubmittedPayment
			verifyAndClaimSubmittedPayment = func(context.Context, *model.Order, string) (bool, error) {
				called = true
				return false, task.ErrSubmittedPaymentDoesNotMatch
			}
			t.Cleanup(func() { verifyAndClaimSubmittedPayment = originalVerifier })

			w := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(w)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/pay/verify-transaction", bytes.NewBufferString(`{"trade_id":`+strconv.Quote(order.TradeId)+`,"tx_hash":`+strconv.Quote(tc.submittedHash)+`}`))
			ctx.Request.Header.Set("Content-Type", "application/json")
			new(Epusdt).VerifyTransaction(ctx)

			var response struct {
				StatusCode int    `json:"status_code"`
				Message    string `json:"message"`
				Data       struct {
					Idempotent bool `json:"idempotent"`
				} `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if response.Data.Idempotent != tc.wantIdempotent {
				t.Fatalf("idempotent = %t, want %t; response=%s", response.Data.Idempotent, tc.wantIdempotent, w.Body.String())
			}
			if response.StatusCode != tc.wantStatusCode {
				t.Fatalf("status_code = %d, want %d; response=%s", response.StatusCode, tc.wantStatusCode, w.Body.String())
			}
			if tc.wantMessage != "" && response.Message != tc.wantMessage {
				t.Fatalf("message = %q, want %q; response=%s", response.Message, tc.wantMessage, w.Body.String())
			}
			if called {
				t.Fatalf("verifier should not run for an already completed order; response=%s", w.Body.String())
			}
		})
	}
}
