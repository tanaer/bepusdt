package task

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/spf13/cast"
	"github.com/tidwall/gjson"
	"github.com/v03413/bepusdt/app/conf"
	"github.com/v03413/bepusdt/app/model"
	"github.com/v03413/bepusdt/app/utils"
	"github.com/v03413/tronprotocol/api"
	"github.com/v03413/tronprotocol/core"
)

var (
	ErrInvalidSubmittedPaymentHash  = errors.New("invalid submitted payment hash")
	ErrUnsupportedSubmittedPayment  = errors.New("submitted payment verification is unsupported for this network")
	ErrSubmittedPaymentNotFound     = errors.New("submitted payment transaction was not found or is not finalized")
	ErrSubmittedPaymentDoesNotMatch = errors.New("submitted payment does not match this order")
)

type submittedPaymentLookup func(context.Context, model.Order, string) (transfer, error)

var tronPaymentHashPattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
var evmPaymentHashPattern = regexp.MustCompile(`^(0x)?[0-9a-fA-F]{64}$`)

// VerifySubmittedPayment fetches a transaction from the order's chain and
// verifies it using the exact same matching rules as the scanner. The caller
// is responsible for atomically claiming the verified transaction hash.
func VerifySubmittedPayment(ctx context.Context, order model.Order, hash string) (transfer, error) {
	return verifySubmittedPayment(ctx, order, hash, lookupSubmittedPayment)
}

// VerifyAndClaimSubmittedPayment verifies a payer-supplied transaction hash and
// atomically reserves it for this order before moving it to confirming.
func VerifyAndClaimSubmittedPayment(ctx context.Context, order *model.Order, hash string) (bool, error) {
	if order == nil {
		return false, fmt.Errorf("verify submitted payment: order is required")
	}
	payment, err := VerifySubmittedPayment(ctx, *order, hash)
	if err != nil {
		return false, err
	}
	return model.ClaimPaymentConfirmation(order, model.PaymentConfirmation{
		BlockNum: payment.BlockNum,
		From:     payment.FromAddress,
		Hash:     payment.TxHash,
		At:       payment.Timestamp,
		Amount:   payment.Amount,
	})
}

func verifySubmittedPayment(ctx context.Context, order model.Order, hash string, lookup submittedPaymentLookup) (transfer, error) {
	normalizedHash, err := normalizeSubmittedPaymentHash(order.TradeType, hash)
	if err != nil {
		return transfer{}, err
	}

	payment, err := lookup(ctx, order, normalizedHash)
	if err != nil {
		return transfer{}, err
	}
	if !sameSubmittedPaymentHash(order.TradeType, payment.TxHash, normalizedHash) {
		return transfer{}, fmt.Errorf("%w: lookup returned a different transaction", ErrSubmittedPaymentDoesNotMatch)
	}
	if reason := orderTransferMatchReason(order, payment); reason != orderMatchOK {
		return transfer{}, fmt.Errorf("%w: %s", ErrSubmittedPaymentDoesNotMatch, reason)
	}

	payment.TxHash = normalizedHash
	return payment, nil
}

func normalizeSubmittedPaymentHash(tradeType model.TradeType, hash string) (string, error) {
	hash = strings.TrimSpace(hash)
	switch {
	case isTronTrade(tradeType):
		hash = strings.TrimPrefix(strings.ToLower(hash), "0x")
		if !tronPaymentHashPattern.MatchString(hash) {
			return "", ErrInvalidSubmittedPaymentHash
		}
		return hash, nil
	case isBscTrade(tradeType):
		if !evmPaymentHashPattern.MatchString(hash) {
			return "", ErrInvalidSubmittedPaymentHash
		}
		return "0x" + strings.TrimPrefix(strings.ToLower(hash), "0x"), nil
	default:
		return "", ErrUnsupportedSubmittedPayment
	}
}

func sameSubmittedPaymentHash(tradeType model.TradeType, left, right string) bool {
	normalizedLeft, leftErr := normalizeSubmittedPaymentHash(tradeType, left)
	normalizedRight, rightErr := normalizeSubmittedPaymentHash(tradeType, right)
	return leftErr == nil && rightErr == nil && normalizedLeft == normalizedRight
}

func isTronTrade(tradeType model.TradeType) bool {
	return tradeType == model.TronTrx || tradeType == model.UsdtTrc20 || tradeType == model.UsdcTrc20
}

func isBscTrade(tradeType model.TradeType) bool {
	return tradeType == model.BscBnb || tradeType == model.UsdtBep20 || tradeType == model.UsdcBep20
}

func isSolanaTrade(tradeType model.TradeType) bool {
	return tradeType == model.UsdtSolana || tradeType == model.UsdcSolana
}

// SupportsSubmittedPaymentVerification reports whether the service can fetch
// and validate a user-submitted transaction hash for a payment type. Keep the
// cashier UI and HTTP endpoint on this single, explicit allowlist.
func SupportsSubmittedPaymentVerification(tradeType model.TradeType) bool {
	return isTronTrade(tradeType) || isBscTrade(tradeType) || isSolanaTrade(tradeType)
}

func lookupSubmittedPayment(ctx context.Context, order model.Order, hash string) (transfer, error) {
	switch {
	case isTronTrade(order.TradeType):
		return lookupTronSubmittedPayment(ctx, order, hash)
	case isBscTrade(order.TradeType):
		return lookupBscSubmittedPayment(ctx, order, hash)
	default:
		return transfer{}, ErrUnsupportedSubmittedPayment
	}
}

func lookupTronSubmittedPayment(ctx context.Context, order model.Order, hash string) (transfer, error) {
	id, err := hex.DecodeString(hash)
	if err != nil {
		return transfer{}, ErrInvalidSubmittedPaymentHash
	}
	conn, err := tr.client()
	if err != nil {
		return transfer{}, fmt.Errorf("query TRON transaction: %w", err)
	}
	client := api.NewWalletClient(conn)
	queryCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	transaction, err := client.GetTransactionById(queryCtx, &api.BytesMessage{Value: id})
	if err != nil {
		reportRPCFailure(conf.Tron, model.Endpoint(conf.Tron), err)
		return transfer{}, fmt.Errorf("query TRON transaction: %w", err)
	}
	info, err := client.GetTransactionInfoById(queryCtx, &api.BytesMessage{Value: id})
	if err != nil {
		reportRPCFailure(conf.Tron, model.Endpoint(conf.Tron), err)
		return transfer{}, fmt.Errorf("query TRON transaction receipt: %w", err)
	}
	if info.GetBlockNumber() <= 0 || info.GetBlockTimeStamp() <= 0 || info.GetReceipt() == nil ||
		info.GetReceipt().GetResult() != core.Transaction_Result_SUCCESS {
		return transfer{}, ErrSubmittedPaymentNotFound
	}

	candidates := tr.parseSubmittedTransaction(hash, transaction, time.UnixMilli(info.GetBlockTimeStamp()), int(info.GetBlockNumber()))
	for _, payment := range candidates {
		if payment.TradeType == order.TradeType {
			return payment, nil
		}
	}

	return transfer{}, ErrSubmittedPaymentDoesNotMatch
}

func (t *tron) parseSubmittedTransaction(hash string, transaction *core.Transaction, timestamp time.Time, blockNum int) []transfer {
	transfers := make([]transfer, 0, 1)
	for _, contract := range transaction.GetRawData().GetContract() {
		switch contract.GetType() {
		case core.Transaction_Contract_TransferContract:
			item := &core.TransferContract{}
			if err := contract.GetParameter().UnmarshalTo(item); err != nil {
				continue
			}
			transfers = append(transfers, transfer{
				Network:     conf.Tron,
				TxHash:      hash,
				Amount:      decimal.NewFromBigInt(big.NewInt(item.Amount), -6),
				FromAddress: t.base58CheckEncode(item.OwnerAddress),
				RecvAddress: t.base58CheckEncode(item.ToAddress),
				Timestamp:   timestamp,
				TradeType:   model.TronTrx,
				BlockNum:    blockNum,
			})
		case core.Transaction_Contract_TriggerSmartContract:
			item := &core.TriggerSmartContract{}
			if err := contract.GetParameter().UnmarshalTo(item); err != nil {
				continue
			}
			data := item.GetData()
			if len(data) < 4 {
				continue
			}

			if bytes.Equal(item.OwnerAddress, gasFreeOwnerAddress) && bytes.Equal(item.ContractAddress, gasFreeContractAddress) {
				from, receiver, amount := t.gasFreePermitTransfer(data)
				if amount != nil {
					transfers = append(transfers, transfer{
						Network: conf.Tron, TxHash: hash, Amount: decimal.NewFromBigInt(amount, conf.UsdtTronDecimals),
						FromAddress: from, RecvAddress: receiver, Timestamp: timestamp, TradeType: model.UsdtTrc20, BlockNum: blockNum,
					})
				}
			}

			tradeType, ok := tronContractTradeType(item.GetContractAddress())
			if !ok {
				continue
			}
			if bytes.Equal(data[:4], []byte{0xa9, 0x05, 0x9c, 0xbb}) {
				receiver, amount := t.parseTrc20ContractTransfer(data)
				if amount != nil {
					transfers = append(transfers, transfer{
						Network: conf.Tron, TxHash: hash, Amount: decimal.NewFromBigInt(amount, model.GetTradeDecimal(tradeType)),
						FromAddress: t.base58CheckEncode(item.OwnerAddress), RecvAddress: receiver, Timestamp: timestamp, TradeType: tradeType, BlockNum: blockNum,
					})
				}
			}
			if bytes.Equal(data[:4], []byte{0x23, 0xb8, 0x72, 0xdd}) {
				from, receiver, amount := t.parseTrc20ContractTransferFrom(data)
				if amount != nil {
					transfers = append(transfers, transfer{
						Network: conf.Tron, TxHash: hash, Amount: decimal.NewFromBigInt(amount, model.GetTradeDecimal(tradeType)),
						FromAddress: from, RecvAddress: receiver, Timestamp: timestamp, TradeType: tradeType, BlockNum: blockNum,
					})
				}
			}
		}
	}
	return transfers
}

func tronContractTradeType(address []byte) (model.TradeType, bool) {
	switch {
	case bytes.Equal(address, usdtTrc20ContractAddress):
		return model.UsdtTrc20, true
	case bytes.Equal(address, usdcTrc20ContractAddress):
		return model.UsdcTrc20, true
	default:
		return "", false
	}
}

func lookupBscSubmittedPayment(ctx context.Context, order model.Order, hash string) (transfer, error) {
	e := evm{Network: conf.Bsc, Client: utils.NewHttpClient()}
	receipt, err := e.submittedRPC(ctx, "eth_getTransactionReceipt", fmt.Sprintf(`[%q]`, hash))
	if err != nil {
		return transfer{}, err
	}
	if !strings.EqualFold(receipt.Get("status").String(), "0x1") {
		return transfer{}, ErrSubmittedPaymentNotFound
	}
	blockNumber := receipt.Get("blockNumber").String()
	if blockNumber == "" {
		return transfer{}, ErrSubmittedPaymentNotFound
	}
	block, err := e.submittedRPC(ctx, "eth_getBlockByNumber", fmt.Sprintf(`[%q,false]`, blockNumber))
	if err != nil {
		return transfer{}, err
	}
	timestamp := time.Unix(utils.HexStr2Int(block.Get("timestamp").String()).Int64(), 0)
	blockNum := cast.ToInt(utils.HexStr2Int(blockNumber).Int64())
	if timestamp.IsZero() || blockNum <= 0 {
		return transfer{}, ErrSubmittedPaymentNotFound
	}

	if order.TradeType == model.BscBnb {
		transaction, err := e.submittedRPC(ctx, "eth_getTransactionByHash", fmt.Sprintf(`[%q]`, hash))
		if err != nil {
			return transfer{}, err
		}
		if transaction.Get("input").String() != "0x" || transaction.Get("to").String() == "" {
			return transfer{}, ErrSubmittedPaymentDoesNotMatch
		}
		amount, ok := new(big.Int).SetString(strings.TrimPrefix(transaction.Get("value").String(), "0x"), 16)
		if !ok || amount.Sign() <= 0 {
			return transfer{}, ErrSubmittedPaymentDoesNotMatch
		}
		return transfer{
			Network: conf.Bsc, TxHash: hash, Amount: decimal.NewFromBigInt(amount, conf.BscBnbDecimals),
			FromAddress: transaction.Get("from").String(), RecvAddress: transaction.Get("to").String(), Timestamp: timestamp,
			TradeType: model.BscBnb, BlockNum: blockNum,
		}, nil
	}

	for _, item := range receipt.Get("logs").Array() {
		tradeType, ok := model.GetContractTrade(strings.ToLower(item.Get("address").String()))
		if !ok || tradeType != order.TradeType {
			continue
		}
		topics := item.Get("topics").Array()
		if len(topics) < 3 || !strings.EqualFold(topics[0].String(), evmTransferEvent) {
			continue
		}
		from, fromOK := evmTopicAddress(topics[1].String())
		recipient, recipientOK := evmTopicAddress(topics[2].String())
		amount, amountOK := new(big.Int).SetString(strings.TrimPrefix(item.Get("data").String(), "0x"), 16)
		if !fromOK || !recipientOK || !amountOK || amount.Sign() <= 0 {
			continue
		}
		return transfer{
			Network: conf.Bsc, TxHash: hash, Amount: decimal.NewFromBigInt(amount, model.GetTradeDecimal(tradeType)),
			FromAddress: from, RecvAddress: recipient, Timestamp: timestamp, TradeType: tradeType, BlockNum: blockNum,
		}, nil
	}

	return transfer{}, ErrSubmittedPaymentDoesNotMatch
}

func (e evm) submittedRPC(ctx context.Context, method, params string) (gjson.Result, error) {
	body, _, err := doRPCRequestWithFailover(ctx, e.Client, e.Network, func(endpoint string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(
			fmt.Sprintf(`{"jsonrpc":"2.0","method":%q,"params":%s,"id":1}`, method, params),
		))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	}, func(body []byte) error {
		data := gjson.ParseBytes(body)
		if data.Get("error").Exists() {
			return errors.New(data.Get("error").String())
		}
		if !data.Get("result").Exists() {
			return ErrSubmittedPaymentNotFound
		}
		return nil
	})
	if err != nil {
		return gjson.Result{}, fmt.Errorf("query BSC transaction: %w", err)
	}
	return gjson.ParseBytes(body).Get("result"), nil
}

func evmTopicAddress(topic string) (string, bool) {
	topic = strings.TrimPrefix(strings.ToLower(topic), "0x")
	if len(topic) != 64 || !tronPaymentHashPattern.MatchString(topic) {
		return "", false
	}
	return "0x" + topic[24:], true
}
