package task

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/v03413/bepusdt/app/log"
	"github.com/v03413/bepusdt/app/model"
)

const (
	testSolanaOwner       = "DAzEQJ8TzdmrAgphrcGGZie4fwiXmRYXRCKX4wQh2oLf"
	testSolanaTokenWallet = "23PXKLkUNQ85LScKDZVWhHLzFppYiSVky2Z7iqkk4JFu"
	testSolanaSender      = "5tzFkiKscXHK5ZXCGbXZxdw7gTjjD1mBwuoFbhUvuAi9"
	testSolanaTxHash      = "3uhzPPDFKz8JTWJTPN2D7VbDTL94KXw2WyqEUvpLvbcsKVymDKETfMgwUxGsWM2tBDvteTHSRNZPU3Cx8u2hyMbe"
)

func TestSolanaReconcileWaitingOrdersMarksMatchingOrderConfirming(t *testing.T) {
	initSolanaReconcileTestLog(t)

	dbPath := filepath.Join(t.TempDir(), "solana-reconcile.db")
	if err := model.Init(dbPath, "", ""); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	t.Cleanup(model.Close)

	blockTime := time.Now().Add(-10 * time.Minute).Unix()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()

		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode rpc request: %v", err)
		}

		method, _ := req["method"].(string)
		switch method {
		case "getTokenAccountsByOwner":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"value":[{"pubkey":"` + testSolanaTokenWallet + `"}]}}`))
		case "getSignaturesForAddress":
			_, _ = w.Write([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":[{"signature":"%s","slot":424729879,"err":null,"blockTime":%d}]}`, testSolanaTxHash, blockTime)))
		case "getTransaction":
			_, _ = w.Write([]byte(fmt.Sprintf(`{
				"jsonrpc":"2.0",
				"id":1,
				"result":{
					"slot":424729879,
					"blockTime":%d,
					"meta":{
						"err":null,
						"preTokenBalances":[
							{"accountIndex":2,"mint":"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v","owner":"`+testSolanaSender+`","programId":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"},
							{"accountIndex":3,"mint":"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v","owner":"`+testSolanaOwner+`","programId":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"}
						],
						"postTokenBalances":[
							{"accountIndex":2,"mint":"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v","owner":"`+testSolanaSender+`","programId":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"},
							{"accountIndex":3,"mint":"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v","owner":"`+testSolanaOwner+`","programId":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"}
						],
						"innerInstructions":[]
					},
					"transaction":{
						"message":{
							"accountKeys":[
								{"pubkey":"`+testSolanaSender+`"},
								{"pubkey":"13gxbc8s6rPLDXPMiTnbnKD6uVdddzKpUCBZA5iCmZkd"},
								{"pubkey":"7KJjY7rArbydeLBF7gQ5LdqXRKRYyPArT99NEctsHsgU"},
								{"pubkey":"`+testSolanaTokenWallet+`"}
							],
							"instructions":[
								{
									"program":"spl-token",
									"programId":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA",
									"parsed":{
										"type":"transferChecked",
										"info":{
											"authority":"`+testSolanaSender+`",
											"source":"7KJjY7rArbydeLBF7gQ5LdqXRKRYyPArT99NEctsHsgU",
											"destination":"`+testSolanaTokenWallet+`",
											"mint":"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
											"tokenAmount":{"amount":"13190000","decimals":6,"uiAmount":13.19,"uiAmountString":"13.19"}
										}
									}
								}
							]
						}
					}
				}
			}`, blockTime)))
		default:
			t.Fatalf("unexpected rpc method: %s", method)
		}
	}))
	defer server.Close()

	model.SetK(model.RpcEndpointSolana, server.URL)

	createdAtTime := time.Unix(blockTime-6*60, 0)
	expiredAt := time.Unix(blockTime+24*60, 0)
	createdAt := model.Datetime(createdAtTime)
	zero := time.Unix(0, 0)
	order := model.Order{
		OrderId:     "sub2_20260607ef8AeiFK",
		TradeId:     "csxMKKMccWtlbGWg2Y",
		TradeType:   model.UsdcSolana,
		Crypto:      model.USDC,
		Amount:      "13.19",
		Money:       "100",
		Address:     testSolanaOwner,
		Status:      model.OrderStatusWaiting,
		NotifyUrl:   "https://example.com/webhook",
		ConfirmedAt: &zero,
		ExpiredAt:   expiredAt,
		AutoTimeAt: model.AutoTimeAt{
			CreatedAt: &createdAt,
			UpdatedAt: &createdAt,
		},
	}
	if err := model.Db.Create(&order).Error; err != nil {
		t.Fatalf("create order: %v", err)
	}

	s := newSolana()
	s.client = server.Client()
	s.reconcileWaitingOrders(context.Background())

	var refreshed model.Order
	if err := model.Db.First(&refreshed, order.ID).Error; err != nil {
		t.Fatalf("reload order: %v", err)
	}

	if refreshed.Status != model.OrderStatusConfirming {
		t.Fatalf("expected status %d, got %d", model.OrderStatusConfirming, refreshed.Status)
	}
	if refreshed.RefHash != testSolanaTxHash {
		t.Fatalf("expected ref hash %s, got %s", testSolanaTxHash, refreshed.RefHash)
	}
	if refreshed.FromAddress != testSolanaSender {
		t.Fatalf("expected from address %s, got %s", testSolanaSender, refreshed.FromAddress)
	}
	if refreshed.ConfirmedAt == nil {
		t.Fatal("expected confirmed_at to be set")
	}
	if got := refreshed.ConfirmedAt.Unix(); got != blockTime {
		t.Fatalf("expected confirmed_at unix %d, got %d", blockTime, got)
	}
	var claim model.PaymentHashClaim
	if err := model.Db.Where("order_id = ?", order.ID).First(&claim).Error; err != nil {
		t.Fatalf("load payment hash claim: %v", err)
	}
	if claim.Hash == testSolanaTxHash || !strings.HasPrefix(claim.Hash, "solana:") {
		t.Fatalf("payment hash claim = %q, want a canonical Solana claim key", claim.Hash)
	}
}

func initSolanaReconcileTestLog(t *testing.T) {
	t.Helper()

	logger := logrus.New()
	logger.SetOutput(io.Discard)
	log.Task = logger
}

func TestSolanaParsedTransactionProducesMatchingTransfer(t *testing.T) {
	tx := gjson.Parse(`{
		"blockTime":1780769985,
		"meta":{
			"preTokenBalances":[
				{"accountIndex":2,"mint":"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v","owner":"` + testSolanaSender + `","programId":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"},
				{"accountIndex":3,"mint":"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v","owner":"` + testSolanaOwner + `","programId":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"}
			],
			"postTokenBalances":[
				{"accountIndex":2,"mint":"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v","owner":"` + testSolanaSender + `","programId":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"},
				{"accountIndex":3,"mint":"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v","owner":"` + testSolanaOwner + `","programId":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"}
			],
			"innerInstructions":[]
		},
		"transaction":{
			"message":{
				"accountKeys":[
					{"pubkey":"` + testSolanaSender + `"},
					{"pubkey":"13gxbc8s6rPLDXPMiTnbnKD6uVdddzKpUCBZA5iCmZkd"},
					{"pubkey":"7KJjY7rArbydeLBF7gQ5LdqXRKRYyPArT99NEctsHsgU"},
					{"pubkey":"` + testSolanaTokenWallet + `"}
				],
				"instructions":[
					{
						"program":"spl-token",
						"programId":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA",
						"parsed":{
							"type":"transferChecked",
							"info":{
								"authority":"` + testSolanaSender + `",
								"source":"7KJjY7rArbydeLBF7gQ5LdqXRKRYyPArT99NEctsHsgU",
								"destination":"` + testSolanaTokenWallet + `",
								"mint":"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
								"tokenAmount":{"amount":"13190000","decimals":6,"uiAmount":13.19,"uiAmountString":"13.19"}
							}
						}
					}
				]
			}
		}
	}`)

	transfers := parseSolanaParsedTransfers(tx)
	if len(transfers) != 1 {
		t.Fatalf("expected 1 transfer, got %d", len(transfers))
	}

	got := transfers[0]
	if got.TradeType != model.UsdcSolana {
		t.Fatalf("expected trade type %s, got %s", model.UsdcSolana, got.TradeType)
	}
	if got.FromAddress != testSolanaSender {
		t.Fatalf("expected from %s, got %s", testSolanaSender, got.FromAddress)
	}
	if got.RecvAddress != testSolanaOwner {
		t.Fatalf("expected recv %s, got %s", testSolanaOwner, got.RecvAddress)
	}
	if got.Amount.String() != "13.19" {
		t.Fatalf("expected amount 13.19, got %s", got.Amount.String())
	}
	if got.Timestamp.Unix() != 1780769985 {
		t.Fatalf("expected timestamp 1780769985, got %d", got.Timestamp.Unix())
	}
}

func TestSolanaSlotParseRequeuesWhenRPCBlockIsTemporarilyUnavailable(t *testing.T) {
	initSolanaReconcileTestLog(t)

	dbPath := filepath.Join(t.TempDir(), "solana-null-block.db")
	if err := model.Init(dbPath, "", ""); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	t.Cleanup(model.Close)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":null}`))
	}))
	defer server.Close()

	model.SetK(model.RpcEndpointSolana, server.URL)
	model.RefreshC()

	const slot = 435577393
	s := newSolana()
	s.client = server.Client()
	s.slotParse(slot)

	select {
	case got := <-s.slotQueue.Out:
		if got != slot {
			t.Fatalf("expected slot %d to be requeued, got %d", slot, got)
		}
	case <-time.After(time.Second):
		t.Fatalf("expected temporarily unavailable slot %d to be requeued", slot)
	}
}

func TestSolanaSlotParseDoesNotRequeuePermanentlySkippedSlot(t *testing.T) {
	initSolanaReconcileTestLog(t)

	dbPath := filepath.Join(t.TempDir(), "solana-skipped-slot.db")
	if err := model.Init(dbPath, "", ""); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	t.Cleanup(model.Close)

	skipped := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32007,"message":"Slot 436683136 was skipped, or missing due to ledger jump to recent snapshot"}}`))
	}))
	defer skipped.Close()

	rateLimitedCalls := 0
	rateLimited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rateLimitedCalls++
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32005,"message":"rate limit exceeded"}}`))
	}))
	defer rateLimited.Close()

	model.SetK(model.RpcEndpointSolana, skipped.URL+","+rateLimited.URL)
	model.RefreshC()

	const slot = 436683136
	s := newSolana()
	s.client = skipped.Client()
	s.slotParse(slot)

	select {
	case got := <-s.slotQueue.Out:
		t.Fatalf("permanently skipped slot must not be requeued, got %d", got)
	case <-time.After(100 * time.Millisecond):
	}
	if rateLimitedCalls != 0 {
		t.Fatalf("permanently skipped slot must not fail over, second endpoint called %d times", rateLimitedCalls)
	}
}

func TestSolanaSlotParseSkipsFailedTransactions(t *testing.T) {
	initSolanaReconcileTestLog(t)

	if err := model.Init(filepath.Join(t.TempDir(), "solana-failed-slot.db"), "", ""); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	t.Cleanup(model.Close)

	blockTime := time.Now().Add(-time.Minute).Unix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(fmt.Sprintf(`{
			"jsonrpc":"2.0",
			"id":1,
			"result":{
				"blockTime":%d,
				"transactions":[{
					"meta":{
						"err":{"InstructionError":[0,"Custom"]},
						"preTokenBalances":[
							{"accountIndex":1,"mint":"Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB","owner":"%s","programId":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"},
							{"accountIndex":2,"mint":"Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB","owner":"%s","programId":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"}
						],
						"postTokenBalances":[
							{"accountIndex":1,"mint":"Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB","owner":"%s","programId":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"},
							{"accountIndex":2,"mint":"Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB","owner":"%s","programId":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"}
						],
						"innerInstructions":[]
					},
					"transaction":{
						"signatures":["%s"],
						"message":{
							"accountKeys":["%s","source-token-account","destination-token-account","ComputeBudget111111111111111111111111111111","Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB","TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"],
							"instructions":[{"programIdIndex":5,"accounts":[1,4,2,0],"data":"gvJRrNPqACL1K"}]
						}
					}
				}]
			}
		}`, blockTime, testSolanaSender, testSolanaOwner, testSolanaSender, testSolanaOwner, testSolanaTxHash, testSolanaSender)))
	}))
	defer server.Close()

	model.SetK(model.RpcEndpointSolana, server.URL)
	model.RefreshC()

	s := newSolana()
	s.client = server.Client()
	s.slotParse(435577393)

	select {
	case queued := <-transferQueue.Out:
		t.Fatalf("failed Solana transaction must not enter the transfer queue: %+v", queued)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSolanaTradeConfirmHandleDoesNotFinalizeFailedTransaction(t *testing.T) {
	initSolanaReconcileTestLog(t)

	if err := model.Init(filepath.Join(t.TempDir(), "solana-failed-confirmation.db"), "", ""); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	t.Cleanup(model.Close)

	now := time.Now().UTC()
	confirmedAt := now.Add(-time.Minute)
	createdAt := model.Datetime(now.Add(-2 * time.Minute))
	order := model.Order{
		OrderId:     "failed-solana-confirmation",
		TradeId:     "failed-solana-confirmation-trade",
		TradeType:   model.UsdcSolana,
		Crypto:      model.USDC,
		Amount:      "2.65",
		Money:       "20",
		Address:     testSolanaOwner,
		Status:      model.OrderStatusConfirming,
		RefHash:     testSolanaTxHash,
		RefBlockNum: 424729879,
		ConfirmedAt: &confirmedAt,
		ExpiredAt:   now.Add(time.Hour),
		AutoTimeAt: model.AutoTimeAt{
			CreatedAt: &createdAt,
			UpdatedAt: &createdAt,
		},
	}
	if err := model.Db.Create(&order).Error; err != nil {
		t.Fatalf("create confirming order: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"value":[{"slot":424729879,"confirmationStatus":"finalized","err":{"InstructionError":[0,"Custom"]}}]}}`))
	}))
	defer server.Close()

	model.SetK(model.RpcEndpointSolana, server.URL)
	model.SetK(model.BlockOffsetConfirm, "0")
	model.RefreshC()

	s := newSolana()
	s.client = server.Client()
	s.tradeConfirmHandle(context.Background())

	var refreshed model.Order
	if err := model.Db.First(&refreshed, order.ID).Error; err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if refreshed.Status != model.OrderStatusConfirming {
		t.Fatalf("failed finalized Solana transaction changed order status to %d, want confirming", refreshed.Status)
	}
}

func TestSolanaParseTransferRecognizesReportedTransaction(t *testing.T) {
	accountKeys := []string{
		"H1NwZnujLy6q2q9Mj813xCMSzkmYE6p6PzyC2zswJSmK",
		"39YTZW8PKvvrcmtmWncD9V8viWjJMG57XeKf283jhnwy",
		"9i5mDNaBqgjojBMpChrWbfCrD4hkBkgmyrvi8AFb9qHr",
		"ComputeBudget111111111111111111111111111111",
		"Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB",
		"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA",
	}
	tokenAccounts := map[string]solanaTokenOwner{
		accountKeys[1]: {
			TradeType: model.UsdtSolana,
			Address:   "H1NwZnujLy6q2q9Mj813xCMSzkmYE6p6PzyC2zswJSmK",
		},
		accountKeys[2]: {
			TradeType: model.UsdtSolana,
			Address:   "DAzEQJ8TzdmrAgphrcGGZie4fwiXmRYXRCKX4wQh2oLf",
		},
	}
	instruction := gjson.Parse(`{
		"accounts":[1,4,2,0],
		"data":"gvJRrNPqACL1K",
		"programIdIndex":5
	}`)

	s := newSolana()
	got := s.parseTransfer(instruction, accountKeys, tokenAccounts)

	if got.TradeType != model.UsdtSolana {
		t.Fatalf("expected trade type %s, got %s", model.UsdtSolana, got.TradeType)
	}
	if got.FromAddress != "H1NwZnujLy6q2q9Mj813xCMSzkmYE6p6PzyC2zswJSmK" {
		t.Fatalf("unexpected sender: %s", got.FromAddress)
	}
	if got.RecvAddress != "DAzEQJ8TzdmrAgphrcGGZie4fwiXmRYXRCKX4wQh2oLf" {
		t.Fatalf("unexpected recipient: %s", got.RecvAddress)
	}
	if got.Amount.String() != "1.32" {
		t.Fatalf("expected amount 1.32, got %s", got.Amount)
	}
}

func TestSolanaParseTransferRecognizesReportedMultisigUSDCTransaction(t *testing.T) {
	accountKeys := []string{
		"BhJuSLM8WzkM71umK4UQXXRfVB4ZoHbaDsEQercc2eMX",
		"23PXKLkUNQ85LScKDZVWhHLzFppYiSVky2Z7iqkk4JFu",
		"8w7MbgKz4mRu2Ku9KS2JQ519mZfX4fgiN6qrjdEuAG8H",
		"ComputeBudget111111111111111111111111111111",
		"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
		"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA",
	}
	tokenAccounts := map[string]solanaTokenOwner{
		accountKeys[1]: {
			TradeType: model.UsdcSolana,
			Address:   "DAzEQJ8TzdmrAgphrcGGZie4fwiXmRYXRCKX4wQh2oLf",
		},
		accountKeys[2]: {
			TradeType: model.UsdcSolana,
			Address:   "BhJuSLM8WzkM71umK4UQXXRfVB4ZoHbaDsEQercc2eMX",
		},
	}
	instruction := gjson.Parse(`{
		"accounts":[2,4,1,0,0],
		"data":"hwaXrj5gCKML9",
		"programIdIndex":5
	}`)

	s := newSolana()
	got := s.parseTransfer(instruction, accountKeys, tokenAccounts)

	if got.TradeType != model.UsdcSolana {
		t.Fatalf("expected trade type %s, got %s", model.UsdcSolana, got.TradeType)
	}
	if got.FromAddress != "BhJuSLM8WzkM71umK4UQXXRfVB4ZoHbaDsEQercc2eMX" {
		t.Fatalf("unexpected sender: %s", got.FromAddress)
	}
	if got.RecvAddress != "DAzEQJ8TzdmrAgphrcGGZie4fwiXmRYXRCKX4wQh2oLf" {
		t.Fatalf("unexpected recipient: %s", got.RecvAddress)
	}
	if got.Amount.String() != "2.65" {
		t.Fatalf("expected amount 2.65, got %s", got.Amount)
	}
}

func TestGetConfirmingOrdersKeepsOnTimePaymentAfterOrderExpires(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "confirming-after-expire.db")
	if err := model.Init(dbPath, "", ""); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	t.Cleanup(model.Close)

	createdAt := model.Datetime(time.Now().Add(-2 * time.Hour))
	confirmedAt := time.Now().Add(-90 * time.Minute)
	expiredAt := time.Now().Add(-1 * time.Hour)
	order := model.Order{
		OrderId:       "late-finalized",
		TradeId:       "late-finalized-trade",
		TradeType:     model.UsdcSolana,
		Crypto:        model.USDC,
		Amount:        "13.19",
		Money:         "100",
		Address:       testSolanaOwner,
		Status:        model.OrderStatusConfirming,
		RefHash:       testSolanaTxHash,
		RefBlockNum:   424729879,
		NotifyUrl:     "https://example.com/webhook",
		ConfirmedAt:   &confirmedAt,
		ExpiredAt:     expiredAt,
		AddressLocked: false,
		AutoTimeAt: model.AutoTimeAt{
			CreatedAt: &createdAt,
			UpdatedAt: &createdAt,
		},
	}
	if err := model.Db.Create(&order).Error; err != nil {
		t.Fatalf("create order: %v", err)
	}

	orders := getConfirmingOrders([]model.TradeType{model.UsdcSolana})
	if len(orders) != 1 {
		t.Fatalf("expected confirming order to remain eligible, got %d", len(orders))
	}

	var refreshed model.Order
	if err := model.Db.First(&refreshed, order.ID).Error; err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if refreshed.Status != model.OrderStatusConfirming {
		t.Fatalf("expected status %d, got %d", model.OrderStatusConfirming, refreshed.Status)
	}
}
