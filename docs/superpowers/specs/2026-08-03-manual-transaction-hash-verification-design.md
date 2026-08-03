# 手动交易哈希验单设计

## 目标

让付款人提交已上链的交易哈希后，系统只对已实现查询与解析能力的支付方式验证交易，并将匹配的订单安全地推进到现有确认和回调流程。

## 支持范围

本功能只支持以下交易类型：

- TRON：`tron.trx`、`usdt.trc20`、`usdc.trc20`
- BSC：`bsc.bnb`、`usdt.bep20`、`usdc.bep20`
- Solana：`usdt.solana`、`usdc.solana`

其他已注册的支付方式不显示交易哈希输入框，接口也返回“不支持该支付网络”。本功能不增加 SOL 原生转账、Token-2022、未注册 mint 或其他链的手动验单能力。

## 后端流程

收银台调用 `POST /api/v1/pay/verify-transaction` 并提交 `trade_id` 和 `tx_hash`。接口加载订单，确认订单状态为等待支付或已过期，再调用任务层验证交易。验证成功后，接口只能调用 `model.ClaimPaymentConfirmation`；它将交易哈希原子认领，并将订单改为 `confirming`。现有链确认任务随后决定 `success`、商户回调和通知。

接口不得直接将订单改为成功，也不得直接调用商户回调。这样自动扫描、回查和用户提交哈希三个入口共享同一份认领和确认语义。

接口在执行幂等判断前先检查支付类型是否支持手动验单。未支持的链始终返回 `unsupported_network`，即使订单已经是确认中或成功状态，避免旧交易哈希绕过白名单。

## 订单时间窗口、过期恢复与交易唯一性

手动验单只接受 `waiting` 或 `expired` 订单。它不受自动扫描和自动回查的 `payment_lookback_hour` 限制：该配置只限制后台扫描的回溯范围，不能阻止客服或付款人在较晚时补交一笔已确认的有效交易。

不过链上交易时间必须严格落在订单窗口内：`CreatedAt < transaction timestamp < ExpiredAt`。因此超时后提交哈希只用于恢复“订单有效期内已付款、后续才确认或才提交凭证”的情况，不能用一笔更早或更晚的交易补单。

一笔链上交易可以包含多条转账，系统会逐条寻找真正匹配当前订单的那一条；但 `PaymentHashClaim` 的唯一性仍以规范化后的**整笔交易哈希**为单位。一笔交易最多只能认领一个本地订单，不支持用同一交易的多个 transfer leg 批量结算多个独立订单。若未来需要该能力，必须引入稳定的 `(chain, transaction hash, transfer identity)` 认领模型，而不能直接放宽现有哈希唯一约束。

## Solana 验证

Solana 签名是大小写敏感的 Base58 字符串。输入仅去除首尾空白，随后必须 Base58 解码为 64 字节，并保持原始大小写。系统不得添加 `0x`、转小写或使用不区分大小写的比较。

任务层使用现有 Solana RPC 故障切换封装查询：

```text
getTransaction(signature, {
  encoding: "jsonParsed",
  commitment: "finalized",
  maxSupportedTransactionVersion: 0
})
```

交易必须存在、已执行成功、包含 slot 和 blockTime。解析时复用现有 `parseSolanaParsedTransfers`，逐笔筛选而不是只选择第一笔转账。只有满足下列条件的转账才能匹配订单：

- 交易类型和注册 mint 与订单一致；
- 收款钱包 owner 与订单收款地址一致；
- 金额、订单创建时间和订单失效时间满足统一匹配规则；
- 金额位于该币种的有效区间；
- 交易哈希没有被其他订单认领。

解析必须使用 Token Account owner 来判断付款方和收款方，以支持 `transferChecked`、inner instruction 和多签付款。不能只读取指令中的 `authority` 字段。

## 哈希比较与认领

TRON 和 EVM/BSC 哈希继续沿用规范化后的十六进制大小写不敏感比较。Solana 哈希必须严格相等。这个比较规则用于：

- 已确认订单的幂等提交；
- `ClaimPaymentConfirmation` 的幂等认领；
- 旧订单表中交易哈希冲突检查；
- `PaymentHashClaim` 的唯一键生成。

数据库继续保存 Solana 的原始 Base58 签名。若 MySQL 的默认排序规则不区分大小写，Solana 的 claim 键必须使用字节安全的规范化存储，以避免两个不同大小写签名冲突。

TRON 哈希去除可选 `0x` 前缀并转小写；BSC 哈希去除可选前缀、转小写后统一添加 `0x`。幂等判断和认领键必须使用这两个规范化结果，保证用户以等价的前缀形式重试时仍然幂等。

Solana 自动回查、自动扫描和用户提交哈希都必须使用 `ClaimPaymentConfirmation`，不得调用 `MarkConfirming` 绕过持久化交易认领。

在嵌入式 SQLite 部署中，两个并发入口可能同时读取旧快照并竞争写入认领记录。仅当底层真实 SQLite 驱动返回 `SQLITE_BUSY`（包括扩展码）时，`ClaimPaymentConfirmation` 最多完整重跑三次事务；每一轮都会重新读取订单、旧认领和唯一键。MySQL、PostgreSQL、业务冲突和其他数据库错误不走此重试分支。这样竞争失败方会重新观察到已认领记录并返回既有的幂等或“交易已被使用”语义，而不会把暂态 SQLite 锁错误暴露为验单失败。

## UI 与接口契约

`/api/v1/pay/info` 返回 `can_verify_transaction_hash`。该字段由后端根据同一份支持白名单计算，前端不得自行维护链和币种白名单。

官方收银台默认隐藏 Transaction Hash 表单。仅当该字段为 `true` 时显示表单并绑定提交事件。订单重选支付方式后，页面重新读取订单信息并重新计算可见性。

订单超时后，支持的支付方式仍提供提交哈希的入口，以处理付款人在有效期结束前发起交易、但链上确认落在超时后的情况。

失败响应保留现有 `status_code` 与 `message` 字段，并增加稳定的错误代码。前端根据错误代码显示本地化文案，未知错误使用通用提示。错误代码包括：

- `invalid_hash`
- `unsupported_network`
- `transaction_not_found`
- `transaction_mismatch`
- `transaction_already_used`
- `order_not_receivable`
- `verification_unavailable`

失败响应的错误代码位于顶层字段。例如，不支持的支付方式返回：

```json
{
  "status_code": 400,
  "message": "transaction hash verification is not available for this payment network",
  "error_code": "unsupported_network"
}
```

请求格式错误使用 `invalid_hash`；订单不存在或当前状态不可接收使用 `order_not_receivable`。所有 `VerifyTransaction` 的失败分支都返回顶层 `error_code`。

## 可追溯性

每次验证请求记录结构化日志：订单交易号、交易类型、提交哈希、结果、错误代码和 RPC 耗时。此版本不引入新的数据库审计表。成功记录由订单字段和 `PaymentHashClaim` 持久化。

日志字段固定为 `trade_id`、`trade_type`、`tx_hash`、`result`、`error_code` 和 `rpc_duration_ms`。handler 的日志调用必须允许测试环境尚未初始化日志对象。

## 测试要求

测试必须先于实现编写并验证失败。至少覆盖：

- Solana 合法和非法 Base58 签名；
- Solana 签名大小写严格比较；
- `getTransaction(finalized, jsonParsed)` 请求和成功解析；
- `result: null`、交易失败、错误 mint、错误收款地址、错误金额、错误时间窗口和重复哈希；
- 同一笔交易中第一个转账不匹配、后一个转账匹配；
- 多签 `transferChecked` 的付款方与收款 owner 解析；
- inner instruction 和多签场景中，`authority` 与 Token Account owner 不同仍能正确验证；
- TRON 和 BSC 保持既有验单能力；
- TRON/BSC 带或不带 `0x` 前缀的等价重试保持幂等；
- 自动回查和手动验单竞争同一 Solana 签名时只有一个订单可认领；
- `can_verify_transaction_hash` 只对允许的交易类型为真；
- 官方模板仅对支持支付方式显示表单，并展示准确的失败原因。
- 订单超时后，支持类型的提交控件仍可点击并发送验单请求。

## 非目标

- 不对历史订单批量自动补单；
- 不直接通过用户提交哈希将订单标记为成功；
- 不扩展到未实现手动验单的支付网络；
- 不修改管理后台“强制补单”的语义。
