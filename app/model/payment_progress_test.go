package model

import (
	"testing"

	"github.com/v03413/bepusdt/app/conf"
)

func TestGetChainProgress(t *testing.T) {
	SetChainProgress(conf.Bsc, 120)

	progress := GetChainProgress(Order{
		TradeType:   UsdtBep20,
		Status:      OrderStatusConfirming,
		RefBlockNum: 100,
	})

	if progress.Current != 15 {
		t.Fatalf("expected capped current confirmations 15, got %d", progress.Current)
	}
	if progress.Required != 15 {
		t.Fatalf("expected required confirmations 15, got %d", progress.Required)
	}
}

func TestGetChainProgressWaitingOrder(t *testing.T) {
	progress := GetChainProgress(Order{
		TradeType:   UsdtBep20,
		Status:      OrderStatusWaiting,
		RefBlockNum: 100,
	})

	if progress.Current != 0 || progress.Required != 0 {
		t.Fatalf("expected zero progress for waiting order, got %+v", progress)
	}
}
