package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/v03413/bepusdt/app/model"
)

const submittedSolanaSignature = "2gqC3gYGfdNMQkF5xhZXmofbjuu3RbdZZrKz7pYvuArMpqgHSvvrQb25AuDVvtsswxkfjWZbDDouH1FBFWwgYkD4"

func TestNormalizeSubmittedPaymentHashSolana(t *testing.T) {
	normalized, err := normalizeSubmittedPaymentHash(model.UsdcSolana, " \t"+submittedSolanaSignature+"\n")
	if err != nil {
		t.Fatalf("normalize Solana signature: %v", err)
	}
	if normalized != submittedSolanaSignature {
		t.Fatalf("normalized signature = %q, want original %q", normalized, submittedSolanaSignature)
	}

	for _, invalid := range []string{
		"0" + submittedSolanaSignature[1:],
		strings.Repeat("1", 63),
		"not base58!",
	} {
		if _, err := normalizeSubmittedPaymentHash(model.UsdcSolana, invalid); !errors.Is(err, ErrInvalidSubmittedPaymentHash) {
			t.Fatalf("normalize invalid Solana signature %q error = %v, want ErrInvalidSubmittedPaymentHash", invalid, err)
		}
	}

	caseVariant := "2gqC3gYGfdNMQkF5xhZXmofbjuu3RbdZZrKz7pYvuArMpqgHSvvrQb25AuDVvtsswxkfjWZbDDouH1FBFWwgYkD5"
	if sameSubmittedPaymentHash(model.UsdcSolana, submittedSolanaSignature, caseVariant) {
		t.Fatal("different Solana signatures must not compare equal")
	}
}

func TestVerifySubmittedPaymentSolana(t *testing.T) {
	blockTime := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	order := newSubmittedSolanaOrder(blockTime)

	t.Run("fetches finalized parsed transfer", func(t *testing.T) {
		server := newSubmittedSolanaRPCServer(t, blockTime, submittedSolanaTransactionJSON(blockTime, nil, nil))
		defer server.Close()
		setSubmittedSolanaEndpoint(t, server)

		payment, err := VerifySubmittedPayment(context.Background(), order, " "+submittedSolanaSignature+" ")
		if err != nil {
			t.Fatalf("verify submitted Solana payment: %v", err)
		}
		if payment.TxHash != submittedSolanaSignature {
			t.Fatalf("payment hash = %q, want %q", payment.TxHash, submittedSolanaSignature)
		}
		if payment.TradeType != model.UsdcSolana || !payment.Amount.Equal(decimal.RequireFromString("2.65")) {
			t.Fatalf("unexpected payment: %+v", payment)
		}
		if payment.FromAddress != "BhJuSLM8WzkM71umK4UQXXRfVB4ZoHbaDsEQercc2eMX" || payment.RecvAddress != testSolanaOwner {
			t.Fatalf("unexpected transfer endpoints: %+v", payment)
		}
		if payment.BlockNum != 436683137 || !payment.Timestamp.Equal(blockTime) {
			t.Fatalf("unexpected block metadata: %+v", payment)
		}
	})

	t.Run("rejects absent or failed transaction", func(t *testing.T) {
		for name, response := range map[string]string{
			"absent": `{"jsonrpc":"2.0","id":1,"result":null}`,
			"failed": submittedSolanaTransactionJSON(blockTime, "CustomError", nil),
		} {
			t.Run(name, func(t *testing.T) {
				server := newSubmittedSolanaRPCServer(t, blockTime, response)
				defer server.Close()
				setSubmittedSolanaEndpoint(t, server)

				_, err := VerifySubmittedPayment(context.Background(), order, submittedSolanaSignature)
				if !errors.Is(err, ErrSubmittedPaymentNotFound) {
					t.Fatalf("verify %s transaction error = %v, want ErrSubmittedPaymentNotFound", name, err)
				}
			})
		}
	})

	t.Run("rejects a transaction that does not match the order", func(t *testing.T) {
		server := newSubmittedSolanaRPCServer(t, blockTime, submittedSolanaTransactionJSON(blockTime, nil, map[string]string{"owner": "11111111111111111111111111111111"}))
		defer server.Close()
		setSubmittedSolanaEndpoint(t, server)

		_, err := VerifySubmittedPayment(context.Background(), order, submittedSolanaSignature)
		if !errors.Is(err, ErrSubmittedPaymentDoesNotMatch) {
			t.Fatalf("verify mismatched transaction error = %v, want ErrSubmittedPaymentDoesNotMatch", err)
		}
	})

	t.Run("selects a matching inner transfer and uses token account owners", func(t *testing.T) {
		server := newSubmittedSolanaRPCServer(t, blockTime, submittedSolanaMultiTransferTransactionJSON(blockTime))
		defer server.Close()
		setSubmittedSolanaEndpoint(t, server)

		payment, err := VerifySubmittedPayment(context.Background(), order, submittedSolanaSignature)
		if err != nil {
			t.Fatalf("verify matching inner Solana transfer: %v", err)
		}
		if payment.FromAddress != "BhJuSLM8WzkM71umK4UQXXRfVB4ZoHbaDsEQercc2eMX" {
			t.Fatalf("payer = %q, want source token account owner", payment.FromAddress)
		}
		if payment.RecvAddress != testSolanaOwner {
			t.Fatalf("recipient = %q, want matching token account owner", payment.RecvAddress)
		}
	})

	t.Run("rejects incorrect mint amount or time window", func(t *testing.T) {
		cases := []struct {
			name     string
			response string
		}{
			{
				name:     "wrong mint",
				response: submittedSolanaTransactionJSON(blockTime, nil, map[string]string{"mint": "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"}),
			},
			{
				name:     "wrong amount",
				response: submittedSolanaTransactionJSON(blockTime, nil, map[string]string{"amount": "2640000"}),
			},
			{
				name:     "outside order window",
				response: submittedSolanaTransactionJSON(order.ExpiredAt.Add(time.Second), nil, nil),
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				server := newSubmittedSolanaRPCServer(t, blockTime, tc.response)
				defer server.Close()
				setSubmittedSolanaEndpoint(t, server)

				_, err := VerifySubmittedPayment(context.Background(), order, submittedSolanaSignature)
				if !errors.Is(err, ErrSubmittedPaymentDoesNotMatch) {
					t.Fatalf("verify %s transaction error = %v, want ErrSubmittedPaymentDoesNotMatch", tc.name, err)
				}
			})
		}
	})
}

func TestSolanaReconcileAndManualVerificationClaimOneOrder(t *testing.T) {
	initSolanaReconcileTestLog(t)

	if err := model.Init(filepath.Join(t.TempDir(), "solana-claim-race.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}
	t.Cleanup(model.Close)

	blockTime := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	response := submittedSolanaTransactionJSON(blockTime, nil, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()

		var request struct {
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode Solana RPC request: %v", err)
		}
		switch request.Method {
		case "getSignaturesForAddress":
			_, _ = w.Write([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":[{"signature":"%s","slot":436683137,"err":null,"blockTime":%d}]}`, submittedSolanaSignature, blockTime.Unix())))
		case "getTransaction":
			_, _ = w.Write([]byte(response))
		default:
			t.Fatalf("unexpected Solana RPC method: %s", request.Method)
		}
	}))
	defer server.Close()
	model.SetK(model.RpcEndpointSolana, server.URL)

	manualOrder := newSubmittedSolanaOrder(blockTime)
	manualOrder.OrderId = "manual-race-order"
	manualOrder.TradeId = "manual-race-trade"
	reconcileOrder := newSubmittedSolanaOrder(blockTime)
	reconcileOrder.OrderId = "reconcile-race-order"
	reconcileOrder.TradeId = "reconcile-race-trade"
	if err := model.Db.Create(&manualOrder).Error; err != nil {
		t.Fatalf("create manual order: %v", err)
	}
	if err := model.Db.Create(&reconcileOrder).Error; err != nil {
		t.Fatalf("create reconcile order: %v", err)
	}

	previousSol := sol
	sol = newSolana()
	sol.client = server.Client()
	t.Cleanup(func() { sol = previousSol })
	reconciler := newSolana()
	reconciler.client = server.Client()

	start := make(chan struct{})
	manualResult := make(chan error, 1)
	reconcileResult := make(chan bool, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, err := VerifyAndClaimSubmittedPayment(context.Background(), &manualOrder, submittedSolanaSignature)
		manualResult <- err
	}()
	go func() {
		defer wg.Done()
		<-start
		reconcileResult <- reconciler.reconcileOrderTokenAccount(context.Background(), reconcileOrder, testSolanaTokenWallet)
	}()
	close(start)
	wg.Wait()

	manualErr := <-manualResult
	reconciled := <-reconcileResult
	if manualErr != nil {
		if !errors.Is(manualErr, model.ErrPaymentHashAlreadyClaimed) {
			t.Fatalf("manual claim error = %v, want the competing-hash error", manualErr)
		}
		if !reconciled {
			t.Fatalf("manual claim lost the hash but reconciliation did not claim it")
		}
	}
	if manualErr == nil && reconciled {
		t.Fatal("both claim entry points reported success for the same Solana signature")
	}

	var claimCount int64
	if err := model.Db.Model(&model.PaymentHashClaim{}).Count(&claimCount).Error; err != nil {
		t.Fatalf("count payment claims: %v", err)
	}
	if claimCount != 1 {
		t.Fatalf("payment claim count = %d, want exactly one", claimCount)
	}
	var confirmingCount int64
	if err := model.Db.Model(&model.Order{}).Where("status = ?", model.OrderStatusConfirming).Count(&confirmingCount).Error; err != nil {
		t.Fatalf("count confirming orders: %v", err)
	}
	if confirmingCount != 1 {
		t.Fatalf("confirming orders = %d, want exactly one", confirmingCount)
	}
}

func newSubmittedSolanaOrder(blockTime time.Time) model.Order {
	createdAt := model.Datetime(blockTime.Add(-time.Minute))
	return model.Order{
		OrderId:      "submitted-solana-order",
		TradeId:      "submitted-solana-trade",
		TradeType:    model.UsdcSolana,
		Fiat:         model.CNY,
		Crypto:       model.USDC,
		Rate:         "7.00",
		Amount:       "2.65",
		Money:        "18.55",
		Address:      testSolanaOwner,
		MatchAddress: testSolanaOwner,
		Status:       model.OrderStatusWaiting,
		ApiType:      model.OrderApiTypeEpusdt,
		ExpiredAt:    blockTime.Add(time.Minute),
		ConfirmedAt:  &blockTime,
		AutoTimeAt:   model.AutoTimeAt{CreatedAt: &createdAt, UpdatedAt: &createdAt},
	}
}

func setSubmittedSolanaEndpoint(t *testing.T, server *httptest.Server) {
	t.Helper()
	if err := model.Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}
	model.SetK(model.RpcEndpointSolana, server.URL)

	previous := sol
	sol = newSolana()
	sol.client = server.Client()
	t.Cleanup(func() { sol = previous })
}

func newSubmittedSolanaRPCServer(t *testing.T, blockTime time.Time, response string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()

		var request struct {
			Method string `json:"method"`
			Params []any  `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode Solana RPC request: %v", err)
		}
		if request.Method != "getTransaction" {
			t.Fatalf("RPC method = %q, want getTransaction", request.Method)
		}
		if len(request.Params) != 2 || request.Params[0] != submittedSolanaSignature {
			t.Fatalf("unexpected RPC params: %#v", request.Params)
		}
		options, ok := request.Params[1].(map[string]any)
		if !ok || options["encoding"] != "jsonParsed" || options["commitment"] != "finalized" || options["maxSupportedTransactionVersion"] != float64(0) {
			t.Fatalf("unexpected transaction options: %#v", request.Params[1])
		}
		_, _ = w.Write([]byte(response))
	}))
}

func submittedSolanaTransactionJSON(blockTime time.Time, transactionError any, overrides map[string]string) string {
	receiver := testSolanaOwner
	mint := "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	amount := "2650000"
	if overrides != nil && overrides["owner"] != "" {
		receiver = overrides["owner"]
	}
	if overrides != nil && overrides["mint"] != "" {
		mint = overrides["mint"]
	}
	if overrides != nil && overrides["amount"] != "" {
		amount = overrides["amount"]
	}
	errJSON := "null"
	if transactionError != nil {
		errJSON = fmt.Sprintf(`"%v"`, transactionError)
	}
	return fmt.Sprintf(`{
		"jsonrpc":"2.0",
		"id":1,
		"result":{
			"slot":436683137,
			"blockTime":%d,
			"meta":{
				"err":%s,
				"preTokenBalances":[
					{"accountIndex":2,"mint":"%s","owner":"BhJuSLM8WzkM71umK4UQXXRfVB4ZoHbaDsEQercc2eMX","programId":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"},
					{"accountIndex":3,"mint":"%s","owner":"%s","programId":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"}
				],
				"postTokenBalances":[
					{"accountIndex":2,"mint":"%s","owner":"BhJuSLM8WzkM71umK4UQXXRfVB4ZoHbaDsEQercc2eMX","programId":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"},
					{"accountIndex":3,"mint":"%s","owner":"%s","programId":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"}
				],
				"innerInstructions":[]
			},
			"transaction":{"message":{
				"accountKeys":[
					{"pubkey":"BhJuSLM8WzkM71umK4UQXXRfVB4ZoHbaDsEQercc2eMX"},
					{"pubkey":"8w7MbgKz4mRu2Ku9KS2JQ519mZfX4fgiN6qrjdEuAG8H"},
					{"pubkey":"7KJjY7rArbydeLBF7gQ5LdqXRKRYyPArT99NEctsHsgU"},
					{"pubkey":"23PXKLkUNQ85LScKDZVWhHLzFppYiSVky2Z7iqkk4JFu"}
				],
				"instructions":[{
					"program":"spl-token",
					"programId":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA",
					"parsed":{"type":"transferChecked","info":{
						"multisigAuthority":"BhJuSLM8WzkM71umK4UQXXRfVB4ZoHbaDsEQercc2eMX",
						"source":"7KJjY7rArbydeLBF7gQ5LdqXRKRYyPArT99NEctsHsgU",
						"destination":"23PXKLkUNQ85LScKDZVWhHLzFppYiSVky2Z7iqkk4JFu",
						"mint":"%s",
						"tokenAmount":{"amount":"%s","decimals":6,"uiAmountString":"2.65"}
					}}
				}]
			}}
		}
	}`, blockTime.Unix(), errJSON, mint, mint, receiver, mint, mint, receiver, mint, amount)
}

func submittedSolanaMultiTransferTransactionJSON(blockTime time.Time) string {
	const (
		sourceOwner   = "BhJuSLM8WzkM71umK4UQXXRfVB4ZoHbaDsEQercc2eMX"
		sourceAccount = "7KJjY7rArbydeLBF7gQ5LdqXRKRYyPArT99NEctsHsgU"
		wrongAccount  = "8w7MbgKz4mRu2Ku9KS2JQ519mZfX4fgiN6qrjdEuAG8H"
	)

	return fmt.Sprintf(`{
		"jsonrpc":"2.0",
		"id":1,
		"result":{
			"slot":436683137,
			"blockTime":%d,
			"meta":{
				"err":null,
				"preTokenBalances":[
					{"accountIndex":1,"mint":"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v","owner":"%s","programId":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"},
					{"accountIndex":2,"mint":"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v","owner":"11111111111111111111111111111111","programId":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"},
					{"accountIndex":3,"mint":"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v","owner":"%s","programId":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"}
				],
				"postTokenBalances":[],
				"innerInstructions":[{"index":0,"instructions":[{
					"program":"spl-token",
					"programId":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA",
					"parsed":{"type":"transferChecked","info":{
						"multisigAuthority":"%s",
						"source":"%s",
						"destination":"%s",
						"mint":"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
						"tokenAmount":{"amount":"2650000","decimals":6}
					}}
				}]}]
			},
			"transaction":{"message":{
				"accountKeys":[
					{"pubkey":"%s"},
					{"pubkey":"%s"},
					{"pubkey":"%s"},
					{"pubkey":"%s"}
				],
				"instructions":[{
					"program":"spl-token",
					"programId":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA",
					"parsed":{"type":"transferChecked","info":{
						"authority":"%s",
						"source":"%s",
						"destination":"%s",
						"mint":"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
						"tokenAmount":{"amount":"2650000","decimals":6}
					}}
				}]
			}}
		}
	}`, blockTime.Unix(), sourceOwner, testSolanaOwner, testSolanaSender, sourceAccount, testSolanaTokenWallet, testSolanaSender, sourceAccount, wrongAccount, testSolanaTokenWallet, testSolanaSender, sourceAccount, wrongAccount)
}
