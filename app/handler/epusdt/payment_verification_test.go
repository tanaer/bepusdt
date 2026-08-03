package epusdt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

func TestVerifyTransactionRejectsUnsupportedNetworkBeforeIdempotency(t *testing.T) {
	gin.SetMode(gin.TestMode)
	if err := model.Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	order := newVerifyTransactionTestOrder("unsupported-network", model.UsdtErc20, model.OrderStatusSuccess, now)
	order.RefHash = "0x" + strings.Repeat("a", 64)
	if err := model.Db.Create(&order).Error; err != nil {
		t.Fatalf("create order: %v", err)
	}

	called := false
	originalVerifier := verifyAndClaimSubmittedPayment
	verifyAndClaimSubmittedPayment = func(context.Context, *model.Order, string) (bool, error) {
		called = true
		return false, nil
	}
	t.Cleanup(func() { verifyAndClaimSubmittedPayment = originalVerifier })

	response := invokeVerifyTransaction(t, order.TradeId, order.RefHash)
	if response.StatusCode != http.StatusBadRequest || response.ErrorCode != "unsupported_network" {
		t.Fatalf("unsupported-network response = %+v, want status_code=400 error_code=unsupported_network", response)
	}
	if called {
		t.Fatal("verifier must not run for an unsupported payment network")
	}
}

func TestVerifyTransactionRejectsOversizedInputBeforeVerifier(t *testing.T) {
	gin.SetMode(gin.TestMode)
	if err := model.Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	order := newVerifyTransactionTestOrder("oversized-transaction-hash", model.UsdcSolana, model.OrderStatusWaiting, now)
	if err := model.Db.Create(&order).Error; err != nil {
		t.Fatalf("create order: %v", err)
	}

	called := false
	originalVerifier := verifyAndClaimSubmittedPayment
	verifyAndClaimSubmittedPayment = func(context.Context, *model.Order, string) (bool, error) {
		called = true
		return false, nil
	}
	t.Cleanup(func() { verifyAndClaimSubmittedPayment = originalVerifier })

	for _, tc := range []struct {
		name string
		hash string
	}{
		{name: "hash field is shorter than every supported transaction hash", hash: strings.Repeat("2", 63)},
		{name: "hash field exceeds its limit", hash: strings.Repeat("2", 89)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called = false
			response := invokeVerifyTransaction(t, order.TradeId, tc.hash)
			assertVerifyTransactionErrorCode(t, response, "invalid_hash")
			if called {
				t.Fatal("verifier must not run for an out-of-range transaction hash")
			}
		})
	}

	t.Run("request body exceeds its limit", func(t *testing.T) {
		called = false
		payload := `{"trade_id":` + strconv.Quote(order.TradeId) + `,"tx_hash":` + strconv.Quote(strings.Repeat("2", 64)) + `,"padding":` + strconv.Quote(strings.Repeat("x", 2048)) + `}`
		response := invokeVerifyTransactionPayload(t, payload)
		assertVerifyTransactionErrorCode(t, response, "invalid_hash")
		if called {
			t.Fatal("verifier must not run when the request body exceeds its limit")
		}
	})
}

func TestVerifyTransactionReturnsStableErrorCodes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	if err := model.Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	originalVerifier := verifyAndClaimSubmittedPayment
	t.Cleanup(func() { verifyAndClaimSubmittedPayment = originalVerifier })

	t.Run("invalid request", func(t *testing.T) {
		response := invokeVerifyTransactionPayload(t, `{"trade_id":"missing-hash"}`)
		assertVerifyTransactionErrorCode(t, response, "invalid_hash")
	})

	t.Run("order not found", func(t *testing.T) {
		response := invokeVerifyTransaction(t, "does-not-exist", strings.Repeat("a", 64))
		assertVerifyTransactionErrorCode(t, response, "order_not_receivable")
	})

	t.Run("order status is not receivable", func(t *testing.T) {
		order := newVerifyTransactionTestOrder("not-receivable", model.UsdtTrc20, model.OrderStatusSuccess, now)
		if err := model.Db.Create(&order).Error; err != nil {
			t.Fatalf("create order: %v", err)
		}
		response := invokeVerifyTransaction(t, order.TradeId, strings.Repeat("a", 64))
		assertVerifyTransactionErrorCode(t, response, "order_not_receivable")
	})

	for _, tc := range []struct {
		name string
		err  error
		code string
	}{
		{name: "invalid hash", err: task.ErrInvalidSubmittedPaymentHash, code: "invalid_hash"},
		{name: "unsupported verifier", err: task.ErrUnsupportedSubmittedPayment, code: "unsupported_network"},
		{name: "transaction not found", err: task.ErrSubmittedPaymentNotFound, code: "transaction_not_found"},
		{name: "transaction mismatch", err: task.ErrSubmittedPaymentDoesNotMatch, code: "transaction_mismatch"},
		{name: "transaction already used", err: model.ErrPaymentHashAlreadyClaimed, code: "transaction_already_used"},
		{name: "order no longer receivable", err: model.ErrOrderNoLongerReceivable, code: "order_not_receivable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			order := newVerifyTransactionTestOrder("error-"+tc.code, model.UsdtTrc20, model.OrderStatusWaiting, now)
			if err := model.Db.Create(&order).Error; err != nil {
				t.Fatalf("create order: %v", err)
			}
			verifyAndClaimSubmittedPayment = func(context.Context, *model.Order, string) (bool, error) {
				return false, tc.err
			}

			response := invokeVerifyTransaction(t, order.TradeId, strings.Repeat("a", 64))
			assertVerifyTransactionErrorCode(t, response, tc.code)
		})
	}
}

func TestVerifyTransactionUnknownErrorIsNilSafeAndUsesUnavailableCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	if err := model.Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	order := newVerifyTransactionTestOrder("unknown-verification-error", model.UsdtTrc20, model.OrderStatusWaiting, now)
	if err := model.Db.Create(&order).Error; err != nil {
		t.Fatalf("create order: %v", err)
	}

	originalVerifier := verifyAndClaimSubmittedPayment
	verifyAndClaimSubmittedPayment = func(context.Context, *model.Order, string) (bool, error) {
		return false, errors.New("temporary RPC outage")
	}
	t.Cleanup(func() { verifyAndClaimSubmittedPayment = originalVerifier })

	response := invokeVerifyTransaction(t, order.TradeId, strings.Repeat("a", 64))
	assertVerifyTransactionErrorCode(t, response, "verification_unavailable")
}

type verifyTransactionTestResponse struct {
	StatusCode int    `json:"status_code"`
	Message    string `json:"message"`
	ErrorCode  string `json:"error_code"`
}

func newVerifyTransactionTestOrder(tradeID string, tradeType model.TradeType, status int, now time.Time) model.Order {
	return model.Order{
		OrderId:      "merchant-" + tradeID,
		TradeId:      "local-" + tradeID,
		TradeType:    tradeType,
		Fiat:         model.CNY,
		Crypto:       model.USDT,
		Rate:         "7.00",
		Amount:       "2.65",
		Money:        "18.55",
		Address:      "TKNUJShSXfCDii9bxdguPwgZsKXG6rBLC6",
		MatchAddress: "TKNUJShSXfCDii9bxdguPwgZsKXG6rBLC6",
		Status:       status,
		ApiType:      model.OrderApiTypeEpusdt,
		ExpiredAt:    now.Add(time.Minute),
		ConfirmedAt:  &now,
		AutoTimeAt:   model.AutoTimeAt{CreatedAt: (*model.Datetime)(&now), UpdatedAt: (*model.Datetime)(&now)},
	}
}

func invokeVerifyTransaction(t *testing.T, tradeID, txHash string) verifyTransactionTestResponse {
	t.Helper()
	return invokeVerifyTransactionPayload(t, `{"trade_id":`+strconv.Quote(tradeID)+`,"tx_hash":`+strconv.Quote(txHash)+`}`)
}

func invokeVerifyTransactionPayload(t *testing.T, payload string) verifyTransactionTestResponse {
	t.Helper()
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/pay/verify-transaction", bytes.NewBufferString(payload))
	ctx.Request.Header.Set("Content-Type", "application/json")
	new(Epusdt).VerifyTransaction(ctx)

	var response verifyTransactionTestResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return response
}

func assertVerifyTransactionErrorCode(t *testing.T, response verifyTransactionTestResponse, want string) {
	t.Helper()
	if response.StatusCode != http.StatusBadRequest || response.ErrorCode != want {
		t.Fatalf("verification response = %+v, want status_code=400 error_code=%q", response, want)
	}
}
