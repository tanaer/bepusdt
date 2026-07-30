package task

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/smallnest/chanx"
	"github.com/v03413/bepusdt/app/conf"
	"github.com/v03413/bepusdt/app/model"
)

func TestEvmSyncBlocksForwardRotatesEndpointOnInvalidRPCResponse(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "evm-rpc-failover.db")
	if err := model.Init(dbPath, "", ""); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	t.Cleanup(model.Close)

	wallet := model.Wallet{
		Name:        "bsc",
		Status:      model.WaStatusEnable,
		Address:     "0x0000000000000000000000000000000000000019",
		MatchAddr:   "0x0000000000000000000000000000000000000019",
		TradeType:   string(model.UsdtBep20),
		OtherNotify: model.WaOtherEnable,
	}
	if err := model.Db.Create(&wallet).Error; err != nil {
		t.Fatalf("create wallet: %v", err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"jsonrpc":"2.0","error":{"code":429,"message":"Too Many Requests"}}`, http.StatusTooManyRequests)
	}))
	defer bad.Close()

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x64"}`))
	}))
	defer good.Close()

	model.SetK(model.RpcEndpointBsc, bad.URL+"\n"+good.URL)
	model.RefreshC()

	scanner := evm{
		Network:        conf.Bsc,
		Client:         bad.Client(),
		blockScanQueue: chanx.NewUnboundedChan[evmBlock](context.Background(), 30),
	}

	scanner.syncBlocksForward(context.Background())

	if got := model.Endpoint(conf.Bsc); got != good.URL {
		t.Fatalf("expected active endpoint to rotate to %q, got %q", good.URL, got)
	}
}

func TestEvmLatestBlockNumberFallsBackToNextEndpointImmediately(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "evm-latest-block-failover.db")
	if err := model.Init(dbPath, "", ""); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	t.Cleanup(model.Close)

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":429,"message":"Too Many Requests"}}`))
	}))
	defer bad.Close()

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x65"}`))
	}))
	defer good.Close()

	model.SetK(model.RpcEndpointArbitrum, bad.URL+"\n"+good.URL)
	model.RefreshC()

	scanner := evm{
		Network: conf.Arbitrum,
		Client:  good.Client(),
	}

	got, err := scanner.latestBlockNumber(context.Background())
	if err != nil {
		t.Fatalf("expected fallback request to succeed, got error: %v", err)
	}
	if got != 101 {
		t.Fatalf("expected latest block 101, got %d", got)
	}
	if active := model.Endpoint(conf.Arbitrum); active != good.URL {
		t.Fatalf("expected active endpoint to be rotated to %q, got %q", good.URL, active)
	}
}
