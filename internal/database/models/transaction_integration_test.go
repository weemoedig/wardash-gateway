package models

import (
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestDeleteTransactionsBeforeIntegration(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("GATEWAY_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL is not set")
	}

	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse GATEWAY_TEST_DATABASE_URL: %v", err)
	}
	if strings.TrimPrefix(parsed.Path, "/") != "gateway_test" {
		t.Fatalf("refusing integration test database %q; want gateway_test", parsed.Path)
	}

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("connect integration database: %v", err)
	}

	if err := db.Migrator().DropTable(&TransactionParticipant{}, &Transaction{}); err != nil {
		t.Fatalf("reset integration tables: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Migrator().DropTable(&TransactionParticipant{}, &Transaction{})
	})

	if err := db.AutoMigrate(&Transaction{}, &TransactionParticipant{}); err != nil {
		t.Fatalf("migrate integration tables: %v", err)
	}

	now := time.Date(2026, time.July, 30, 20, 0, 0, 0, time.UTC)
	transactions := []Transaction{
		{
			ID:              "aaaaaaaaaaaaaaaaaaaaaaaa",
			TransactionType: "market",
			Data:            datatypes.JSON(`{"_id":"aaaaaaaaaaaaaaaaaaaaaaaa"}`),
			CreatedAt:       now.Add(-31 * 24 * time.Hour),
			UpdatedAt:       now.Add(-31 * 24 * time.Hour),
			Participants: []TransactionParticipant{
				{
					TransactionID: "aaaaaaaaaaaaaaaaaaaaaaaa",
					EntityID:      "seller-old",
					EntityType:    "user",
					Role:          "seller",
				},
			},
		},
		{
			ID:              "bbbbbbbbbbbbbbbbbbbbbbbb",
			TransactionType: "market",
			Data:            datatypes.JSON(`{"_id":"bbbbbbbbbbbbbbbbbbbbbbbb"}`),
			CreatedAt:       now.Add(-24 * time.Hour),
			UpdatedAt:       now.Add(-24 * time.Hour),
			Participants: []TransactionParticipant{
				{
					TransactionID: "bbbbbbbbbbbbbbbbbbbbbbbb",
					EntityID:      "buyer-new",
					EntityType:    "user",
					Role:          "buyer",
				},
			},
		},
	}
	if err := db.Create(&transactions).Error; err != nil {
		t.Fatalf("insert integration fixtures: %v", err)
	}

	deleted, err := DeleteTransactionsBefore(db, now.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("DeleteTransactionsBefore returned error: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}

	var transactionCount int64
	if err := db.Model(&Transaction{}).Count(&transactionCount).Error; err != nil {
		t.Fatalf("count transactions: %v", err)
	}
	if transactionCount != 1 {
		t.Fatalf("transaction count = %d, want 1", transactionCount)
	}

	var participantCount int64
	if err := db.Model(&TransactionParticipant{}).Count(&participantCount).Error; err != nil {
		t.Fatalf("count participants: %v", err)
	}
	if participantCount != 1 {
		t.Fatalf("participant count = %d, want 1", participantCount)
	}

	var orphanCount int64
	if err := db.Raw(
		`select count(*)
		   from transaction_participants as participant
		   left join transactions as transaction
		     on transaction.id = participant.transaction_id
		  where transaction.id is null`,
	).Scan(&orphanCount).Error; err != nil {
		t.Fatalf("count orphan participants: %v", err)
	}
	if orphanCount != 0 {
		t.Fatalf("orphan participant count = %d, want 0", orphanCount)
	}
}
