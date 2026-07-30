# Payment hash verification and order matching recovery

## Goal

Prevent valid transfers from being silently classified as non-order activity, and
allow a payer to submit a transaction hash as an immediate verification clue for
TRON and BSC payments.

## Confirmed product behaviour

- The checkout displays the transaction-hash entry point immediately; it does
  not wait for a countdown.
- A submitted hash is only a verification clue. The server independently checks
  the transaction and never trusts client-provided payment details.
- The first release supports TRON and BSC.

## Security and state rules

1. Verify chain success/finality data, network, asset/contract, recipient,
   amount according to the configured matching mode, and the original order
   payment window.
2. Accept a matching payment made during the original payment window even if
   the scanner has subsequently changed the order to `expired`; this preserves
   the existing lookback recovery behaviour.
3. Reject cancelled and unrelated orders. Repeated submissions of the same
   matching hash for the same order are idempotent.
4. Record a durable unique hash claim before moving an order to `confirming`,
   so a hash cannot credit two orders. Existing confirmation and merchant
   callback paths remain responsible for final success.

## Implementation outline

1. Give scanner matching a canonical evaluator that returns an explicit
   mismatch reason. Normalize case-insensitive EVM addresses before map lookup
   and comparison.
2. Make an error reading receivable orders retry the transfer batch rather than
   classifying it as non-order activity. Log missing candidates and mismatch
   reasons with transaction/order identifiers.
3. Reuse the canonical evaluator in submitted-hash verification. Fetch and
   normalize transfer data through TRON gRPC or BSC JSON-RPC.
4. Add `POST /api/v1/pay/verify-transaction`, accepting only `trade_id` and
   `tx_hash`, then atomically claim the hash and mark the order confirming.
5. Add the immediate hash form, request state, and localized messages to the
   active official checkout. Backend support remains checkout-template agnostic.

## Verification

- Reproduce the affected TRC20 order match as a unit test.
- Test every canonical mismatch reason and database-read failure handling.
- Test accepted/rejected transaction lookups, duplicate claims, route results,
  and official checkout request wiring.
- Run targeted Go tests, then `go test ./...`.
