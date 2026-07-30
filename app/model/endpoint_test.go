package model

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/v03413/bepusdt/app/conf"
)

func resetEndpointStateForTest() {
	confCache.Range(func(key, value any) bool {
		confCache.Delete(key)
		return true
	})
	endpointMu.Lock()
	defer endpointMu.Unlock()
	endpointIndexes = make(map[Network]int)
}

func TestEndpointCandidatesParsesSingleAndMultipleAddresses(t *testing.T) {
	resetEndpointStateForTest()

	confCache.Store(RpcEndpointArbitrum, " https://one.example/rpc \nhttps://two.example/rpc, https://three.example/rpc;;")

	got := EndpointCandidates(conf.Arbitrum)
	want := []string{
		"https://one.example/rpc",
		"https://two.example/rpc",
		"https://three.example/rpc",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected candidates %+v, got %+v", want, got)
	}

	if got := Endpoint(conf.Arbitrum); got != want[0] {
		t.Fatalf("expected first endpoint %q, got %q", want[0], got)
	}
}

func TestReportEndpointFailureRotatesOnlyFailedNetwork(t *testing.T) {
	resetEndpointStateForTest()

	confCache.Store(RpcEndpointArbitrum, "https://arb-one.example\nhttps://arb-two.example")
	confCache.Store(RpcEndpointBsc, "https://bsc-one.example\nhttps://bsc-two.example")

	if got := Endpoint(conf.Arbitrum); got != "https://arb-one.example" {
		t.Fatalf("expected first arbitrum endpoint, got %q", got)
	}

	next := ReportEndpointFailure(conf.Arbitrum, "https://arb-one.example")
	if next != "https://arb-two.example" {
		t.Fatalf("expected arbitrum to rotate to second endpoint, got %q", next)
	}
	if got := Endpoint(conf.Arbitrum); got != "https://arb-two.example" {
		t.Fatalf("expected active arbitrum endpoint to remain second endpoint, got %q", got)
	}
	if got := Endpoint(conf.Bsc); got != "https://bsc-one.example" {
		t.Fatalf("expected bsc endpoint to be unaffected, got %q", got)
	}

	ReportEndpointFailure(conf.Arbitrum, "https://arb-two.example")
	if got := Endpoint(conf.Arbitrum); got != "https://arb-one.example" {
		t.Fatalf("expected arbitrum endpoint to wrap around, got %q", got)
	}
}

func TestSetKRefreshesEndpointCacheAfterCommit(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "endpoint-cache.db")
	if err := Init(dbPath, "", ""); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	t.Cleanup(Close)

	SetK(RpcEndpointArbitrum, "https://arb-one.example,https://arb-two.example")

	got := EndpointCandidates(conf.Arbitrum)
	want := []string{"https://arb-one.example", "https://arb-two.example"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected candidates %+v, got %+v", want, got)
	}
}
