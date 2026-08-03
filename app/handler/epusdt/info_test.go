package epusdt

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/v03413/bepusdt/app/model"
)

func TestInfoReportsTransactionHashVerificationSupport(t *testing.T) {
	gin.SetMode(gin.TestMode)
	if err := model.Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	for _, tc := range []struct {
		tradeType model.TradeType
		want      bool
	}{
		{tradeType: model.UsdcSolana, want: true},
		{tradeType: model.UsdtErc20, want: false},
	} {
		order := model.Order{
			OrderId:      "merchant-" + string(tc.tradeType),
			TradeId:      "local-" + string(tc.tradeType),
			TradeType:    tc.tradeType,
			Fiat:         model.CNY,
			Crypto:       model.USDT,
			Rate:         "7.00",
			Amount:       "1.00",
			Money:        "7.00",
			Address:      "11111111111111111111111111111111",
			MatchAddress: "11111111111111111111111111111111",
			Status:       model.OrderStatusWaiting,
			ApiType:      model.OrderApiTypeEpusdt,
			ExpiredAt:    now.Add(10 * time.Minute),
			ConfirmedAt:  &now,
			AutoTimeAt:   model.AutoTimeAt{CreatedAt: (*model.Datetime)(&now), UpdatedAt: (*model.Datetime)(&now)},
		}
		if err := model.Db.Create(&order).Error; err != nil {
			t.Fatalf("create %s order: %v", tc.tradeType, err)
		}

		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/pay/info", bytes.NewBufferString(`{"trade_id":"`+order.TradeId+`"}`))
		ctx.Request.Header.Set("Content-Type", "application/json")

		new(Epusdt).Info(ctx)

		var response struct {
			StatusCode int `json:"status_code"`
			Data       struct {
				CanVerifyTransactionHash bool `json:"can_verify_transaction_hash"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode %s response: %v", tc.tradeType, err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("unexpected %s response: %s", tc.tradeType, w.Body.String())
		}
		if response.Data.CanVerifyTransactionHash != tc.want {
			t.Fatalf("can_verify_transaction_hash for %s = %t, want %t", tc.tradeType, response.Data.CanVerifyTransactionHash, tc.want)
		}
	}
}
