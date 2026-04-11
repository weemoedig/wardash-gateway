package models

import (
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type Transaction struct {
	ID              string         `gorm:"primaryKey;type:char(24)"`
	TransactionType string         `gorm:"not null"`
	ItemCode        string         `gorm:"not null;default:''"`
	Data            datatypes.JSON `gorm:"type:jsonb;not null"`
	CreatedAt       time.Time      `gorm:"not null;autoCreateTime:false"`
	UpdatedAt       time.Time      `gorm:"not null;autoUpdateTime:false"`

	Participants []TransactionParticipant `gorm:"foreignKey:TransactionID;constraint:OnDelete:CASCADE"`
}

type TransactionParticipant struct {
	TransactionID string `gorm:"primaryKey;type:char(24)"`
	EntityID      string `gorm:"primaryKey;type:char(24)"`
	EntityType    string `gorm:"primaryKey;type:varchar(10)"` // user, country, mu, party
	Role          string `gorm:"primaryKey;type:varchar(10)"` // seller, buyer
}

func AddTransactionIndexes(db *gorm.DB) {
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_transactions_created_at_id_desc ON transactions (created_at DESC, id DESC)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_transactions_type ON transactions (transaction_type)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_transactions_item_code ON transactions (item_code) WHERE item_code != ''`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_transaction_participants_entity ON transaction_participants (entity_type, entity_id, transaction_id)`)
}

func CreateTransactionFromJSON(db *gorm.DB, raw json.RawMessage) error {
	var parsed struct {
		ID              string    `json:"_id"`
		TransactionType string    `json:"transactionType"`
		ItemCode        string    `json:"itemCode"`
		SellerID        string    `json:"sellerId"`
		BuyerID         string    `json:"buyerId"`
		SellerCountryID string    `json:"sellerCountryId"`
		BuyerCountryID  string    `json:"buyerCountryId"`
		SellerMuID      string    `json:"sellerMuId"`
		BuyerMuID       string    `json:"buyerMuId"`
		SellerPartyID   string    `json:"sellerPartyId"`
		BuyerPartyID    string    `json:"buyerPartyId"`
		CreatedAt       time.Time `json:"createdAt"`
		UpdatedAt       time.Time `json:"updatedAt"`
	}

	err := json.Unmarshal(raw, &parsed)
	if err != nil {
		return fmt.Errorf("failed to parse transaction JSON: %w", err)
	}

	var participants []TransactionParticipant
	add := func(entityID, entityType, role string) {
		if entityID != "" {
			participants = append(participants, TransactionParticipant{
				TransactionID: parsed.ID,
				EntityID:      entityID,
				EntityType:    entityType,
				Role:          role,
			})
		}
	}

	add(parsed.SellerID, "user", "seller")
	add(parsed.BuyerID, "user", "buyer")
	add(parsed.SellerCountryID, "country", "seller")
	add(parsed.BuyerCountryID, "country", "buyer")
	add(parsed.SellerMuID, "mu", "seller")
	add(parsed.BuyerMuID, "mu", "buyer")
	add(parsed.SellerPartyID, "party", "seller")
	add(parsed.BuyerPartyID, "party", "buyer")

	txn := Transaction{
		ID:              parsed.ID,
		TransactionType: parsed.TransactionType,
		ItemCode:        parsed.ItemCode,
		Data:            datatypes.JSON(raw),
		CreatedAt:       parsed.CreatedAt,
		UpdatedAt:       parsed.UpdatedAt,
		Participants:    participants,
	}

	return db.Create(&txn).Error
}

func UpsertTransactionFromJSON(db *gorm.DB, raw json.RawMessage) error {
	var parsed struct {
		ID              string    `json:"_id"`
		TransactionType string    `json:"transactionType"`
		ItemCode        string    `json:"itemCode"`
		SellerID        string    `json:"sellerId"`
		BuyerID         string    `json:"buyerId"`
		SellerCountryID string    `json:"sellerCountryId"`
		BuyerCountryID  string    `json:"buyerCountryId"`
		SellerMuID      string    `json:"sellerMuId"`
		BuyerMuID       string    `json:"buyerMuId"`
		SellerPartyID   string    `json:"sellerPartyId"`
		BuyerPartyID    string    `json:"buyerPartyId"`
		CreatedAt       time.Time `json:"createdAt"`
		UpdatedAt       time.Time `json:"updatedAt"`
	}

	err := json.Unmarshal(raw, &parsed)
	if err != nil {
		return fmt.Errorf("failed to parse transaction JSON: %w", err)
	}

	var participants []TransactionParticipant
	add := func(entityID, entityType, role string) {
		if entityID != "" {
			participants = append(participants, TransactionParticipant{
				TransactionID: parsed.ID,
				EntityID:      entityID,
				EntityType:    entityType,
				Role:          role,
			})
		}
	}

	add(parsed.SellerID, "user", "seller")
	add(parsed.BuyerID, "user", "buyer")
	add(parsed.SellerCountryID, "country", "seller")
	add(parsed.BuyerCountryID, "country", "buyer")
	add(parsed.SellerMuID, "mu", "seller")
	add(parsed.BuyerMuID, "mu", "buyer")
	add(parsed.SellerPartyID, "party", "seller")
	add(parsed.BuyerPartyID, "party", "buyer")

	return db.Transaction(func(tx *gorm.DB) error {
		txn := Transaction{
			ID:              parsed.ID,
			TransactionType: parsed.TransactionType,
			ItemCode:        parsed.ItemCode,
			Data:            datatypes.JSON(raw),
			CreatedAt:       parsed.CreatedAt,
			UpdatedAt:       parsed.UpdatedAt,
		}

		err := tx.Save(&txn).Error
		if err != nil {
			return err
		}

		if len(participants) > 0 {
			tx.Where("transaction_id = ?", parsed.ID).Delete(&TransactionParticipant{})
			return tx.Create(&participants).Error
		}
		return nil
	})
}

type TransactionQuery struct {
	Limit           int
	Cursor          string
	UserID          string
	MuID            string
	CountryID       string
	PartyID         string
	ItemCode        string
	TransactionType string
}

type TransactionResult struct {
	Data   []json.RawMessage
	Cursor string
}

func QueryTransactions(db *gorm.DB, q TransactionQuery) (*TransactionResult, error) {
	if q.Limit <= 0 {
		q.Limit = 10
	}
	if q.Limit > 100 {
		q.Limit = 100
	}

	query := db.Model(&Transaction{}).Select("id, data, created_at")

	if q.Cursor != "" {
		cursorTime, cursorID, err := ParseCursor(q.Cursor)
		if err != nil {
			return nil, err
		}
		query = query.Where("(created_at, id) < (?, ?)", cursorTime, cursorID)
	}

	if q.TransactionType != "" {
		query = query.Where("transaction_type = ?", q.TransactionType)
	}

	if q.ItemCode != "" {
		query = query.Where("item_code = ?", q.ItemCode)
	}

	addEntityFilter := func(entityID, entityType string) {
		if entityID != "" {
			query = query.Where(
				"id IN (SELECT transaction_id FROM transaction_participants WHERE entity_type = ? AND entity_id = ?)",
				entityType, entityID,
			)
		}
	}

	addEntityFilter(q.UserID, "user")
	addEntityFilter(q.MuID, "mu")
	addEntityFilter(q.CountryID, "country")
	addEntityFilter(q.PartyID, "party")

	var txns []Transaction
	err := query.Order("created_at DESC, id DESC").Limit(q.Limit).Find(&txns).Error
	if err != nil {
		return nil, err
	}

	result := &TransactionResult{
		Data: make([]json.RawMessage, len(txns)),
	}

	for i, t := range txns {
		result.Data[i] = json.RawMessage(t.Data)
	}

	if len(txns) == q.Limit {
		last := txns[len(txns)-1]
		result.Cursor = FormatCursor(last.CreatedAt, last.ID)
	}

	return result, nil
}
