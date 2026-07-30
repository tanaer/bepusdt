package task

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/panjf2000/ants/v2"
	"github.com/shopspring/decimal"
	"github.com/smallnest/chanx"
	"github.com/spf13/cast"
	"github.com/tidwall/gjson"
	"github.com/v03413/bepusdt/app/conf"
	blockapi "github.com/v03413/bepusdt/app/core"
	"github.com/v03413/bepusdt/app/log"
	"github.com/v03413/bepusdt/app/model"
	"github.com/v03413/bepusdt/app/utils"
)

const (
	blockParseMaxNum = 10 // 每次解析区块的最大数量
	evmTransferEvent = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"
)

var chainBlockNum sync.Map

type block struct {
	RollDelayOffset int64 // 延迟偏移量，某些RPC节点如果不延迟，会报错 block is out of range，目前发现 https://rpc.xlayer.tech/ 存在此问题
	ConfirmedOffset int   // 确认偏移量，开启交易确认后，区块高度需要减去此值认为交易已确认
}

type evmNative struct {
	Parse     bool
	Decimal   int32
	TradeType model.TradeType
}

type evm struct {
	Network          string
	Block            block
	Native           evmNative
	Client           *http.Client
	blockScanQueue   *chanx.UnboundedChan[evmBlock]
	LookbackInterval time.Duration // 回溯时每批入队的间隔，控制 RPC 调用速率；默认 500ms
}

type evmBlock struct {
	From int64
	To   int64
}

func (e *evm) syncBlocksForward(ctx context.Context) {
	if syncBreak(e.Network, e.blockScanQueue.Len()) {

		return
	}

	now, err := e.latestBlockNumber(ctx)
	if err != nil {
		log.Task.Warn("Error sending request:", err)

		return
	}
	if now <= 0 {

		return
	}

	var lastBlockNumber int64
	if v, ok := chainBlockNum.Load(e.Network); ok {
		lastBlockNumber = v.(int64)
	}

	if now-lastBlockNumber > cast.ToInt64(model.GetC(model.BlockHeightMaxDiff)) {

		lastBlockNumber = now - 1
	}

	chainBlockNum.Store(e.Network, now)
	model.SetChainProgress(model.Network(e.Network), int(now))
	if now <= lastBlockNumber {

		return
	}

	for from := lastBlockNumber + 1; from <= now; from += blockParseMaxNum {
		to := from + blockParseMaxNum - 1
		if to > now {
			to = now
		}

		e.blockScanQueue.In <- evmBlock{From: from, To: to}
	}
}

func (e *evm) latestBlockNumber(ctx context.Context) (int64, error) {
	post := []byte(`{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}`)
	body, _, err := doRPCRequestWithFailover(ctx, e.Client, e.Network, func(endpoint string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBuffer(post))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	}, func(body []byte) error {
		var res = gjson.ParseBytes(body)
		if !res.IsObject() {
			return fmt.Errorf("invalid rpc body: %s", string(body))
		}
		if data := res.Get("error"); data.Exists() {
			return errors.New(data.String())
		}
		return nil
	})
	if err != nil {
		return 0, err
	}

	res := gjson.ParseBytes(body)
	return utils.HexStr2Int(res.Get("result").String()).Int64() - e.Block.RollDelayOffset, nil
}

func (e *evm) lookbackBlocks(ctx context.Context) {
	if syncBreak(e.Network, e.blockScanQueue.Len()) {
		return
	}

	startAt, endAt, ok := getLookbackUnix(model.Network(e.Network))
	if !ok {
		return
	}

	interval := e.LookbackInterval
	if interval <= 0 {
		interval = time.Millisecond * 300
	}

	start, end := blockapi.New().GetBoundaryHeights(startAt, endAt, e.Network)
	for i := start; i <= end; i += blockParseMaxNum {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if syncBreak(e.Network, e.blockScanQueue.Len()) {
			return
		}
		to := i + blockParseMaxNum - 1
		if to > end {
			to = end
		}
		e.blockScanQueue.In <- evmBlock{From: i, To: to}
		time.Sleep(interval)
	}
}

func (e *evm) blockDispatch(ctx context.Context) {
	p, err := ants.NewPoolWithFunc(3, e.getBlockByNumber)
	if err != nil {
		log.Task.Warn("Error creating pool:", err)

		return
	}

	defer p.Release()

	for {
		select {
		case <-ctx.Done():
			return
		case n := <-e.blockScanQueue.Out:
			if err := p.Invoke(n); err != nil {
				e.blockScanQueue.In <- n

				log.Task.Warn("Evm Block Dispatch Error invoking process block:", err)
			}
		}
	}
}

func (e *evm) getBlockByNumber(a any) {
	b, ok := a.(evmBlock)
	if !ok {
		log.Task.Warn("Evm Block Parse Error: expected []int64, got", a)

		return
	}

	items := make([]string, 0)
	for i := b.From; i <= b.To; i++ {
		items = append(items, fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x%x",%t],"id":%d}`, i, e.Native.Parse, i))
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	body, _, err := doRPCRequestWithFailover(ctx, e.Client, e.Network, func(endpoint string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBuffer([]byte(fmt.Sprintf(`[%s]`, strings.Join(items, ",")))))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	}, func(body []byte) error {
		for _, itm := range gjson.ParseBytes(body).Array() {
			if itm.Get("error").Exists() {
				return errors.New(itm.Get("error").String())
			}
		}
		return nil
	})
	if err != nil {
		e.blockScanQueue.In <- b
		log.Task.Warn("eth_getBlockByNumber Error:", err)

		return
	}

	nativeTransfers := make([]transfer, 0)
	blockTimestamp := make(map[string]time.Time)
	for _, itm := range gjson.ParseBytes(body).Array() {
		timestamp := utils.HexStr2Int(itm.Get("result.timestamp").String()).Int64()
		blockTime := time.Unix(timestamp, 0)
		blockNumHex := itm.Get("result.number").String()
		blockTimestamp[blockNumHex] = blockTime

		var array = itm.Get("result.transactions").Array()
		if e.Native.Parse && len(array) != 0 {

			nativeTransfers = append(nativeTransfers, e.parseNativeTransfer(array, int(utils.HexStr2Int(blockNumHex).Int64()), blockTime)...)
		}
	}

	transfers, err := e.parseEventTransfer(b, blockTimestamp)
	if err != nil {
		conf.RecordFailure(e.Network)
		e.blockScanQueue.In <- b
		log.Task.Warn("Evm Block Parse Error parsing block transfer:", err)

		return
	}

	if len(nativeTransfers) > 0 {
		transferQueue.In <- nativeTransfers
	}
	if len(transfers) > 0 {
		transferQueue.In <- transfers
	}

	log.Task.Info(fmt.Sprintf("区块扫描完成(%s): %d → %d 成功率：%s", e.Network, b.From, b.To, conf.GetSuccessRate(e.Network)))
}

func (e *evm) parseNativeTransfer(array []gjson.Result, num int, timestamp time.Time) []transfer {
	nativeTransfers := make([]transfer, 0)
	for _, tx := range array {
		if tx.Get("input").String() != "0x" {
			// 非原生币交易

			continue
		}

		valStr := tx.Get("value").String()
		if valStr == "0x0" || len(valStr) < 3 {
			// 过滤 0 值交易

			continue
		}

		amount, ok := big.NewInt(0).SetString(valStr[2:], 16)
		if !ok || amount.Sign() <= 0 {

			continue
		}

		toAddress := tx.Get("to").String()
		if toAddress == "" { // 合约创建交易 to 为空

			continue
		}

		nativeTransfers = append(nativeTransfers, transfer{
			Network:     e.Network,
			FromAddress: tx.Get("from").String(),
			RecvAddress: toAddress,
			Amount:      decimal.NewFromBigInt(amount, e.Native.Decimal),
			TxHash:      tx.Get("hash").String(),
			BlockNum:    num,
			Timestamp:   timestamp,
			TradeType:   e.Native.TradeType,
		})
	}

	return nativeTransfers
}

func (e *evm) parseEventTransfer(b evmBlock, timestamp map[string]time.Time) ([]transfer, error) {
	transfers := make([]transfer, 0)
	post := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[{"fromBlock":"0x%x","toBlock":"0x%x","topics":["%s"]}],"id":1}`, b.From, b.To, evmTransferEvent))
	body, _, err := doRPCRequestWithFailover(context.Background(), e.Client, e.Network, func(endpoint string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(context.Background(), "POST", endpoint, bytes.NewBuffer(post))
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
		return nil
	})
	if err != nil {

		return transfers, errors.Join(errors.New("eth_getLogs request error"), err)
	}

	data := gjson.ParseBytes(body)

	for _, itm := range data.Get("result").Array() {
		to := itm.Get("address").String()
		tradeType, ok := model.GetContractTrade(to)
		if !ok {

			continue
		}

		topics := itm.Get("topics").Array()
		if len(topics) < 3 {

			continue
		}

		if topics[0].String() != evmTransferEvent { // transfer event signature

			continue
		}

		from := fmt.Sprintf("0x%s", topics[1].String()[26:])
		recv := fmt.Sprintf("0x%s", topics[2].String()[26:])
		amount, ok := big.NewInt(0).SetString(itm.Get("data").String()[2:], 16)
		if !ok || amount.Sign() <= 0 {

			continue
		}

		transfers = append(transfers, transfer{
			Network:     e.Network,
			FromAddress: from,
			RecvAddress: recv,
			Amount:      decimal.NewFromBigInt(amount, model.GetContractDecimal(to)),
			TxHash:      itm.Get("transactionHash").String(),
			BlockNum:    cast.ToInt(itm.Get("blockNumber").String()),
			Timestamp:   timestamp[itm.Get("blockNumber").String()],
			TradeType:   tradeType,
		})
	}

	return transfers, nil
}

func (e *evm) tradeConfirmHandle(ctx context.Context) {
	var orders = getConfirmingOrders(model.GetNetworkTrades(model.Network(e.Network)))
	var wg sync.WaitGroup

	var handle = func(o model.Order) {
		if model.GetC(model.BlockOffsetConfirm) == "1" {
			last, ok := chainBlockNum.Load(e.Network)
			if !ok {
				return
			}
			if cast.ToInt(last)-o.RefBlockNum < e.Block.ConfirmedOffset {
				return
			}
		}

		post := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_getTransactionReceipt","params":["%s"],"id":1}`, o.RefHash))
		body, _, err := doRPCRequestWithFailover(ctx, e.Client, e.Network, func(endpoint string) (*http.Request, error) {
			req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBuffer(post))
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
			return nil
		})
		if err != nil {
			log.Task.Warn("evm tradeConfirmHandle Error:", err)

			return
		}

		data := gjson.ParseBytes(body)

		if data.Get("result.status").String() == "0x1" {
			markFinalConfirmed(o)
		}
	}

	for _, order := range orders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			handle(order)
		}()
	}

	wg.Wait()
}

func (e *evm) rpcEndpoint() string {

	return model.Endpoint(model.Network(e.Network))
}

func syncBreak(network string, num int) bool {
	if num >= blockQueueLimit {
		log.Task.Warn(fmt.Sprintf("%s 同步阻塞，当前区块消费堆积数量：%d", network, num))

		return true
	}

	if mqttSubscribed(network) {
		return false
	}

	trades := model.GetNetworkTrades(model.Network(network))
	if len(trades) == 0 {

		return true
	}

	var count int64
	model.Db.Model(&model.Wallet{}).
		Where("other_notify = ? and trade_type in (?)", model.WaOtherEnable, trades).
		Count(&count)
	if count > 0 {

		return false
	}

	return !hasLookbackOrders(trades)
}
