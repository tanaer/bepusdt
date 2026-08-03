package model

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gosqlite "github.com/glebarez/go-sqlite"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	sqlite3 "modernc.org/sqlite/lib"
)

func TestClaimPaymentConfirmationPreventsHashReuseAndIsIdempotent(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	first := newPaymentClaimTestOrder("payment-claim-first", now)
	second := newPaymentClaimTestOrder("payment-claim-second", now)
	if err := Db.Create(&first).Error; err != nil {
		t.Fatalf("create first order: %v", err)
	}
	if err := Db.Create(&second).Error; err != nil {
		t.Fatalf("create second order: %v", err)
	}
	payment := PaymentConfirmation{
		BlockNum: 100,
		From:     "TJ5usJLLwjwn7Pw3TPbdzreG7dvgKzfQ5y",
		Hash:     "303245E65DA6DCA3E7AED8DC5386CD0CBBC7F67E4C21FF4EE0D9330055A41983",
		At:       now.Add(time.Second),
		Amount:   decimal.RequireFromString("65.94"),
	}

	already, err := ClaimPaymentConfirmation(&first, payment)
	if err != nil || already {
		t.Fatalf("first claim = already:%t err:%v, want new successful claim", already, err)
	}
	if first.Status != OrderStatusConfirming {
		t.Fatalf("first order status = %d, want confirming", first.Status)
	}

	already, err = ClaimPaymentConfirmation(&first, payment)
	if err != nil || !already {
		t.Fatalf("same order repeat = already:%t err:%v, want idempotent success", already, err)
	}

	if _, err := ClaimPaymentConfirmation(&second, payment); !errors.Is(err, ErrPaymentHashAlreadyClaimed) {
		t.Fatalf("claiming same hash for second order error = %v, want ErrPaymentHashAlreadyClaimed", err)
	}
}

func TestClaimPaymentConfirmationRejectsCompetingClaimWhenConflictReportsFoundRow(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	first := newPaymentClaimTestOrder("payment-claim-found-row-first", now)
	second := newPaymentClaimTestOrder("payment-claim-found-row-second", now)
	if err := Db.Create(&first).Error; err != nil {
		t.Fatalf("create first order: %v", err)
	}
	if err := Db.Create(&second).Error; err != nil {
		t.Fatalf("create second order: %v", err)
	}
	payment := PaymentConfirmation{
		BlockNum: 100,
		From:     "TJ5usJLLwjwn7Pw3TPbdzreG7dvgKzfQ5y",
		Hash:     "303245E65DA6DCA3E7AED8DC5386CD0CBBC7F67E4C21FF4EE0D9330055A41983",
		At:       now.Add(time.Second),
		Amount:   decimal.RequireFromString("65.94"),
	}
	claimHash, err := paymentHashClaimKey(second.TradeType, payment.Hash)
	if err != nil {
		t.Fatalf("build payment claim key: %v", err)
	}

	const (
		injectCompetingClaimCallback = "test:payment_claim_inject_competitor"
		forceFoundRowsCallback       = "test:payment_claim_force_found_rows"
	)
	var injectedCompetingClaim bool
	if err := Db.Callback().Create().Before("gorm:create").Register(injectCompetingClaimCallback, func(tx *gorm.DB) {
		if injectedCompetingClaim || tx.Statement.Schema == nil || tx.Statement.Schema.Table != (PaymentHashClaim{}).TableName() {
			return
		}
		// Use Exec so this callback does not recursively invoke the Create
		// callback chain. The insert runs on the claim transaction itself,
		// representing another order winning the race immediately before the
		// attempted insert.
		quotedTable := tx.Statement.Quote((PaymentHashClaim{}).TableName())
		result := tx.Exec(
			"INSERT INTO "+quotedTable+" (hash, order_id, created_at, updated_at) VALUES (?, ?, ?, ?)",
			claimHash, first.ID, now, now,
		)
		if result.Error != nil {
			tx.AddError(result.Error)
			return
		}
		injectedCompetingClaim = true
	}); err != nil {
		t.Fatalf("register competing-claim callback: %v", err)
	}
	if err := Db.Callback().Create().After("gorm:create").Register(forceFoundRowsCallback, func(tx *gorm.DB) {
		if injectedCompetingClaim && tx.Statement.Schema != nil && tx.Statement.Schema.Table == (PaymentHashClaim{}).TableName() {
			// MySQL with clientFoundRows=true reports a matching duplicate-key
			// no-op update as one affected row. Reproduce that driver contract
			// without requiring a MySQL service in the unit suite.
			tx.RowsAffected = 1
		}
	}); err != nil {
		t.Fatalf("register found-rows callback: %v", err)
	}
	t.Cleanup(func() {
		if err := Db.Callback().Create().Remove(injectCompetingClaimCallback); err != nil {
			t.Errorf("remove competing-claim callback: %v", err)
		}
		if err := Db.Callback().Create().Remove(forceFoundRowsCallback); err != nil {
			t.Errorf("remove found-rows callback: %v", err)
		}
	})

	if _, err := ClaimPaymentConfirmation(&second, payment); !errors.Is(err, ErrPaymentHashAlreadyClaimed) {
		t.Fatalf("claim with found-row conflict error = %v, want ErrPaymentHashAlreadyClaimed", err)
	}
	if !injectedCompetingClaim {
		t.Fatal("competing claim callback did not run")
	}

	var refreshed Order
	if err := Db.First(&refreshed, second.ID).Error; err != nil {
		t.Fatalf("reload second order: %v", err)
	}
	if refreshed.Status != OrderStatusWaiting || refreshed.RefHash != "" {
		t.Fatalf("second order must remain unclaimed: %+v", refreshed)
	}
}

func TestMarkConfirmingClaimsPaymentHash(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	first := newPaymentClaimTestOrder("mark-confirming-first", now)
	second := newPaymentClaimTestOrder("mark-confirming-second", now)
	if err := Db.Create(&first).Error; err != nil {
		t.Fatalf("create first order: %v", err)
	}
	if err := Db.Create(&second).Error; err != nil {
		t.Fatalf("create second order: %v", err)
	}

	hash := strings.Repeat("a", 64)
	if err := first.MarkConfirming(100, "sender", hash, now.Add(time.Second), decimal.RequireFromString("65.94")); err != nil {
		t.Fatalf("mark first order confirming: %v", err)
	}
	if first.Status != OrderStatusConfirming || first.RefHash != hash {
		t.Fatalf("first order confirmation = %+v, want claimed payment", first)
	}
	claimHash, err := paymentHashClaimKey(first.TradeType, hash)
	if err != nil {
		t.Fatalf("build payment claim key: %v", err)
	}
	var claim PaymentHashClaim
	if err := Db.First(&claim, "hash = ?", claimHash).Error; err != nil {
		t.Fatalf("load durable payment claim: %v", err)
	}
	if claim.Hash != claimHash || claim.OrderID != first.ID {
		t.Fatalf("payment claim = %+v, want hash %q owned by order %d", claim, claimHash, first.ID)
	}

	if err := second.MarkConfirming(100, "sender", hash, now.Add(time.Second), decimal.RequireFromString("65.94")); !errors.Is(err, ErrPaymentHashAlreadyClaimed) {
		t.Fatalf("mark second order confirming error = %v, want ErrPaymentHashAlreadyClaimed", err)
	}

	var refreshed Order
	if err := Db.First(&refreshed, second.ID).Error; err != nil {
		t.Fatalf("reload second order: %v", err)
	}
	if refreshed.Status != OrderStatusWaiting || refreshed.RefHash != "" {
		t.Fatalf("second order must remain unclaimed: %+v", refreshed)
	}
}

func TestClaimPaymentConfirmationProtectsLegacyOrderHash(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	legacy := newPaymentClaimTestOrder("legacy-payment-claim", now)
	legacy.Status = OrderStatusConfirming
	legacy.RefHash = "303245e65da6dca3e7aed8dc5386cd0cbbc7f67e4c21ff4ee0d9330055a41983"
	if err := Db.Create(&legacy).Error; err != nil {
		t.Fatalf("create legacy order: %v", err)
	}

	newOrder := newPaymentClaimTestOrder("new-payment-claim", now)
	if err := Db.Create(&newOrder).Error; err != nil {
		t.Fatalf("create new order: %v", err)
	}
	_, err := ClaimPaymentConfirmation(&newOrder, PaymentConfirmation{
		BlockNum: 101,
		From:     "TJ5usJLLwjwn7Pw3TPbdzreG7dvgKzfQ5y",
		Hash:     legacy.RefHash,
		At:       now.Add(time.Second),
		Amount:   decimal.RequireFromString("65.94"),
	})
	if !errors.Is(err, ErrPaymentHashAlreadyClaimed) {
		t.Fatalf("legacy hash reuse error = %v, want ErrPaymentHashAlreadyClaimed", err)
	}
}

func TestClaimPaymentConfirmationPreservesCaseSensitiveSolanaSignature(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	order := newPaymentClaimTestOrder("solana-payment-claim", now)
	order.TradeType = UsdtSolana
	if err := Db.Create(&order).Error; err != nil {
		t.Fatalf("create order: %v", err)
	}

	const signature = "5GTnZsNBcpPxBb2fEA5u2SwaNVry97ouHsa5oFKKVNEt4Da8YzakDTky6byNLGZDQdLrx9sDXeNr7XYDgNDiSJJQ"
	_, err := ClaimPaymentConfirmation(&order, PaymentConfirmation{
		BlockNum: 435577393,
		From:     "H1NwZnujLy6q2q9Mj813xCMSzkmYE6p6PzyC2zswJSmK",
		Hash:     signature,
		At:       time.Unix(1785171892, 0),
		Amount:   decimal.RequireFromString("1.32"),
	})
	if err != nil {
		t.Fatalf("claim Solana payment: %v", err)
	}
	if order.RefHash != signature {
		t.Fatalf("stored Solana signature = %q, want original %q", order.RefHash, signature)
	}
	var claim PaymentHashClaim
	if err := Db.Where("order_id = ?", order.ID).First(&claim).Error; err != nil {
		t.Fatalf("load Solana payment claim: %v", err)
	}
	if claim.Hash == signature {
		t.Fatalf("Solana claim key must be collation-safe, got raw signature %q", claim.Hash)
	}
	if !strings.HasPrefix(claim.Hash, "solana:") {
		t.Fatalf("Solana claim key = %q, want solana prefix", claim.Hash)
	}
	encodedClaim := strings.TrimPrefix(claim.Hash, "solana:")
	if len(encodedClaim) != 103 || encodedClaim != strings.ToLower(encodedClaim) || strings.Contains(encodedClaim, "=") {
		t.Fatalf("Solana claim key = %q, want lowercase unpadded Base32", claim.Hash)
	}

	already, err := ClaimPaymentConfirmation(&order, PaymentConfirmation{
		BlockNum: 435577393,
		From:     "H1NwZnujLy6q2q9Mj813xCMSzkmYE6p6PzyC2zswJSmK",
		Hash:     signature,
		At:       time.Unix(1785171892, 0),
		Amount:   decimal.RequireFromString("1.32"),
	})
	if err != nil || !already {
		t.Fatalf("repeat Solana claim = already:%t err:%v, want idempotent success", already, err)
	}
}

func TestPaymentHashClaimKeyRejectsOutOfRangeSolanaSignature(t *testing.T) {
	for _, hash := range []string{
		strings.Repeat("1", 63),
		strings.Repeat("1", 89),
	} {
		if _, err := paymentHashClaimKey(UsdcSolana, hash); err == nil {
			t.Fatalf("payment claim key accepted out-of-range Solana signature length %d", len(hash))
		}
	}
}

func TestClaimPaymentConfirmationRejectsOutOfRangeSolanaSignature(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	order := newPaymentClaimTestOrder("oversized-solana-payment-claim", now)
	order.TradeType = UsdcSolana
	if err := Db.Create(&order).Error; err != nil {
		t.Fatalf("create order: %v", err)
	}

	_, err := ClaimPaymentConfirmation(&order, PaymentConfirmation{
		BlockNum: 1,
		From:     "sender",
		Hash:     strings.Repeat("1", 89),
		At:       now.Add(time.Second),
		Amount:   decimal.RequireFromString("1.00"),
	})
	if err == nil {
		t.Fatal("claim out-of-range Solana signature error = nil, want rejection")
	}

	var refreshed Order
	if err := Db.First(&refreshed, order.ID).Error; err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if refreshed.Status != OrderStatusWaiting || refreshed.RefHash != "" {
		t.Fatalf("order must remain unclaimed: %+v", refreshed)
	}
	var claimCount int64
	if err := Db.Model(&PaymentHashClaim{}).Count(&claimCount).Error; err != nil {
		t.Fatalf("count payment claims: %v", err)
	}
	if claimCount != 0 {
		t.Fatalf("payment claim count = %d, want zero", claimCount)
	}
}

func TestPaymentHashEqualUsesChainSpecificCanonicalization(t *testing.T) {
	const solanaSignature = "2gqC3gYGfdNMQkF5xhZXmofbjuu3RbdZZrKz7pYvuArMpqgHSvvrQb25AuDVvtsswxkfjWZbDDouH1FBFWwgYkD4"
	const solanaCaseVariant = "2GqC3gYGfdNMQkF5xhZXmofbjuu3RbdZZrKz7pYvuArMpqgHSvvrQb25AuDVvtsswxkfjWZbDDouH1FBFWwgYkD4"
	if PaymentHashEqual(UsdcSolana, solanaSignature, solanaCaseVariant) {
		t.Fatal("Solana signatures that differ by case must not compare equal")
	}
	if !PaymentHashEqual(UsdtBep20, strings.Repeat("A", 64), "0x"+strings.Repeat("a", 64)) {
		t.Fatal("BSC transaction hashes with or without 0x must compare equal")
	}
	if !PaymentHashEqual(UsdtTrc20, "0x"+strings.Repeat("A", 64), strings.Repeat("a", 64)) {
		t.Fatal("TRON transaction hashes with optional 0x must compare equal")
	}
}

func TestClaimPaymentConfirmationAllowsDistinctSolanaCaseVariant(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	first := newPaymentClaimTestOrder("solana-case-first", now)
	second := newPaymentClaimTestOrder("solana-case-second", now)
	first.TradeType = UsdcSolana
	second.TradeType = UsdcSolana
	if err := Db.Create(&first).Error; err != nil {
		t.Fatalf("create first order: %v", err)
	}
	if err := Db.Create(&second).Error; err != nil {
		t.Fatalf("create second order: %v", err)
	}
	third := newPaymentClaimTestOrder("solana-case-third", now)
	third.TradeType = UsdcSolana
	if err := Db.Create(&third).Error; err != nil {
		t.Fatalf("create third order: %v", err)
	}

	const signature = "2gqC3gYGfdNMQkF5xhZXmofbjuu3RbdZZrKz7pYvuArMpqgHSvvrQb25AuDVvtsswxkfjWZbDDouH1FBFWwgYkD4"
	const caseVariant = "2GqC3gYGfdNMQkF5xhZXmofbjuu3RbdZZrKz7pYvuArMpqgHSvvrQb25AuDVvtsswxkfjWZbDDouH1FBFWwgYkD4"
	basePayment := PaymentConfirmation{BlockNum: 1, From: "sender", At: now, Amount: decimal.RequireFromString("1.00")}

	basePayment.Hash = signature
	if _, err := ClaimPaymentConfirmation(&first, basePayment); err != nil {
		t.Fatalf("claim original signature: %v", err)
	}
	if _, err := ClaimPaymentConfirmation(&third, basePayment); !errors.Is(err, ErrPaymentHashAlreadyClaimed) {
		t.Fatalf("claim exact Solana signature for another order error = %v, want ErrPaymentHashAlreadyClaimed", err)
	}
	basePayment.Hash = caseVariant
	if _, err := ClaimPaymentConfirmation(&second, basePayment); err != nil {
		t.Fatalf("claim case-variant signature: %v", err)
	}
}

func TestClaimPaymentConfirmationTreatsBscPrefixVariantsAsIdempotent(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	order := newPaymentClaimTestOrder("bsc-prefix-variant", now)
	order.TradeType = UsdtBep20
	if err := Db.Create(&order).Error; err != nil {
		t.Fatalf("create order: %v", err)
	}

	firstHash := strings.Repeat("A", 64)
	if _, err := ClaimPaymentConfirmation(&order, PaymentConfirmation{
		BlockNum: 1,
		From:     "0xsender",
		Hash:     firstHash,
		At:       now,
		Amount:   decimal.RequireFromString("1.00"),
	}); err != nil {
		t.Fatalf("claim BSC payment without prefix: %v", err)
	}

	already, err := ClaimPaymentConfirmation(&order, PaymentConfirmation{
		BlockNum: 1,
		From:     "0xsender",
		Hash:     "0x" + strings.Repeat("a", 64),
		At:       now,
		Amount:   decimal.RequireFromString("1.00"),
	})
	if err != nil || !already {
		t.Fatalf("repeat BSC claim = already:%t err:%v, want idempotent success", already, err)
	}
}

func TestClaimPaymentConfirmationProtectsLegacyBscPrefixVariant(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	legacy := newPaymentClaimTestOrder("legacy-bsc-payment-claim", now)
	legacy.TradeType = UsdtBep20
	legacy.Status = OrderStatusConfirming
	legacy.RefHash = "0x" + strings.Repeat("a", 64)
	if err := Db.Create(&legacy).Error; err != nil {
		t.Fatalf("create legacy order: %v", err)
	}

	order := newPaymentClaimTestOrder("new-bsc-payment-claim", now)
	order.TradeType = UsdtBep20
	if err := Db.Create(&order).Error; err != nil {
		t.Fatalf("create new order: %v", err)
	}

	_, err := ClaimPaymentConfirmation(&order, PaymentConfirmation{
		BlockNum: 1,
		From:     "0xsender",
		Hash:     strings.Repeat("A", 64),
		At:       now,
		Amount:   decimal.RequireFromString("1.00"),
	})
	if !errors.Is(err, ErrPaymentHashAlreadyClaimed) {
		t.Fatalf("legacy BSC prefix variant error = %v, want ErrPaymentHashAlreadyClaimed", err)
	}
}

func TestClaimPaymentConfirmationRejectsConflictingClaimForSameOrder(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	order := newPaymentClaimTestOrder("conflicting-payment-claim", now)
	order.TradeType = UsdcSolana
	order.RefHash = "2GqC3gYGfdNMQkF5xhZXmofbjuu3RbdZZrKz7pYvuArMpqgHSvvrQb25AuDVvtsswxkfjWZbDDouH1FBFWwgYkD4"
	if err := Db.Create(&order).Error; err != nil {
		t.Fatalf("create order: %v", err)
	}

	const hash = "2gqC3gYGfdNMQkF5xhZXmofbjuu3RbdZZrKz7pYvuArMpqgHSvvrQb25AuDVvtsswxkfjWZbDDouH1FBFWwgYkD4"
	claimHash, err := paymentHashClaimKey(order.TradeType, hash)
	if err != nil {
		t.Fatalf("build Solana claim key: %v", err)
	}
	if err := Db.Create(&PaymentHashClaim{Hash: claimHash, OrderID: order.ID}).Error; err != nil {
		t.Fatalf("seed conflicting payment claim: %v", err)
	}

	_, err = ClaimPaymentConfirmation(&order, PaymentConfirmation{
		BlockNum: 1,
		From:     "sender",
		Hash:     hash,
		At:       now,
		Amount:   decimal.RequireFromString("1.00"),
	})
	if !errors.Is(err, ErrPaymentHashAlreadyClaimed) {
		t.Fatalf("conflicting claim error = %v, want ErrPaymentHashAlreadyClaimed", err)
	}
}

func TestClaimPaymentConfirmationRejectsOrderWhosePaymentTermsChanged(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	verifiedOrder := newPaymentClaimTestOrder("changed-payment-terms", now)
	verifiedOrder.TradeType = UsdtBep20
	verifiedOrder.Address = "0x1111111111111111111111111111111111111111"
	verifiedOrder.MatchAddress = verifiedOrder.Address
	if err := Db.Create(&verifiedOrder).Error; err != nil {
		t.Fatalf("create verified order snapshot: %v", err)
	}

	if err := Db.Model(&Order{}).Where("id = ?", verifiedOrder.ID).Updates(map[string]any{
		"trade_type":    UsdcBep20,
		"crypto":        USDC,
		"rate":          "6.00",
		"amount":        "76.93",
		"address":       "0x2222222222222222222222222222222222222222",
		"match_address": "0x2222222222222222222222222222222222222222",
		"expired_at":    now.Add(20 * time.Minute),
	}).Error; err != nil {
		t.Fatalf("reselect payment terms: %v", err)
	}

	_, err := ClaimPaymentConfirmation(&verifiedOrder, PaymentConfirmation{
		BlockNum: 100,
		From:     "0x3333333333333333333333333333333333333333",
		Hash:     strings.Repeat("a", 64),
		At:       now.Add(time.Second),
		Amount:   decimal.RequireFromString("65.94"),
	})
	if !errors.Is(err, ErrOrderNoLongerReceivable) {
		t.Fatalf("claim after payment terms changed error = %v, want ErrOrderNoLongerReceivable", err)
	}

	var refreshed Order
	if err := Db.First(&refreshed, verifiedOrder.ID).Error; err != nil {
		t.Fatalf("reload reselected order: %v", err)
	}
	if refreshed.Status != OrderStatusWaiting || refreshed.RefHash != "" {
		t.Fatalf("reselected order must remain unclaimed: %+v", refreshed)
	}
	var claimCount int64
	if err := Db.Model(&PaymentHashClaim{}).Where("order_id = ?", verifiedOrder.ID).Count(&claimCount).Error; err != nil {
		t.Fatalf("count payment claims: %v", err)
	}
	if claimCount != 0 {
		t.Fatalf("payment claim count = %d, want 0", claimCount)
	}
}

func TestApplyClaimedPaymentConfirmationDoesNotOverwriteNonReceivableOrder(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	stored := newPaymentClaimTestOrder("non-receivable-payment-claim", now)
	stored.Status = OrderStatusConfirming
	stored.RefHash = "existing-transaction"
	if err := Db.Create(&stored).Error; err != nil {
		t.Fatalf("create order: %v", err)
	}

	stale := stored
	stale.Status = OrderStatusWaiting
	err := applyClaimedPaymentConfirmation(Db, &stale, PaymentConfirmation{
		BlockNum: 2,
		From:     "new-sender",
		Hash:     "new-transaction",
		At:       now.Add(time.Second),
		Amount:   decimal.RequireFromString("1.00"),
	})
	if !errors.Is(err, ErrOrderNoLongerReceivable) {
		t.Fatalf("apply stale payment confirmation error = %v, want ErrOrderNoLongerReceivable", err)
	}

	var refreshed Order
	if err := Db.First(&refreshed, stored.ID).Error; err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if refreshed.Status != OrderStatusConfirming || refreshed.RefHash != "existing-transaction" {
		t.Fatalf("non-receivable order was overwritten: %+v", refreshed)
	}
}

func TestClaimPaymentConfirmationProtectsLegacyRawSolanaClaimWithoutOrder(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	order := newPaymentClaimTestOrder("legacy-raw-solana-claim", now)
	order.TradeType = UsdcSolana
	if err := Db.Create(&order).Error; err != nil {
		t.Fatalf("create order: %v", err)
	}

	const signature = "2gqC3gYGfdNMQkF5xhZXmofbjuu3RbdZZrKz7pYvuArMpqgHSvvrQb25AuDVvtsswxkfjWZbDDouH1FBFWwgYkD4"
	if err := Db.Create(&PaymentHashClaim{Hash: signature, OrderID: order.ID + 100}).Error; err != nil {
		t.Fatalf("seed legacy raw Solana claim: %v", err)
	}

	_, err := ClaimPaymentConfirmation(&order, PaymentConfirmation{
		BlockNum: 1,
		From:     "sender",
		Hash:     signature,
		At:       now,
		Amount:   decimal.RequireFromString("1.00"),
	})
	if !errors.Is(err, ErrPaymentHashAlreadyClaimed) {
		t.Fatalf("legacy raw Solana claim error = %v, want ErrPaymentHashAlreadyClaimed", err)
	}
}

func TestShouldNotRetryPaymentClaimForeignCodedError(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	for _, tc := range []struct {
		name string
		code int
		want bool
	}{
		{name: "busy", code: 5, want: false},
		{name: "busy snapshot", code: 517, want: false},
		{name: "constraint", code: 19, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := fmt.Errorf("wrapped SQLite error: %w", paymentClaimTestSQLiteError{code: tc.code})
			if got := shouldRetryPaymentClaimSQLiteBusy(err); got != tc.want {
				t.Fatalf("should retry error code %d = %t, want %t", tc.code, got, tc.want)
			}
		})
	}
}

func TestShouldRetryPaymentClaimSQLiteBusyError(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "bepusdt.db"), "", ""); err != nil {
		t.Fatalf("initialize test database: %v", err)
	}

	now := time.Now().UTC()
	order := newPaymentClaimTestOrder("sqlite-busy-claim", now)
	if err := Db.Create(&order).Error; err != nil {
		t.Fatalf("create order: %v", err)
	}

	stale := Db.Begin()
	if stale.Error != nil {
		t.Fatalf("begin stale transaction: %v", stale.Error)
	}
	defer stale.Rollback()
	var read Order
	if err := stale.First(&read, order.ID).Error; err != nil {
		t.Fatalf("read order in stale transaction: %v", err)
	}

	writer := Db.Begin()
	if writer.Error != nil {
		t.Fatalf("begin writer transaction: %v", writer.Error)
	}
	if err := writer.Create(&PaymentHashClaim{Hash: "sqlite-busy-existing", OrderID: order.ID}).Error; err != nil {
		writer.Rollback()
		t.Fatalf("create competing payment claim: %v", err)
	}
	if err := writer.Commit().Error; err != nil {
		t.Fatalf("commit competing payment claim: %v", err)
	}

	err := stale.Create(&PaymentHashClaim{Hash: "sqlite-busy-stale", OrderID: order.ID + 1}).Error
	if err == nil {
		t.Fatal("stale transaction write unexpectedly succeeded")
	}
	var sqliteErr *gosqlite.Error
	if !errors.As(err, &sqliteErr) {
		t.Fatalf("stale transaction error type = %T, want wrapped *go-sqlite.Error", err)
	}
	if sqliteErr.Code()&0xff != sqlite3.SQLITE_BUSY {
		t.Fatalf("stale transaction error code = %d, want SQLite BUSY primary code", sqliteErr.Code())
	}
	if !shouldRetryPaymentClaimSQLiteBusy(err) {
		t.Fatalf("should retry actual SQLite busy error = false, error: %v", err)
	}
}

type paymentClaimTestSQLiteError struct {
	code int
}

func (err paymentClaimTestSQLiteError) Error() string {
	return "SQLite test error"
}

func (err paymentClaimTestSQLiteError) Code() int {
	return err.code
}

func newPaymentClaimTestOrder(tradeID string, now time.Time) Order {
	return Order{
		OrderId:      tradeID,
		TradeId:      tradeID,
		TradeType:    UsdtTrc20,
		Fiat:         CNY,
		Crypto:       USDT,
		Rate:         "7.00",
		Amount:       "65.94",
		Money:        "461.58",
		Address:      "TKNUJShSXfCDii9bxdguPwgZsKXG6rBLC6",
		MatchAddress: "TKNUJShSXfCDii9bxdguPwgZsKXG6rBLC6",
		Status:       OrderStatusWaiting,
		ApiType:      OrderApiTypeEpusdt,
		ExpiredAt:    now.Add(10 * time.Minute),
		ConfirmedAt:  &now,
		AutoTimeAt:   AutoTimeAt{CreatedAt: (*Datetime)(&now), UpdatedAt: (*Datetime)(&now)},
	}
}
