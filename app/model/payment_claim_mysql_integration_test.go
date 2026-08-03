package model

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	drivermysql "github.com/go-sql-driver/mysql"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func TestClaimPaymentConfirmationMySQLClientFoundRows(t *testing.T) {
	dsn := os.Getenv("BEPUSDT_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("BEPUSDT_TEST_MYSQL_DSN is not set")
	}

	config, err := drivermysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse BEPUSDT_TEST_MYSQL_DSN: %v", err)
	}
	if !config.ClientFoundRows {
		t.Fatal("BEPUSDT_TEST_MYSQL_DSN must enable clientFoundRows=true")
	}
	if !config.ParseTime {
		t.Fatal("BEPUSDT_TEST_MYSQL_DSN must enable parseTime=true")
	}
	if err := Init("", dsn, ""); err != nil {
		t.Fatalf("initialize MySQL test database: %v", err)
	}
	t.Cleanup(Close)

	now := time.Now().UTC()
	testID := fmt.Sprintf("mysql-client-found-rows-%d", now.UnixNano())
	first := newPaymentClaimTestOrder(testID+"-first", now)
	second := newPaymentClaimTestOrder(testID+"-second", now)
	if err := Db.Create(&first).Error; err != nil {
		t.Fatalf("create first order: %v", err)
	}
	if err := Db.Create(&second).Error; err != nil {
		t.Fatalf("create second order: %v", err)
	}
	// MySQL 5.7 stores DATETIME without the nanosecond precision carried by
	// time.Now. Claim verification normally receives this database snapshot.
	if err := Db.First(&second, second.ID).Error; err != nil {
		t.Fatalf("reload second order: %v", err)
	}
	payment := PaymentConfirmation{
		BlockNum: 100,
		From:     "TJ5usJLLwjwn7Pw3TPbdzreG7dvgKzfQ5y",
		Hash:     fmt.Sprintf("%064x", now.UnixNano()),
		At:       now.Add(time.Second),
		Amount:   decimal.RequireFromString("65.94"),
	}
	claimHash, err := paymentHashClaimKey(first.TradeType, payment.Hash)
	if err != nil {
		t.Fatalf("build payment claim key: %v", err)
	}
	t.Cleanup(func() {
		if err := Db.Where("hash = ?", claimHash).Delete(&PaymentHashClaim{}).Error; err != nil {
			t.Errorf("delete payment claim: %v", err)
		}
		if err := Db.Where("id IN ?", []int64{first.ID, second.ID}).Delete(&Order{}).Error; err != nil {
			t.Errorf("delete test orders: %v", err)
		}
	})

	const (
		injectCompetingClaimCallback = "test:mysql_payment_claim_inject_competitor"
		observeFoundRowsCallback     = "test:mysql_payment_claim_observe_found_rows"
	)
	var injectedCompetingClaim bool
	var observedDuplicateCreate bool
	var duplicateRowsAffected int64
	if err := Db.Callback().Create().Before("gorm:create").Register(injectCompetingClaimCallback, func(tx *gorm.DB) {
		if injectedCompetingClaim || tx.Statement.Schema == nil || tx.Statement.Schema.Table != (PaymentHashClaim{}).TableName() {
			return
		}
		// Keep this insert in the same ClaimPaymentConfirmation transaction so
		// its initial lookup has already observed no row, while Exec avoids
		// recursively entering the GORM Create callback chain.
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
	if err := Db.Callback().Create().After("gorm:create").Register(observeFoundRowsCallback, func(tx *gorm.DB) {
		if injectedCompetingClaim && tx.Statement.Schema != nil && tx.Statement.Schema.Table == (PaymentHashClaim{}).TableName() {
			observedDuplicateCreate = true
			duplicateRowsAffected = tx.RowsAffected
		}
	}); err != nil {
		t.Fatalf("register found-rows observer: %v", err)
	}
	t.Cleanup(func() {
		if err := Db.Callback().Create().Remove(injectCompetingClaimCallback); err != nil {
			t.Errorf("remove competing-claim callback: %v", err)
		}
		if err := Db.Callback().Create().Remove(observeFoundRowsCallback); err != nil {
			t.Errorf("remove found-rows observer: %v", err)
		}
	})

	if _, err := ClaimPaymentConfirmation(&second, payment); !errors.Is(err, ErrPaymentHashAlreadyClaimed) {
		t.Fatalf("claim second order with duplicate hash error = %v, want ErrPaymentHashAlreadyClaimed", err)
	}
	if !injectedCompetingClaim || !observedDuplicateCreate {
		t.Fatal("expected the competing claim and duplicate Create callbacks to run")
	}
	if duplicateRowsAffected != 1 {
		t.Fatalf("duplicate claim RowsAffected = %d, want 1 with clientFoundRows=true", duplicateRowsAffected)
	}

	var refreshed Order
	if err := Db.First(&refreshed, second.ID).Error; err != nil {
		t.Fatalf("reload second order: %v", err)
	}
	if refreshed.Status != OrderStatusWaiting || refreshed.RefHash != "" {
		t.Fatalf("second order must remain unclaimed: %+v", refreshed)
	}
}
