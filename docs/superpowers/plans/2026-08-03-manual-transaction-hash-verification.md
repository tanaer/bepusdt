# 手动交易哈希验单 Implementation Plan

> For agentic workers: REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox syntax for tracking.

**Goal:** 为 TRON、BSC 和 Solana 的既有支付类型提供安全的用户提交交易哈希验单，并在官方收银台只展示真实可用的入口。

**Architecture:** 任务层维护唯一的“支持手动验单”能力判定，并按订单交易类型验证和查询链上交易。Solana 复用现有 RPC 故障切换与 jsonParsed SPL Token 解析器；成功结果统一交给 ClaimPaymentConfirmation，再由现有确认任务完成入账和回调。订单信息接口下发能力标记，官方收银台据此展示入口和本地化错误。

**Tech Stack:** Go、Gin、GORM、SQLite/MySQL/PostgreSQL、Solana JSON-RPC、btcd Base58、原生 JavaScript、嵌入式静态资源。

---

## 文件结构

- app/task/payment_verification.go：支持白名单、链上哈希规范化、TRON/BSC/Solana 查询分发与统一匹配。
- app/task/payment_verification_test.go：任务层的哈希、Solana RPC 和交易匹配测试。
- app/model/payment_claim.go：按链选择哈希相等规则，生成大小写安全的认领键。
- app/model/payment_claim_test.go：Solana 大小写、幂等和跨订单认领测试。
- app/handler/epusdt/epusdt.go：订单信息能力字段、稳定验证错误代码和结构化日志。
- app/handler/epusdt/payment_verification_test.go：HTTP 验单响应和能力字段测试。
- static/checkout/official/：受控表单、样式和双语文案。
- app/router/checkout_template_test.go：嵌入模板与受控 UI 入口的静态回归检查。

### Task 1: 定义唯一的手动验单能力和接口契约

**Files:**

- Modify: app/task/payment_verification.go
- Modify: app/task/payment_verification_test.go
- Modify: app/handler/epusdt/epusdt.go
- Modify: app/handler/epusdt/payment_verification_test.go

- [ ] **Step 1: 写出支持类型和订单信息字段的失败测试**

在任务层测试精确断言下列类型返回 true：tron.trx、usdt.trc20、usdc.trc20、bsc.bnb、usdt.bep20、usdc.bep20、usdt.solana、usdc.solana。断言 usdt.erc20、usdc.polygon、usdt.aptos 等返回 false。在 HTTP 测试中创建 Solana 订单，断言 /api/v1/pay/info 的 data.can_verify_transaction_hash 为真；创建未支持订单，断言为假。

- [ ] **Step 2: 运行测试，确认它因缺少能力函数和字段失败**

Run: go test ./app/task ./app/handler/epusdt -run 'Test.*SubmittedPayment.*Support|TestInfo.*CanVerify' -count=1

Expected: FAIL，原因是能力函数或 JSON 字段不存在。

- [ ] **Step 3: 实现最小能力函数和接口字段**

在 app/task/payment_verification.go 导出 SupportsSubmittedPaymentVerification(tradeType model.TradeType) bool，只接受 TRON、BSC、Solana 三类精确白名单。Info 调用该函数并写入 can_verify_transaction_hash。保持其他接口兼容。

- [ ] **Step 4: 运行针对性测试，确认通过**

Run: go test ./app/task ./app/handler/epusdt -run 'Test.*SubmittedPayment.*Support|TestInfo.*CanVerify' -count=1

Expected: PASS。

- [ ] **Step 5: 提交能力契约**

Run:
  git add app/task/payment_verification.go app/task/payment_verification_test.go app/handler/epusdt/epusdt.go app/handler/epusdt/payment_verification_test.go
  git commit -m "feat(payment): expose transaction hash verification support"

### Task 2: 先测试 Solana 交易签名和链上查询

**Files:**

- Modify: app/task/payment_verification.go
- Modify: app/task/payment_verification_test.go

- [ ] **Step 1: 写 Solana 哈希规范化的失败测试**

使用真实格式的 64 字节 Base58 签名。测试首尾空白被去除但原始大小写保留；含 0、O、I、l 的字符串、无法解码的字符串和解码后不是 64 字节的字符串返回 ErrInvalidSubmittedPaymentHash。断言同一字母改变大小写后不是同一签名。

- [ ] **Step 2: 运行测试，确认 Solana 目前被拒绝**

Run: go test ./app/task -run 'TestNormalizeSubmittedPaymentHashSolana' -count=1

Expected: FAIL，当前逻辑返回 ErrUnsupportedSubmittedPayment。

- [ ] **Step 3: 实现 Solana 签名规范化**

复用 github.com/btcsuite/btcd/btcutil/base58。对 Solana 输入执行 TrimSpace、Base58 解码和 64 字节检查；返回原始大小写字符串。不要转小写或添加前缀。

- [ ] **Step 4: 运行规范化测试，确认通过**

Run: go test ./app/task -run 'TestNormalizeSubmittedPaymentHashSolana' -count=1

Expected: PASS。

- [ ] **Step 5: 写 getTransaction 的失败测试**

用 httptest.Server 模拟 Solana RPC。测试必须断言请求使用 getTransaction、jsonParsed、finalized 和 maxSupportedTransactionVersion: 0。返回一笔 transferChecked USDC 交易，包含 source/destination Token Account、pre/post balances、slot 与 blockTime。断言返回收款钱包 owner、付款钱包、金额、slot、时间和原始签名。再写 result: null、meta.err、错误 mint、错误收款 owner、错误金额、超出时间窗、第一笔转账不匹配而第二笔匹配的测试。

- [ ] **Step 6: 运行查询测试，确认缺少 Solana 查询分支而失败**

Run: go test ./app/task -run 'TestVerifySubmittedPayment.*Solana|TestLookupSolanaSubmittedPayment' -count=1

Expected: FAIL，当前逻辑返回 ErrUnsupportedSubmittedPayment。

- [ ] **Step 7: 实现 Solana 查询和逐笔匹配**

在 lookupSubmittedPayment 增加 Solana 分支。使用现有 sol.rpc 查询 getTransaction。拒绝空结果、执行失败、无 slot 或无 blockTime 的交易。复用 parseSolanaParsedTransfers，为每笔解析结果补入用户原始签名，过滤交易类型、金额范围和 orderTransferMatchReason；返回第一笔真正匹配订单的转账。没有匹配时返回 ErrSubmittedPaymentDoesNotMatch。

- [ ] **Step 8: 在通用验证路径增加金额范围保护**

在 verifySubmittedPayment 的统一匹配前调用 model.IsAmountValid(payment.TradeType, payment.Amount)，使锁定地址订单也遵守与扫描器相同的金额准入规则。

- [ ] **Step 9: 运行 Solana 与既有手动验单测试**

Run: go test ./app/task -run 'Test(NormalizeSubmittedPaymentHash|VerifySubmittedPayment|LookupSolanaSubmittedPayment|OrderTransferMatch)' -count=1

Expected: PASS。

- [ ] **Step 10: 提交 Solana 手动验单**

Run:
  git add app/task/payment_verification.go app/task/payment_verification_test.go
  git commit -m "feat(solana): verify submitted payment signatures"

### Task 3: 修复 Solana 哈希的大小写安全认领

**Files:**

- Modify: app/model/payment_claim.go
- Modify: app/model/payment_claim_test.go
- Modify: app/handler/epusdt/epusdt.go
- Modify: app/handler/epusdt/payment_verification_test.go

- [ ] **Step 1: 写失败测试**

创建 Solana 订单，使用同一精确签名两次认领，断言第二次为幂等成功。再用仅改变大小写的签名认领，断言它不被视为同一交易。为第二个订单提交原签名，断言为 ErrPaymentHashAlreadyClaimed。HTTP 测试中已确认订单提交大小写变体，断言接口不返回幂等成功。

- [ ] **Step 2: 运行测试，确认当前 EqualFold 会错误通过**

Run: go test ./app/model ./app/handler/epusdt -run 'Test.*Solana.*Case|Test.*Solana.*Idempotent' -count=1

Expected: FAIL，大小写变体被错误视为相同签名。

- [ ] **Step 3: 实现按交易类型的比较与认领键**

在 model 中封装按 trade type 的哈希相等函数：Solana 使用精确比较，TRON/EVM 使用大小写不敏感比较。对 Solana PaymentHashClaim.Hash 使用基于解码签名字节的、可逆且小于 128 字符的 canonical key；订单 RefHash 继续保存原始 Base58 字符串。旧订单表的冲突检查对 Solana 使用字节敏感比较。

- [ ] **Step 4: 将 HTTP 幂等判断改为共享比较函数**

VerifyTransaction 不再直接调用 strings.EqualFold，而是调用 model 的按类型比较函数。

- [ ] **Step 5: 运行模型与 HTTP 测试，确认通过**

Run: go test ./app/model ./app/handler/epusdt -run 'Test.*PaymentClaim|Test.*Solana.*Case|TestVerifyTransaction' -count=1

Expected: PASS。

- [ ] **Step 6: 提交大小写安全认领**

Run:
  git add app/model/payment_claim.go app/model/payment_claim_test.go app/handler/epusdt/epusdt.go app/handler/epusdt/payment_verification_test.go
  git commit -m "fix(solana): preserve case-sensitive payment claims"

### Task 4: 提供稳定错误码和结构化验单日志

**Files:**

- Modify: app/handler/epusdt/epusdt.go
- Modify: app/handler/epusdt/payment_verification_test.go

- [ ] **Step 1: 写失败测试**

让 handler 的替身验证器分别返回无效 Hash、不支持网络、未找到交易、不匹配、已被认领、不可接收订单和 RPC 错误。断言 JSON 响应保留 status_code: 400、保留兼容 message，并返回相应 error_code。

- [ ] **Step 2: 运行测试，确认响应中没有 error_code**

Run: go test ./app/handler/epusdt -run 'TestVerifyTransaction.*ErrorCode' -count=1

Expected: FAIL，错误响应中没有稳定错误码。

- [ ] **Step 3: 实现验证错误响应和结构化日志**

增加只供验单接口使用的失败响应帮助函数。将已知错误映射为设计文档定义的错误码；未知错误映射为 verification_unavailable，并记录 trade_id、trade_type、tx_hash、错误码和验证耗时。成功也记录结构化成功日志。不要改变其他 Epusdt API 的响应格式。

- [ ] **Step 4: 运行 handler 测试，确认通过**

Run: go test ./app/handler/epusdt -run 'TestVerifyTransaction' -count=1

Expected: PASS。

- [ ] **Step 5: 提交错误契约与日志**

Run:
  git add app/handler/epusdt/epusdt.go app/handler/epusdt/payment_verification_test.go
  git commit -m "feat(payment): report submitted hash verification errors"

### Task 5: 仅对支持支付方式显示官方收银台入口

**Files:**

- Modify: static/checkout/official/views/checkout.html
- Modify: static/checkout/official/assets/js/checkout.js
- Modify: static/checkout/official/assets/css/checkout.css
- Modify: static/checkout/official/assets/locales/en.json
- Modify: static/checkout/official/assets/locales/zh.json
- Modify: app/router/checkout_template_test.go

- [ ] **Step 1: 写模板静态失败测试**

在 router 测试中断言官方模板包含默认隐藏的 transactionHashForm、超时场景入口和必要的 data/i18n 标记；断言脚本读取后端 can_verify_transaction_hash 并使用 error_code，而不是前端链种白名单。

- [ ] **Step 2: 运行测试，确认当前模板没有入口**

Run: go test ./app/router -run 'TestOfficialCheckout.*TransactionHash' -count=1

Expected: FAIL，模板和脚本尚未实现受控入口。

- [ ] **Step 3: 实现受控表单显示与提交**

在官方模板增加默认隐藏的输入框、按钮和状态区。showPayment 将 can_verify_transaction_hash 传给 Payment.initQrPage；仅为 true 的订单显示并绑定表单。提交后使用后端 error_code 显示本地化的、可操作的失败原因，未知错误使用通用文案。订单超时时，支持类型的超时弹窗提供同一表单或明确的提交入口；不支持类型保持现有超时行为。

- [ ] **Step 4: 增加样式与双语文案**

为输入框、状态提示和超时入口增加最小样式。补齐中文与英文的标题、提示、按钮、成功、网络错误和七类错误码文案。

- [ ] **Step 5: 运行模板测试，确认通过**

Run: go test ./app/router -run 'TestOfficialCheckout.*TransactionHash' -count=1

Expected: PASS。

- [ ] **Step 6: 提交收银台 UI**

Run:
  git add static/checkout/official app/router/checkout_template_test.go
  git commit -m "feat(checkout): show supported transaction hash verification"

### Task 6: 全量验证、构建和交付

**Files:**

- Modify: docs/superpowers/specs/2026-08-03-manual-transaction-hash-verification-design.md（仅在实现暴露设计偏差时）
- Modify: docs/superpowers/plans/2026-08-03-manual-transaction-hash-verification.md（勾选已完成步骤）

- [ ] **Step 1: 格式化改动文件**

Run: gofmt -w app/task/payment_verification.go app/task/payment_verification_test.go app/model/payment_claim.go app/model/payment_claim_test.go app/handler/epusdt/epusdt.go app/handler/epusdt/payment_verification_test.go

- [ ] **Step 2: 运行聚焦回归测试**

Run: go test ./app/task ./app/model ./app/handler/epusdt ./app/router -count=1

Expected: PASS。

- [ ] **Step 3: 运行全量 Go 测试和生产构建**

Run: go test ./... -count=1 && go build -o bin/bepusdt ./main

Expected: PASS，并产出可启动二进制。

- [ ] **Step 4: 检查差异和提交边界**

Run: git diff 3caa4f6...HEAD --check && git status --short && git log --oneline 3caa4f6..HEAD

Expected: 无空白错误、无未提交文件，提交只包含设计、后端、测试和官方收银台资源。

- [ ] **Step 5: 推送新分支**

Run: git push -u fork feat/manual-transaction-hash-verification

Expected: fork 上出现同名分支，供创建第二个中文 PR。
