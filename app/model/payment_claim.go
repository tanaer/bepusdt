package model

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	// ErrPaymentHashAlreadyClaimed means that the same on-chain transaction has
	// already been associated with a different order.
	ErrPaymentHashAlreadyClaimed = errors.New("payment hash is already claimed by another order")
	// ErrOrderNoLongerReceivable means that an order cannot accept a new payment.
	ErrOrderNoLongerReceivable = errors.New("order is no longer receivable")
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
	// Keep Solana's case-sensitive Base58 signature exact both on the order and
	// in the uniqueness claim. Hex transaction identifiers on the other chains
	// remain normalized for migration compatibility.
	claimHash := originalHash
	trade, isKnownTrade := registry[order.TradeType]
	isSolana := isKnownTrade && trade.Network == Network("solana")
	if !isSolana {
		claimHash = strings.ToLower(originalHash)
	}

	var updated Order
	err = Db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("id = ?", order.ID).First(&updated).Error; err != nil {
			return err
		}

		if updated.RefHash != "" && strings.EqualFold(updated.RefHash, originalHash) &&
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
		var legacyOwner Order
		legacyQuery := "LOWER(ref_hash) = ? AND id <> ?"
		if isSolana {
			legacyQuery = "ref_hash = ? AND id <> ?"
		}
		legacyLookup := tx.Where(legacyQuery, claimHash, updated.ID).First(&legacyOwner).Error
		if legacyLookup == nil {
			return ErrPaymentHashAlreadyClaimed
		}
		if legacyLookup != nil && !errors.Is(legacyLookup, gorm.ErrRecordNotFound) {
			return legacyLookup
		}

		claim := PaymentHashClaim{Hash: claimHash, OrderID: updated.ID}
		create := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&claim)
		if create.Error != nil {
			return create.Error
		}
		if create.RowsAffected == 0 {
			var existing PaymentHashClaim
			lookupErr := tx.Where("hash = ?", claimHash).First(&existing).Error
			if lookupErr == nil && existing.OrderID != updated.ID {
				return ErrPaymentHashAlreadyClaimed
			}
			if lookupErr == nil && existing.OrderID == updated.ID && strings.EqualFold(updated.RefHash, originalHash) {
				alreadyClaimed = true
				return nil
			}
			return lookupErr
		}

		updated.FromAddress = payment.From
		updated.ConfirmedAt = &payment.At
		updated.RefHash = originalHash
		updated.RefBlockNum = payment.BlockNum
		updated.Status = OrderStatusConfirming
		if updated.AddressLocked {
			rate, _ := decimal.NewFromString(updated.Rate)
			updated.Amount = payment.Amount.String()
			updated.Money = rate.Mul(payment.Amount).String()
		}

		return tx.Save(&updated).Error
	})
	if err != nil {
		return false, err
	}

	*order = updated
	return alreadyClaimed, nil
}
