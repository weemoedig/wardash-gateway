package models

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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

func DeleteTransactionsBefore(db *gorm.DB, cutoff time.Time) (int64, error) {
	const batchSize = 1000
	var total int64

	for {
		result := db.Exec(
			`with expired as (
			   select id
			     from transactions
			    where created_at < ?
			    order by created_at
			    limit ?
			 )
			 delete from transactions as transactions_to_delete
			  using expired
			  where transactions_to_delete.id = expired.id`,
			cutoff.UTC(),
			batchSize,
		)
		if result.Error != nil {
			return total, result.Error
		}

		total += result.RowsAffected
		if result.RowsAffected < batchSize {
			return total, nil
		}
	}
}

func FindExistingTransactionIDs(db *gorm.DB, ids []string) (map[string]struct{}, error) {
	existing := make(map[string]struct{})
	if len(ids) == 0 {
		return existing, nil
	}

	var rows []string
	err := db.Model(&Transaction{}).
		Where("id in ?", ids).
		Pluck("id", &rows).
		Error
	if err != nil {
		return nil, err
	}

	for _, id := range rows {
		existing[id] = struct{}{}
	}

	return existing, nil
}

type parsedTransactionJSON struct {
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

func parseTransactionJSON(
	raw json.RawMessage,
) (Transaction, []TransactionParticipant, *MarketContribution, error) {
	var parsed parsedTransactionJSON
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Transaction{}, nil, nil, fmt.Errorf("failed to parse transaction JSON: %w", err)
	}
	parsed.ID = strings.TrimSpace(parsed.ID)
	if parsed.ID == "" {
		return Transaction{}, nil, nil, fmt.Errorf("failed to parse transaction JSON: missing _id")
	}
	if parsed.CreatedAt.IsZero() {
		return Transaction{}, nil, nil, fmt.Errorf(
			"failed to parse transaction JSON %s: missing createdAt",
			parsed.ID,
		)
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

	contribution, err := MarketContributionFromJSON(raw)
	if err != nil {
		return Transaction{}, nil, nil, err
	}
	txn := Transaction{
		ID:              parsed.ID,
		TransactionType: parsed.TransactionType,
		ItemCode:        parsed.ItemCode,
		Data:            datatypes.JSON(raw),
		CreatedAt:       parsed.CreatedAt.Truncate(time.Second),
		UpdatedAt:       parsed.UpdatedAt.Truncate(time.Second),
	}
	return txn, participants, contribution, nil
}

func insertTransactionFromJSON(
	db *gorm.DB,
	raw json.RawMessage,
	updateExisting bool,
) (bool, error) {
	txn, participants, contribution, err := parseTransactionJSON(raw)
	if err != nil {
		return false, err
	}

	inserted := false
	err = db.Transaction(func(tx *gorm.DB) error {
		result := tx.
			Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "id"}},
				DoNothing: true,
			}).
			Omit(clause.Associations).
			Create(&txn)
		if result.Error != nil {
			return result.Error
		}

		if result.RowsAffected == 1 {
			inserted = true
			if len(participants) > 0 {
				if err := tx.Create(&participants).Error; err != nil {
					return err
				}
			}
			if contribution != nil {
				if err := IncrementMarketDailyRollup(tx, txn.CreatedAt, *contribution); err != nil {
					return fmt.Errorf("increment market rollup for transaction %s: %w", txn.ID, err)
				}
			}
			return nil
		}

		if !updateExisting {
			return nil
		}
		if err := tx.Model(&Transaction{}).
			Where("id = ?", txn.ID).
			Updates(map[string]any{
				"transaction_type": txn.TransactionType,
				"item_code":        txn.ItemCode,
				"data":             txn.Data,
				"created_at":       txn.CreatedAt,
				"updated_at":       txn.UpdatedAt,
			}).
			Error; err != nil {
			return err
		}
		if err := tx.Where("transaction_id = ?", txn.ID).
			Delete(&TransactionParticipant{}).
			Error; err != nil {
			return err
		}
		if len(participants) > 0 {
			return tx.Create(&participants).Error
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return inserted, nil
}

func InsertTransactionFromJSON(db *gorm.DB, raw json.RawMessage) (bool, error) {
	return insertTransactionFromJSON(db, raw, false)
}

func CreateTransactionFromJSON(db *gorm.DB, raw json.RawMessage) error {
	_, err := InsertTransactionFromJSON(db, raw)
	return err
}

func UpsertTransactionFromJSON(db *gorm.DB, raw json.RawMessage) error {
	_, err := insertTransactionFromJSON(db, raw, true)
	return err
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
