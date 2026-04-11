package models

import (
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type WorkOffer struct {
	ID            string         `gorm:"primaryKey;type:char(24)"`
	UserID        string         `gorm:"not null;type:char(24)"`
	RegionID      string         `gorm:"not null;type:char(24)"`
	MinEnergy     int            `gorm:"not null"`
	MinProduction int            `gorm:"not null"`
	Data          datatypes.JSON `gorm:"type:jsonb;not null"`
	CreatedAt     time.Time      `gorm:"not null;autoCreateTime:false"`
	UpdatedAt     time.Time      `gorm:"not null;autoUpdateTime:false"`
}

func AddWorkOfferIndexes(db *gorm.DB) {
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_work_offers_created_at_id_desc ON work_offers (created_at DESC, id DESC)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_work_offers_user_id ON work_offers (user_id)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_work_offers_region_id ON work_offers (region_id)`)
}

func CreateWorkOfferFromJSON(db *gorm.DB, raw json.RawMessage) error {
	var parsed struct {
		ID            string    `json:"_id"`
		User          string    `json:"user"`
		Region        string    `json:"region"`
		MinEnergy     int       `json:"minEnergy"`
		MinProduction int       `json:"minProduction"`
		CreatedAt     time.Time `json:"createdAt"`
		UpdatedAt     time.Time `json:"updatedAt"`
	}

	err := json.Unmarshal(raw, &parsed)
	if err != nil {
		return fmt.Errorf("failed to parse work offer JSON: %w", err)
	}

	offer := WorkOffer{
		ID:            parsed.ID,
		UserID:        parsed.User,
		RegionID:      parsed.Region,
		MinEnergy:     parsed.MinEnergy,
		MinProduction: parsed.MinProduction,
		Data:          datatypes.JSON(raw),
		CreatedAt:     parsed.CreatedAt,
		UpdatedAt:     parsed.UpdatedAt,
	}

	return db.Create(&offer).Error
}

type WorkOfferQuery struct {
	Limit      int
	Cursor     string
	UserID     string
	RegionID   string
	Energy     int
	Production int
}

type WorkOfferResult struct {
	Data   []json.RawMessage
	Cursor string
}

func QueryWorkOffers(db *gorm.DB, q WorkOfferQuery) (*WorkOfferResult, error) {
	if q.Limit <= 0 {
		q.Limit = 10
	}
	if q.Limit > 100 {
		q.Limit = 100
	}

	query := db.Model(&WorkOffer{}).Select("id, data, created_at")

	if q.Cursor != "" {
		cursorTime, cursorID, err := ParseCursor(q.Cursor)
		if err != nil {
			return nil, err
		}
		query = query.Where("(created_at, id) < (?, ?)", cursorTime, cursorID)
	}

	if q.UserID != "" {
		query = query.Where("user_id = ?", q.UserID)
	}

	if q.RegionID != "" {
		query = query.Where("region_id = ?", q.RegionID)
	}

	if q.Energy > 0 {
		query = query.Where("min_energy <= ?", q.Energy)
	}

	if q.Production > 0 {
		query = query.Where("min_production <= ?", q.Production)
	}

	var offers []WorkOffer
	err := query.Order("created_at DESC, id DESC").Limit(q.Limit).Find(&offers).Error
	if err != nil {
		return nil, err
	}

	result := &WorkOfferResult{
		Data: make([]json.RawMessage, len(offers)),
	}

	for i, o := range offers {
		result.Data[i] = json.RawMessage(o.Data)
	}

	if len(offers) == q.Limit {
		last := offers[len(offers)-1]
		result.Cursor = FormatCursor(last.CreatedAt, last.ID)
	}

	return result, nil
}
