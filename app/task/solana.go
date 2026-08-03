package task

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil/base58"
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

// 参考文档
//  - https://solana.com/zh/docs/rpc
//  - https://github.com/solana-program/token/blob/6d18ff73b1dd30703a30b1ca941cb0f1d18c2b2a/program/src/instruction.rs

type solana struct {
	slotConfirmedOffset int
	lastSlotNum         int
	slotQueue           *chanx.UnboundedChan[int]
	client              *http.Client
}

type solanaTokenOwner struct {
	TradeType model.TradeType
	Address   string
}

var sol solana

var errSolanaSlotSkipped = errors.New("solana slot was permanently skipped")

func init() {
	sol = newSolana()
	Register(Task{Callback: sol.slotDispatch})
	Register(Task{Callback: sol.syncSlotForward, Duration: time.Second * 5})
	Register(Task{Callback: sol.tradeConfirmHandle, Duration: time.Second * 5})
	Register(Task{Callback: sol.lookbackSlots, Duration: time.Second * 15})
}

func newSolana() solana {
	return solana{
		slotConfirmedOffset: 60,
		lastSlotNum:         0,
		slotQueue:           chanx.NewUnboundedChan[int](context.Background(), 30),
		client:              utils.NewHttpClient(),
	}
}

func (s *solana) syncSlotForward(ctx context.Context) {
	if syncBreak(conf.Solana, s.slotQueue.Len()) {

		return
	}

	body, _, err := doRPCRequestWithFailover(ctx, s.client, conf.Solana, func(endpoint string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBuffer([]byte(`{"jsonrpc":"2.0","id":1,"method":"getSlot"}`)))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	}, func(body []byte) error {
		data := gjson.ParseBytes(body)
		if data.Get("error").Exists() {
			return fmt.Errorf("%s", data.Get("error").String())
		}
		return nil
	})
	if err != nil {
		log.Task.Warn("syncSlotForward Error:", err)

		return
	}

	now := int(gjson.GetBytes(body, "result").Int())
	if now <= 0 {
		log.Task.Warn("syncSlotForward Error: invalid slot number:", now)

		return
	}
	model.SetChainProgress(conf.Solana, now)

	if now-s.lastSlotNum > cast.ToInt(model.GetC(model.BlockHeightMaxDiff)) { // 区块高度变化过大，强制丢块重扫
		s.lastSlotNum = now
	}

	if now == s.lastSlotNum { // 区块高度没有变化

		return
	}

	for n := s.lastSlotNum + 1; n <= now; n++ {
		// 待扫描区块入列

		s.slotQueue.In <- n
	}

	s.lastSlotNum = now
}

func (s *solana) slotDispatch(ctx context.Context) {
	p, err := ants.NewPoolWithFunc(3, s.slotParse)
	if err != nil {
		log.Task.Warn("Error creating pool:", err)

		return
	}

	defer p.Release()

	for {
		select {
		case slot := <-s.slotQueue.Out:
			if err := p.Invoke(slot); err != nil {
				s.slotQueue.In <- slot
				log.Task.Warn("slotDispatch Error invoking process slot:", err)
			}
		case <-ctx.Done():
			if err := ctx.Err(); err != nil {
				log.Task.Warn("slotDispatch context done:", err)
			}

			return
		}
	}
}

func (s *solana) slotParse(n any) {
	slot := n.(int)
	post := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[%d,{"encoding":"json","maxSupportedTransactionVersion":0,"transactionDetails":"full","rewards":false}]}`, slot))
	network := conf.Solana

	body, _, err := doRPCRequestWithFailover(context.Background(), s.client, network, func(endpoint string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(context.Background(), "POST", endpoint, bytes.NewBuffer(post))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	}, func(body []byte) error {
		data := gjson.ParseBytes(body)
		if rpcErr := data.Get("error"); rpcErr.Exists() {
			if rpcErr.Get("code").Int() == -32007 {
				return terminalRPCError(fmt.Errorf("%w: %s", errSolanaSlotSkipped, rpcErr.Get("message").String()))
			}
			return fmt.Errorf("%s", rpcErr.String())
		}
		result := data.Get("result")
		if !result.Exists() || result.Raw == "null" {
			return fmt.Errorf("slot %d block is temporarily unavailable", slot)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errSolanaSlotSkipped) {
			log.Task.Info(fmt.Sprintf("Solana 跳过永久缺失 Slot：%d", slot))
			return
		}
		s.slotQueue.In <- slot
		log.Task.Warn("slotParse Error:", err)

		return
	}

	timestamp := time.Unix(gjson.GetBytes(body, "result.blockTime").Int(), 0)

	for _, trans := range gjson.GetBytes(body, "result.transactions").Array() {
		// A failed transaction can still retain parsed SPL instructions and token
		// balances in getBlock. It must never reach the generic transfer matcher.
		if txErr := trans.Get("meta.err"); txErr.Exists() && txErr.Raw != "null" {
			continue
		}

		hash := trans.Get("transaction.signatures.0").String()

		// 解析账号索引
		accountKeys := make([]string, 0)
		for _, key := range trans.Get("transaction.message.accountKeys").Array() {
			accountKeys = append(accountKeys, key.String())
		}
		for _, v := range []string{"readonly", "writable"} {
			for _, key := range trans.Get("meta.loadedAddresses." + v).Array() {
				accountKeys = append(accountKeys, key.String())
			}
		}

		// 查找SPL Token索引
		splTokenIndex := int64(-1)
		for i, v := range accountKeys {
			if v == conf.SolSplToken {
				splTokenIndex = int64(i)

				break
			}
		}

		// SPL Token的Mint地址，即不包含 Token 交易信息
		if splTokenIndex == -1 {

			continue
		}

		// 解析 Token 账户 【Token Wallet => Owner Wallet】
		tokenAccountMap := make(map[string]solanaTokenOwner)
		for _, v := range []string{"postTokenBalances", "preTokenBalances"} {
			for _, itm := range trans.Get("meta." + v).Array() {
				tradeType, ok := model.GetContractTrade(itm.Get("mint").String())
				if !ok || itm.Get("programId").String() != conf.SolSplToken {

					continue
				}

				tokenAccountMap[accountKeys[itm.Get("accountIndex").Int()]] = solanaTokenOwner{
					TradeType: tradeType,
					Address:   itm.Get("owner").String(),
				}
			}
		}

		transArr := make([]transfer, 0)

		// 解析外部指令
		for _, instr := range trans.Get("transaction.message.instructions").Array() {
			if instr.Get("programIdIndex").Int() != splTokenIndex {

				continue
			}

			transArr = append(transArr, s.parseTransfer(instr, accountKeys, tokenAccountMap))
		}

		// 解析内部指令
		for _, itm := range trans.Get("meta.innerInstructions").Array() {
			for _, instr := range itm.Get("instructions").Array() {
				if instr.Get("programIdIndex").Int() != splTokenIndex {

					continue
				}

				transArr = append(transArr, s.parseTransfer(instr, accountKeys, tokenAccountMap))
			}
		}

		// 过滤无关交易
		result := make([]transfer, 0)
		for _, t := range transArr {
			if t.FromAddress == "" || t.RecvAddress == "" || t.Amount.IsZero() {

				continue
			}

			t.TxHash = hash
			t.Network = conf.Solana
			t.BlockNum = slot
			t.Timestamp = timestamp

			result = append(result, t)
		}

		if len(result) > 0 {
			transferQueue.In <- result
		}
	}

	log.Task.Info(fmt.Sprintf("区块扫描完成(Solana) %d 成功率：%s", slot, conf.GetSuccessRate(network)))
}

func (s *solana) parseTransfer(instr gjson.Result, accountKeys []string, tokenAccountMap map[string]solanaTokenOwner) transfer {
	accounts := instr.Get("accounts").Array()
	trans := transfer{}
	if len(accounts) < 3 { // from to singer，至少存在3个账户索引，如果是多签则 > 3

		return trans
	}

	data := base58.Decode(instr.Get("data").String())
	dLen := len(data)
	if dLen < 9 {

		return trans
	}

	isTransfer := data[0] == 3 && dLen == 9
	isTransferChecked := data[0] == 12 && dLen == 10
	if !isTransfer && !isTransferChecked {

		return trans
	}

	var exp int32 = -6
	if isTransferChecked {
		exp = int32(data[9]) * -1
	}

	from, ok := tokenAccountMap[accountKeys[accounts[0].Int()]]
	if !ok {

		return trans
	}

	trans.FromAddress = from.Address
	trans.RecvAddress = tokenAccountMap[accountKeys[accounts[1].Int()]].Address
	if isTransferChecked {
		trans.RecvAddress = tokenAccountMap[accountKeys[accounts[2].Int()]].Address
	}

	buf := make([]byte, 8)
	copy(buf[:], data[1:9])
	number := binary.LittleEndian.Uint64(buf)
	b := new(big.Int)
	b.SetUint64(number)
	trans.TradeType = from.TradeType
	trans.Amount = decimal.NewFromBigInt(b, exp)

	return trans
}

func (s *solana) tradeConfirmHandle(ctx context.Context) {
	var orders = getConfirmingOrders(model.GetNetworkTrades(conf.Solana))
	var wg sync.WaitGroup

	var handle = func(o model.Order) {
		if model.GetC(model.BlockOffsetConfirm) == "1" {
			if s.lastSlotNum == 0 {
				return
			}
			if s.lastSlotNum-o.RefBlockNum < s.slotConfirmedOffset {
				return
			}
		}

		post := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"getSignatureStatuses","params":[["%s"],{"searchTransactionHistory":true}]}`, o.RefHash))
		body, _, err := doRPCRequestWithFailover(ctx, s.client, conf.Solana, func(endpoint string) (*http.Request, error) {
			req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBuffer(post))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", "application/json")
			return req, nil
		}, func(body []byte) error {
			data := gjson.ParseBytes(body)
			if data.Get("error").Exists() {
				return fmt.Errorf("%s", data.Get("error").String())
			}
			return nil
		})
		if err != nil {
			log.Task.Warn("solana tradeConfirmHandle Error:", err)

			return
		}

		data := gjson.ParseBytes(body)
		status := data.Get("result.value.0")
		// Solana finalizes both successful and failed transactions. The status
		// must therefore be finalized *and* have a null execution error before
		// an order can become successful or trigger a merchant callback.
		if txErr := status.Get("err"); txErr.Exists() && txErr.Raw != "null" {
			return
		}
		if status.Get("confirmationStatus").String() == "finalized" {

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

func (s *solana) lookbackSlots(ctx context.Context) {
	if syncBreak(conf.Solana, s.slotQueue.Len()) {
		return
	}

	s.reconcileWaitingOrders(ctx)

	startAt, endAt, ok := getLookbackUnix(conf.Solana)
	if !ok {
		return
	}

	start, end := blockapi.New().GetBoundaryHeights(startAt, endAt, conf.Solana)
	for i := int(start); i <= int(end); i++ {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if syncBreak(conf.Solana, s.slotQueue.Len()) {
			return
		}
		s.slotQueue.In <- i
		time.Sleep(time.Millisecond * 200)
	}
}

func (s *solana) reconcileWaitingOrders(ctx context.Context) {
	trades := model.GetNetworkTrades(conf.Solana)
	if len(trades) == 0 {
		return
	}

	orders := make([]model.Order, 0)
	model.Db.Where("status in (?) and trade_type in (?)", receivableOrderStatuses(), trades).
		Where("expired_at > ?", time.Now().Add(model.GetLookbackHour())).
		Order("created_at asc").
		Find(&orders)
	if len(orders) == 0 {
		return
	}

	tokenAccounts := make(map[string][]string)
	for _, order := range orders {
		if ctx.Err() != nil {
			return
		}

		address := orderMatchAddress(order)
		key := fmt.Sprintf("%s%s", address, order.TradeType)
		accounts, ok := tokenAccounts[key]
		if !ok {
			var err error
			accounts, err = s.getTokenAccountsByOwner(ctx, address, order.TradeType)
			if err != nil {
				log.Task.Warn("solana reconcile getTokenAccountsByOwner Error:", err)
				continue
			}
			tokenAccounts[key] = accounts
		}

		for _, account := range accounts {
			if s.reconcileOrderTokenAccount(ctx, order, account) {
				break
			}
		}
	}
}

func (s *solana) getTokenAccountsByOwner(ctx context.Context, owner string, tradeType model.TradeType) ([]string, error) {
	contract := ""
	if c, ok := model.GetAllTradeConfig()[string(tradeType)]; ok {
		contract = c.Contract
	}
	if contract == "" {
		return nil, nil
	}

	result, err := s.rpc(ctx, "getTokenAccountsByOwner", []any{
		owner,
		map[string]any{"mint": contract},
		map[string]any{"encoding": "jsonParsed"},
	})
	if err != nil {
		return nil, err
	}

	accounts := make([]string, 0)
	for _, item := range result.Get("value").Array() {
		pubkey := item.Get("pubkey").String()
		if pubkey != "" {
			accounts = append(accounts, pubkey)
		}
	}

	return accounts, nil
}

func (s *solana) reconcileOrderTokenAccount(ctx context.Context, order model.Order, account string) bool {
	result, err := s.rpc(ctx, "getSignaturesForAddress", []any{
		account,
		map[string]any{"limit": 50},
	})
	if err != nil {
		log.Task.Warn("solana reconcile getSignaturesForAddress Error:", err)
		return false
	}

	for _, sig := range result.Array() {
		if sig.Get("err").Exists() && sig.Get("err").Raw != "null" {
			continue
		}

		blockTime := sig.Get("blockTime").Int()
		if blockTime > 0 {
			ts := time.Unix(blockTime, 0)
			if !order.CreatedAt.Before(ts) || !order.ExpiredAt.After(ts) {
				continue
			}
		}

		hash := sig.Get("signature").String()
		if hash == "" {
			continue
		}

		result, err := s.rpc(ctx, "getTransaction", []any{
			hash,
			map[string]any{"encoding": "jsonParsed", "maxSupportedTransactionVersion": 0},
		})
		if err != nil {
			log.Task.Warn("solana reconcile getTransaction Error:", err)
			continue
		}
		if result.Get("meta.err").Exists() && result.Get("meta.err").Raw != "null" {
			continue
		}

		for _, t := range parseSolanaParsedTransfers(result) {
			t.TxHash = hash
			if t.BlockNum == 0 {
				t.BlockNum = int(result.Get("slot").Int())
			}
			if t.Timestamp.IsZero() {
				t.Timestamp = time.Unix(result.Get("blockTime").Int(), 0)
			}
			if !orderTransferMatch(order, t) {
				continue
			}

			if err := order.MarkConfirming(t.BlockNum, t.FromAddress, t.TxHash, t.Timestamp, t.Amount); err != nil {
				log.Task.Warn("solana reconcile mark order confirming failed:", err)
				return false
			}

			log.Task.Info(fmt.Sprintf("Solana 订单回查确认成功：%s %s", order.TradeId, t.TxHash))
			return true
		}
	}

	return false
}

func (s *solana) rpc(ctx context.Context, method string, params any) (gjson.Result, error) {
	post, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return gjson.Result{}, err
	}

	body, _, err := doRPCRequestWithFailover(ctx, s.client, conf.Solana, func(endpoint string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBuffer(post))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	}, func(body []byte) error {
		data := gjson.ParseBytes(body)
		if data.Get("error").Exists() {
			return fmt.Errorf("%s", data.Get("error").String())
		}
		return nil
	})
	if err != nil {
		return gjson.Result{}, err
	}

	data := gjson.ParseBytes(body)

	return data.Get("result"), nil
}

func parseSolanaParsedTransfers(tx gjson.Result) []transfer {
	tokenAccountMap := make(map[string]solanaTokenOwner)
	for _, v := range []string{"postTokenBalances", "preTokenBalances"} {
		for _, item := range tx.Get("meta." + v).Array() {
			tradeType, ok := model.GetContractTrade(item.Get("mint").String())
			if !ok || item.Get("programId").String() != conf.SolSplToken {
				continue
			}

			tokenAccountMap[item.Get("accountIndex").String()] = solanaTokenOwner{
				TradeType: tradeType,
				Address:   item.Get("owner").String(),
			}
		}
	}

	transfers := make([]transfer, 0)
	instructions := tx.Get("transaction.message.instructions").Array()
	for _, inner := range tx.Get("meta.innerInstructions").Array() {
		instructions = append(instructions, inner.Get("instructions").Array()...)
	}

	for _, instr := range instructions {
		if instr.Get("programId").String() != conf.SolSplToken {
			continue
		}

		parsed := instr.Get("parsed")
		if !parsed.Exists() {
			continue
		}

		typ := parsed.Get("type").String()
		if typ != "transfer" && typ != "transferChecked" {
			continue
		}

		info := parsed.Get("info")
		source := info.Get("source").String()
		destination := info.Get("destination").String()
		from, ok := tokenAccountOwnerByPubkey(tx, tokenAccountMap, source)
		if !ok {
			continue
		}
		to, ok := tokenAccountOwnerByPubkey(tx, tokenAccountMap, destination)
		if !ok {
			continue
		}

		amountRaw := info.Get("tokenAmount.amount").String()
		decimals := info.Get("tokenAmount.decimals").Int()
		if amountRaw == "" {
			amountRaw = info.Get("amount").String()
			decimals = 6
		}

		amountInt, ok := new(big.Int).SetString(amountRaw, 10)
		if !ok {
			continue
		}

		transfers = append(transfers, transfer{
			Network:     conf.Solana,
			Amount:      decimal.NewFromBigInt(amountInt, -int32(decimals)),
			FromAddress: from.Address,
			RecvAddress: to.Address,
			Timestamp:   time.Unix(tx.Get("blockTime").Int(), 0),
			TradeType:   from.TradeType,
			BlockNum:    int(tx.Get("slot").Int()),
		})
	}

	return transfers
}

func tokenAccountOwnerByPubkey(tx gjson.Result, tokenAccountMap map[string]solanaTokenOwner, pubkey string) (solanaTokenOwner, bool) {
	accountKeys := tx.Get("transaction.message.accountKeys").Array()
	for i, key := range accountKeys {
		if key.Get("pubkey").String() == pubkey || key.String() == pubkey {
			owner, ok := tokenAccountMap[fmt.Sprintf("%d", i)]
			return owner, ok
		}
	}

	return solanaTokenOwner{}, false
}
