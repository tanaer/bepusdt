package model

import (
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil/base58"
	gosqlite "github.com/glebarez/go-sqlite"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	sqlite3 "modernc.org/sqlite/lib"
)

var (
	// ErrPaymentHashAlreadyClaimed means that the same on-chain transaction has
	// already been associated with a different order.
	ErrPaymentHashAlreadyClaimed = errors.New("payment hash is already claimed by another order")
	// ErrOrderNoLongerReceivable means that an order cannot accept a new payment.
	ErrOrderNoLongerReceivable = errors.New("order is no longer receivable")
)

const (
	paymentClaimSQLiteBusyMaxAttempts = 3
	paymentClaimSQLiteBusyRetryDelay  = 5 * time.Millisecond
)

// PaymentHashClaim is a durable, cross-process uniqueness guard. A payment
// transaction may only be associated with one local order.
type PaymentHashClaim struct {
	Hash    string `gorm:"column:hash;type:varchar(128);primaryKey;not null" json:"hash"`
	OrderID int64  `gorm:"column:order_id;not null;index" json:"order_id"`
	AutoTimeAt
}

func (PaymentHashClaim) TableName() string {
	return "bep_payment_hash_claim"
}

type PaymentConfirmation struct {
	BlockNum int
	From     string
	Hash     string
	At       time.Time
	Amount   decimal.Decimal
}

// PaymentHashEqual compares transaction identifiers according to the chain
// that produced them. Solana signatures are case-sensitive Base58 values;
// hex-based chains retain their historical case-insensitive behavior.
func PaymentHashEqual(tradeType TradeType, left, right string) bool {
	return canonicalPaymentHash(tradeType, left) == canonicalPaymentHash(tradeType, right)
}

func canonicalPaymentHash(tradeType TradeType, hash string) string {
	hash = strings.TrimSpace(hash)
	switch {
	case isSolanaPaymentTrade(tradeType):
		return hash
	case isTronPaymentTrade(tradeType):
		return strings.TrimPrefix(strings.ToLower(hash), "0x")
	case isBscPaymentTrade(tradeType):
		return "0x" + strings.TrimPrefix(strings.ToLower(hash), "0x")
	default:
		return strings.ToLower(hash)
	}
}

func paymentHashClaimKey(tradeType TradeType, hash string) (string, error) {
	hash = strings.TrimSpace(hash)
	if !isSolanaPaymentTrade(tradeType) {
		return canonicalPaymentHash(tradeType, hash), nil
	}

	decoded := base58.Decode(hash)
	if len(decoded) != 64 || base58.Encode(decoded) != hash {
		return "", fmt.Errorf("claim payment confirmation: invalid Solana transaction hash")
	}

	// MySQL commonly uses a case-insensitive collation. Base32 encodes the
	// signature bytes into a lowercase-only key so two distinct Base58 values
	// cannot collide merely because their letters differ in case.
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(decoded)
	return "solana:" + strings.ToLower(encoded), nil
}

func isSolanaPaymentTrade(tradeType TradeType) bool {
	return tradeType == UsdtSolana || tradeType == UsdcSolana
}

func isTronPaymentTrade(tradeType TradeType) bool {
	return tradeType == TronTrx || tradeType == UsdtTrc20 || tradeType == UsdcTrc20
}

func isBscPaymentTrade(tradeType TradeType) bool {
	return tradeType == BscBnb || tradeType == UsdtBep20 || tradeType == UsdcBep20
}

func legacyPaymentHashLookupVariants(tradeType TradeType, hash string) []string {
	canonical := canonicalPaymentHash(tradeType, hash)
	switch {
	case isTronPaymentTrade(tradeType):
		return []string{canonical, "0x" + canonical}
	case isBscPaymentTrade(tradeType):
		return []string{canonical, strings.TrimPrefix(canonical, "0x")}
	default:
		return []string{canonical}
	}
}

func legacyPaymentHashAlreadyClaimed(tx *gorm.DB, order Order, hash string) error {
	var candidates []Order
	query := tx.Where("id <> ?", order.ID)
	if isSolanaPaymentTrade(order.TradeType) {
		// A MySQL default collation may return case variants for this query.
		// Therefore the result must always be compared byte-for-byte in Go.
		query = query.Where("ref_hash = ?", strings.TrimSpace(hash))
	} else {
		query = query.Where("LOWER(TRIM(ref_hash)) IN ?", legacyPaymentHashLookupVariants(order.TradeType, hash))
	}
	if err := query.Find(&candidates).Error; err != nil {
		return err
	}
	for _, candidate := range candidates {
		if PaymentHashEqual(order.TradeType, candidate.RefHash, hash) {
			return ErrPaymentHashAlreadyClaimed
		}
	}
	return nil
}

func legacyRawSolanaPaymentHashAlreadyClaimed(tx *gorm.DB, hash string) error {
	var claims []PaymentHashClaim
	// Earlier versions stored raw Base58 signatures as primary keys. A
	// case-insensitive collation can return case variants here, so verify the
	// exact signature in Go before treating the old row as a collision.
	if err := tx.Where("hash = ?", strings.TrimSpace(hash)).Find(&claims).Error; err != nil {
		return err
	}
	for _, claim := range claims {
		if claim.Hash == strings.TrimSpace(hash) {
			return ErrPaymentHashAlreadyClaimed
		}
	}
	return nil
}

func applyClaimedPaymentConfirmation(tx *gorm.DB, order *Order, payment PaymentConfirmation) error {
	updates := map[string]any{
		"from_address":  payment.From,
		"confirmed_at":  payment.At,
		"ref_hash":      strings.TrimSpace(payment.Hash),
		"ref_block_num": payment.BlockNum,
		"status":        OrderStatusConfirming,
	}
	if order.AddressLocked {
		rate, _ := decimal.NewFromString(order.Rate)
		updates["amount"] = payment.Amount.String()
		updates["money"] = rate.Mul(payment.Amount).String()
	}

	result := tx.Model(&Order{}).
		Where("id = ? AND status IN ?", order.ID, []int{OrderStatusWaiting, OrderStatusExpired}).
		Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrOrderNoLongerReceivable
	}

	order.FromAddress = payment.From
	order.ConfirmedAt = &payment.At
	order.RefHash = strings.TrimSpace(payment.Hash)
	order.RefBlockNum = payment.BlockNum
	order.Status = OrderStatusConfirming
	if order.AddressLocked {
		rate, _ := decimal.NewFromString(order.Rate)
		order.Amount = payment.Amount.String()
		order.Money = rate.Mul(payment.Amount).String()
	}

	return nil
}

func shouldRetryPaymentClaimSQLiteBusy(err error) bool {
	if err == nil || Db == nil || Db.Dialector == nil || Db.Dialector.Name() != "sqlite" {
		return false
	}

	var sqliteErr *gosqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	// SQLite extended result codes retain the primary result code in the low
	// byte, so this accepts both SQLITE_BUSY and SQLITE_BUSY_SNAPSHOT.
	return sqliteErr.Code()&0xff == sqlite3.SQLITE_BUSY
}

// paymentConfirmationTermsEqual prevents a transfer verified against one
// payment method from being applied after the waiting order was reselected to
// another method while the chain RPC request was in flight.
func paymentConfirmationTermsEqual(expected, current Order) bool {
	return expected.TradeType == current.TradeType &&
		expected.Crypto == current.Crypto &&
		expected.Fiat == current.Fiat &&
		expected.Rate == current.Rate &&
		expected.Amount == current.Amount &&
		expected.Money == current.Money &&
		expected.Address == current.Address &&
		expected.MatchAddress == current.MatchAddress &&
		expected.AddressLocked == current.AddressLocked &&
		expected.ExpiredAt.Equal(current.ExpiredAt)
}

func findPaymentHashClaim(tx *gorm.DB, hash string) (PaymentHashClaim, bool, error) {
	var claim PaymentHashClaim
	err := tx.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate}).
		Where("hash = ?", hash).First(&claim).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return PaymentHashClaim{}, false, nil
	}
	if err != nil {
		return PaymentHashClaim{}, false, err
	}
	return claim, true, nil
}

func resolveExistingPaymentHashClaim(tx *gorm.DB, order *Order, originalHash, claimHash string) (bool, error) {
	existing, found, err := findPaymentHashClaim(tx, claimHash)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	if existing.OrderID != order.ID {
		return false, ErrPaymentHashAlreadyClaimed
	}
	if err := tx.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate}).
		Where("id = ?", order.ID).First(order).Error; err != nil {
		return false, err
	}
	if PaymentHashEqual(order.TradeType, order.RefHash, originalHash) {
		return true, nil
	}
	return false, fmt.Errorf("%w: existing order claim does not match transaction hash", ErrPaymentHashAlreadyClaimed)
}

// ClaimPaymentConfirmation atomically reserves a transaction hash and moves a
// waiting (or lookback-expired) order into the existing confirming workflow.
// Repeating a successful claim for the same order is intentionally idempotent.
func ClaimPaymentConfirmation(order *Order, payment PaymentConfirmation) (alreadyClaimed bool, err error) {
	if order == nil || order.ID == 0 {
		return false, fmt.Errorf("claim payment confirmation: order is required")
	}

	originalHash := strings.TrimSpace(payment.Hash)
	if originalHash == "" {
		return false, fmt.Errorf("claim payment confirmation: transaction hash is required")
	}
	expectedOrder := *order

	for attempt := 0; attempt < paymentClaimSQLiteBusyMaxAttempts; attempt++ {
		var updated Order
		alreadyClaimed = false
		err = Db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate}).Where("id = ?", order.ID).First(&updated).Error; err != nil {
				return err
			}
			if !paymentConfirmationTermsEqual(expectedOrder, updated) {
				return fmt.Errorf("%w: payment terms changed after transaction verification", ErrOrderNoLongerReceivable)
			}
			claimHash, err := paymentHashClaimKey(updated.TradeType, originalHash)
			if err != nil {
				return err
			}

			if updated.RefHash != "" && PaymentHashEqual(updated.TradeType, updated.RefHash, originalHash) &&
				(updated.Status == OrderStatusConfirming || updated.Status == OrderStatusSuccess) {
				alreadyClaimed = true
				return nil
			}
			if updated.Status != OrderStatusWaiting && updated.Status != OrderStatusExpired {
				return ErrOrderNoLongerReceivable
			}
			// Records created before payment-hash claims were introduced do not have
			// a claim row. Check the order table as a migration-safe fallback before
			// reserving this hash for a new order.
			if err := legacyPaymentHashAlreadyClaimed(tx, updated, originalHash); err != nil {
				return err
			}
			if isSolanaPaymentTrade(updated.TradeType) {
				if err := legacyRawSolanaPaymentHashAlreadyClaimed(tx, originalHash); err != nil {
					return err
				}
			}

			if already, err := resolveExistingPaymentHashClaim(tx, &updated, originalHash, claimHash); err != nil {
				return err
			} else if already {
				alreadyClaimed = true
				return nil
			}

			claim := PaymentHashClaim{Hash: claimHash, OrderID: updated.ID}
			create := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&claim)
			if create.Error != nil {
				return create.Error
			}
			// Do not infer ownership from RowsAffected. MySQL's
			// clientFoundRows=true reports a duplicate-key no-op update as one
			// matching row. The post-insert lookup is the authoritative owner
			// check for every SQL dialect.
			existing, found, err := findPaymentHashClaim(tx, claimHash)
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("claim payment confirmation: payment claim disappeared after insert")
			}
			if existing.OrderID != updated.ID {
				return ErrPaymentHashAlreadyClaimed
			}

			return applyClaimedPaymentConfirmation(tx, &updated, payment)
		})
		if err == nil {
			*order = updated
			return alreadyClaimed, nil
		}
		if !shouldRetryPaymentClaimSQLiteBusy(err) || attempt == paymentClaimSQLiteBusyMaxAttempts-1 {
			return false, err
		}
		time.Sleep(time.Duration(attempt+1) * paymentClaimSQLiteBusyRetryDelay)
	}

	return false, err
}
