package task

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/smallnest/chanx"
	"github.com/v03413/bepusdt/app/conf"
	"github.com/v03413/bepusdt/app/model"
)

func TestEvmBlockParseRequeuesWhenRPCBlockIsTemporarilyUnavailable(t *testing.T) {
	initSolanaReconcileTestLog(t)

	if err := model.Init(filepath.Join(t.TempDir(), "evm-null-block.db"), "", ""); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	t.Cleanup(model.Close)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"jsonrpc":"2.0","id":113093582,"result":null}]`))
	}))
	defer server.Close()

	model.SetK(model.RpcEndpointBsc, server.URL)
	model.RefreshC()

	block := evmBlock{From: 113093582, To: 113093582}
	scanner := evm{
		Network:        conf.Bsc,
		Client:         server.Client(),
		blockScanQueue: chanx.NewUnboundedChan[evmBlock](context.Background(), 30),
	}
	scanner.getBlockByNumber(block)

	select {
	case got := <-scanner.blockScanQueue.Out:
		if got != block {
			t.Fatalf("expected block %+v to be requeued, got %+v", block, got)
		}
	case <-time.After(time.Second):
		t.Fatalf("expected temporarily unavailable block %+v to be requeued", block)
	}
}
