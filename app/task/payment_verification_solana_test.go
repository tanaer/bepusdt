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
	if overrides != nil && overrides["owner"] != "" {
		receiver = overrides["owner"]
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
					{"accountIndex":2,"mint":"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v","owner":"BhJuSLM8WzkM71umK4UQXXRfVB4ZoHbaDsEQercc2eMX","programId":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"},
					{"accountIndex":3,"mint":"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v","owner":"%s","programId":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"}
				],
				"postTokenBalances":[
					{"accountIndex":2,"mint":"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v","owner":"BhJuSLM8WzkM71umK4UQXXRfVB4ZoHbaDsEQercc2eMX","programId":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"},
					{"accountIndex":3,"mint":"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v","owner":"%s","programId":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"}
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
						"mint":"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
						"tokenAmount":{"amount":"2650000","decimals":6,"uiAmountString":"2.65"}
					}}
				}]
			}}
		}
	}`, blockTime.Unix(), errJSON, receiver, receiver)
}
