package models

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type Event struct {
	ID        string         `gorm:"primaryKey;type:char(24)"`
	EventType string         `gorm:"not null"`
	Data      datatypes.JSON `gorm:"type:jsonb;not null"`
	CreatedAt time.Time      `gorm:"not null;autoCreateTime:false"`
	UpdatedAt time.Time      `gorm:"not null;autoUpdateTime:false"`

	Countries []EventCountry `gorm:"foreignKey:EventID;constraint:OnDelete:CASCADE"`
}

type EventCountry struct {
	EventID   string `gorm:"primaryKey;type:char(24);index:idx_country_event,priority:2"`
	CountryID string `gorm:"primaryKey;type:char(24);index:idx_country_event,priority:1"`
}

func AddEventIndexes(db *gorm.DB) {
	db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_events_created_at_id_desc
		ON events (created_at DESC, id DESC)
	`)

	db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_events_type_created_at_id_desc
		ON events (event_type, created_at DESC, id DESC)
	`)

	db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_event_countries_country_id_event_id
		ON event_countries (country_id, event_id)
	`)
}

const jsDateLayout = "Mon Jan 02 2006 15:04:05 GMT-0700"

func ParseCursor(cursor string) (time.Time, string, error) {
	parts := strings.SplitN(cursor, "|", 2)
	if len(parts) != 2 {
		return time.Time{}, "", fmt.Errorf("invalid cursor format")
	}
	dateStr := parts[0]
	if idx := strings.Index(dateStr, " ("); idx != -1 {
		dateStr = dateStr[:idx]
	}
	t, err := time.Parse(jsDateLayout, dateStr)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("invalid cursor time: %w", err)
	}
	return t, parts[1], nil
}

func FormatCursor(t time.Time, id string) string {
	return t.UTC().Format(jsDateLayout) + " (Coordinated Universal Time)|" + id
}

func CreateEventFromJSON(db *gorm.DB, raw json.RawMessage) error {
	var parsed struct {
		ID        string   `json:"_id"`
		Countries []string `json:"countries"`
		Data      struct {
			Type string `json:"type"`
		} `json:"data"`
		CreatedAt time.Time `json:"createdAt"`
		UpdatedAt time.Time `json:"updatedAt"`
	}

	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("failed to parse event JSON: %w", err)
	}

	countries := make([]EventCountry, len(parsed.Countries))
	for i, c := range parsed.Countries {
		countries[i] = EventCountry{
			EventID:   parsed.ID,
			CountryID: c,
		}
	}

	event := Event{
		ID:        parsed.ID,
		EventType: parsed.Data.Type,
		Data:      datatypes.JSON(raw),
		CreatedAt: parsed.CreatedAt,
		UpdatedAt: parsed.UpdatedAt,
		Countries: countries,
	}

	return db.Create(&event).Error
}

type EventQuery struct {
	Limit      int
	Cursor     string
	CountryID  string
	EventTypes []string
}

type EventResult struct {
	Data   []json.RawMessage
	Cursor string
}

func QueryEvents(db *gorm.DB, q EventQuery) (*EventResult, error) {
	if q.Limit <= 0 {
		q.Limit = 10
	}
	if q.Limit > 100 {
		q.Limit = 100
	}

	query := db.Model(&Event{}).Select("id, data, created_at")

	if q.Cursor != "" {
		cursorTime, cursorID, err := ParseCursor(q.Cursor)
		if err != nil {
			return nil, err
		}
		query = query.Where("(created_at, id) < (?, ?)", cursorTime, cursorID)
	}

	if q.CountryID != "" {
		query = query.Where("id IN (SELECT event_id FROM event_countries WHERE country_id = ?)", q.CountryID)
	}

	if len(q.EventTypes) > 0 {
		query = query.Where("event_type IN ?", q.EventTypes)
	}

	var events []Event
	if err := query.Order("created_at DESC, id DESC").Limit(q.Limit).Find(&events).Error; err != nil {
		return nil, err
	}

	result := &EventResult{
		Data: make([]json.RawMessage, len(events)),
	}

	for i, e := range events {
		result.Data[i] = json.RawMessage(e.Data)
	}

	if len(events) == q.Limit {
		last := events[len(events)-1]
		result.Cursor = FormatCursor(last.CreatedAt, last.ID)
	}

	return result, nil
}
