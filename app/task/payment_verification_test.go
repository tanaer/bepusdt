package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/v03413/bepusdt/app/conf"
	"github.com/v03413/bepusdt/app/model"
)

func TestOrderTransferMatchReproducesAffectedTRC20Payment(t *testing.T) {
	if err := model.Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	createdAt := time.Date(2026, 7, 19, 14, 2, 20, 0, time.UTC)
	order := model.Order{
		TradeType:    model.UsdtTrc20,
		Amount:       "65.94",
		Address:      "TKNUJShSXfCDii9bxdguPwgZsKXG6rBLC6",
		MatchAddress: "TKNUJShSXfCDii9bxdguPwgZsKXG6rBLC6",
		ExpiredAt:    createdAt.Add(30 * time.Minute),
		AutoTimeAt:   model.AutoTimeAt{CreatedAt: (*model.Datetime)(&createdAt)},
	}
	transfer := transfer{
		Network:     conf.Tron,
		TxHash:      "303245e65da6dca3e7aed8dc5386cd0cbbc7f67e4c21ff4ee0d9330055a41983",
		TradeType:   model.UsdtTrc20,
		Amount:      decimal.RequireFromString("65.94"),
		RecvAddress: "TKNUJShSXfCDii9bxdguPwgZsKXG6rBLC6",
		Timestamp:   time.Date(2026, 7, 19, 14, 7, 6, 0, time.UTC),
		BlockNum:    84599893,
	}

	if reason := orderTransferMatchReason(order, transfer); reason != orderMatchOK {
		t.Fatalf("affected payment should match, got reason %q", reason)
	}
}

func TestGetReceivableOrdersReturnsDatabaseReadFailure(t *testing.T) {
	if err := model.Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}
	sqlDB, err := model.Db.DB()
	if err != nil {
		t.Fatalf("get sql database: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close test database: %v", err)
	}

	if _, err := getReceivableOrders(); err == nil {
		t.Fatal("receivable-order query failure must be returned so transfers can be retried")
	}
}

func TestNormalizeSubmittedPaymentHash(t *testing.T) {
	if _, err := normalizeSubmittedPaymentHash(model.UsdtTrc20, "not-a-hash"); !errors.Is(err, ErrInvalidSubmittedPaymentHash) {
		t.Fatalf("invalid TRON hash error = %v, want ErrInvalidSubmittedPaymentHash", err)
	}
	if _, err := normalizeSubmittedPaymentHash(model.UsdtErc20, strings.Repeat("a", 64)); !errors.Is(err, ErrUnsupportedSubmittedPayment) {
		t.Fatalf("unsupported network error = %v, want ErrUnsupportedSubmittedPayment", err)
	}

	normalized, err := normalizeSubmittedPaymentHash(model.UsdtBep20, strings.Repeat("A", 64))
	if err != nil {
		t.Fatalf("normalize BSC hash: %v", err)
	}
	if normalized != "0x"+strings.Repeat("a", 64) {
		t.Fatalf("normalized BSC hash = %q", normalized)
	}
}

func TestOrderTransferMatchReasonIdentifiesMismatch(t *testing.T) {
	if err := model.Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	order := model.Order{
		TradeType:    model.UsdtBep20,
		Amount:       "17.54",
		Address:      "0x00000000000000000000000000000000000000aa",
		MatchAddress: "0x00000000000000000000000000000000000000aa",
		ExpiredAt:    now.Add(time.Minute),
		AutoTimeAt:   model.AutoTimeAt{CreatedAt: (*model.Datetime)(&now)},
	}
	matchedTransfer := transfer{
		Network:     conf.Bsc,
		TradeType:   model.UsdtBep20,
		Amount:      decimal.RequireFromString("17.54"),
		RecvAddress: "0x00000000000000000000000000000000000000AA",
		Timestamp:   now.Add(time.Second),
	}
	if reason := orderTransferMatchReason(order, matchedTransfer); reason != orderMatchOK {
		t.Fatalf("EVM addresses must match case-insensitively, got reason %q", reason)
	}

	matchedTransfer.TradeType = model.UsdcBep20
	if reason := orderTransferMatchReason(order, matchedTransfer); reason != orderMatchTradeType {
		t.Fatalf("expected trade type mismatch, got reason %q", reason)
	}
	matchedTransfer.TradeType = model.UsdtBep20

	matchedTransfer.RecvAddress = "0x00000000000000000000000000000000000000bb"
	if reason := orderTransferMatchReason(order, matchedTransfer); reason != orderMatchReceivingAddress {
		t.Fatalf("expected recipient mismatch, got reason %q", reason)
	}
	matchedTransfer.RecvAddress = "0x00000000000000000000000000000000000000AA"

	matchedTransfer.Amount = decimal.RequireFromString("17.55")
	if reason := orderTransferMatchReason(order, matchedTransfer); reason != orderMatchAmount {
		t.Fatalf("expected amount mismatch, got reason %q", reason)
	}
	matchedTransfer.Amount = decimal.RequireFromString("17.54")
	matchedTransfer.Timestamp = now
	if reason := orderTransferMatchReason(order, matchedTransfer); reason != orderMatchPaymentTimeWindow {
		t.Fatalf("expected payment time mismatch, got reason %q", reason)
	}
}

func TestVerifySubmittedPaymentUsesCanonicalOrderMatch(t *testing.T) {
	if err := model.Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	order := model.Order{
		TradeType:    model.UsdtTrc20,
		Amount:       "65.94",
		Address:      "TKNUJShSXfCDii9bxdguPwgZsKXG6rBLC6",
		MatchAddress: "TKNUJShSXfCDii9bxdguPwgZsKXG6rBLC6",
		ExpiredAt:    now.Add(time.Minute),
		AutoTimeAt:   model.AutoTimeAt{CreatedAt: (*model.Datetime)(&now)},
	}
	valid := transfer{
		Network:     conf.Tron,
		TxHash:      "303245e65da6dca3e7aed8dc5386cd0cbbc7f67e4c21ff4ee0d9330055a41983",
		TradeType:   model.UsdtTrc20,
		Amount:      decimal.RequireFromString("65.94"),
		RecvAddress: order.MatchAddress,
		Timestamp:   now.Add(time.Second),
		BlockNum:    1,
	}
	lookup := func(context.Context, model.Order, string) (transfer, error) {
		return valid, nil
	}

	got, err := verifySubmittedPayment(context.Background(), order, valid.TxHash, lookup)
	if err != nil {
		t.Fatalf("verify valid submitted payment: %v", err)
	}
	if got.TxHash != valid.TxHash {
		t.Fatalf("verified hash = %s, want %s", got.TxHash, valid.TxHash)
	}

	wrongAddress := func(context.Context, model.Order, string) (transfer, error) {
		t := valid
		t.RecvAddress = "TWrongAddress"
		return t, nil
	}
	if _, err := verifySubmittedPayment(context.Background(), order, valid.TxHash, wrongAddress); !errors.Is(err, ErrSubmittedPaymentDoesNotMatch) {
		t.Fatalf("wrong recipient error = %v, want ErrSubmittedPaymentDoesNotMatch", err)
	}
}

func TestVerifySubmittedPaymentFetchesAndValidatesBscTransfer(t *testing.T) {
	if err := model.Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	const transactionHash = "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const sender = "0x0000000000000000000000000000000000000001"
	const receiver = "0x00000000000000000000000000000000000000aa"
	const blockNumber = "0x64"
	const timestamp = "0x6694b540"
	amount := big.NewInt(0)
	amount.SetString("17540000000000000000", 10)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode RPC request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case "eth_getTransactionReceipt":
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"status":"0x1","blockNumber":%q,"logs":[{"address":%q,"topics":[%q,%q,%q],"data":%q}]}}`,
				blockNumber, conf.UsdtBep20, evmTransferEvent, paddedEvmTopic(sender), paddedEvmTopic(receiver), "0x"+amount.Text(16))
		case "eth_getBlockByNumber":
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"number":%q,"timestamp":%q}}`, blockNumber, timestamp)
		default:
			t.Fatalf("unexpected BSC RPC method: %s", request.Method)
		}
	}))
	defer server.Close()
	model.SetK(model.RpcEndpointBsc, server.URL)

	blockTime := time.Unix(0x6694b540, 0)
	createdAt := blockTime.Add(-time.Minute)
	order := model.Order{
		TradeType:    model.UsdtBep20,
		Amount:       "17.54",
		Address:      receiver,
		MatchAddress: receiver,
		ExpiredAt:    blockTime.Add(time.Minute),
		AutoTimeAt:   model.AutoTimeAt{CreatedAt: (*model.Datetime)(&createdAt)},
	}

	payment, err := verifySubmittedPayment(context.Background(), order, transactionHash, lookupSubmittedPayment)
	if err != nil {
		t.Fatalf("verify BSC transaction: %v", err)
	}
	if payment.TradeType != model.UsdtBep20 || payment.RecvAddress != receiver || !payment.Amount.Equal(decimal.RequireFromString("17.54")) {
		t.Fatalf("unexpected verified BSC payment: %+v", payment)
	}
}

func paddedEvmTopic(address string) string {
	return "0x" + strings.Repeat("0", 24) + strings.TrimPrefix(strings.ToLower(address), "0x")
}
