package models

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type Article struct {
	ID        string         `gorm:"primaryKey;type:char(24)"`
	AuthorID  string         `gorm:"not null;type:char(24)"`
	Category  string         `gorm:"not null"`
	Language  string         `gorm:"not null"`
	Likes     int            `gorm:"not null"`
	Score     int            `gorm:"not null"`
	Data      datatypes.JSON `gorm:"type:jsonb;not null"`
	CreatedAt time.Time      `gorm:"not null;autoCreateTime:false"`
	UpdatedAt time.Time      `gorm:"not null;autoUpdateTime:false"`
}

func AddArticleIndexes(db *gorm.DB) {
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_articles_created_at_id_desc ON articles (created_at DESC, id DESC)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_articles_likes_id_desc ON articles (likes DESC, id DESC)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_articles_author_id ON articles (author_id)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_articles_category ON articles (category)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_articles_language ON articles (language)`)
}

func ParseScoreCursor(cursor string) (int, string, error) {
	parts := strings.SplitN(cursor, "|", 2)
	if len(parts) != 2 {
		return 0, "", fmt.Errorf("invalid score cursor format")
	}
	score, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, "", fmt.Errorf("invalid score cursor value: %w", err)
	}
	return score, parts[1], nil
}

func FormatScoreCursor(score int, id string) string {
	return strconv.Itoa(score) + "|" + id
}

func CreateArticleFromJSON(db *gorm.DB, raw json.RawMessage) error {
	var parsed struct {
		ID       string `json:"_id"`
		Author   string `json:"author"`
		Category string `json:"category"`
		Language string `json:"language"`
		Stats    struct {
			Likes int `json:"likes"`
			Score int `json:"score"`
		} `json:"stats"`
		CreatedAt time.Time `json:"createdAt"`
		UpdatedAt time.Time `json:"updatedAt"`
	}

	err := json.Unmarshal(raw, &parsed)
	if err != nil {
		return fmt.Errorf("failed to parse article JSON: %w", err)
	}

	article := Article{
		ID:        parsed.ID,
		AuthorID:  parsed.Author,
		Category:  parsed.Category,
		Language:  parsed.Language,
		Likes:     parsed.Stats.Likes,
		Score:     parsed.Stats.Score,
		Data:      datatypes.JSON(raw),
		CreatedAt: parsed.CreatedAt,
		UpdatedAt: parsed.UpdatedAt,
	}

	return db.Create(&article).Error
}

type ArticleQuery struct {
	Type              string // required: "daily", "weekly", "top", "last"
	Limit             int
	Cursor            string
	UserID            string
	Categories        []string
	Languages         []string
	PositiveScoreOnly bool
}

type ArticleResult struct {
	Data   []json.RawMessage
	Cursor string
}

func QueryArticles(db *gorm.DB, q ArticleQuery) (*ArticleResult, error) {
	if q.Limit <= 0 {
		q.Limit = 10
	}
	if q.Limit > 100 {
		q.Limit = 100
	}

	query := db.Model(&Article{}).Select("id, data, created_at, likes")

	switch q.Type {
	case "daily":
		query = query.Where("created_at > NOW() - INTERVAL '1 day'")
	case "weekly":
		query = query.Where("created_at > NOW() - INTERVAL '7 days'")
	}

	scoreMode := q.Type == "daily" || q.Type == "weekly" || q.Type == "top"

	if q.Cursor != "" {
		if scoreMode {
			cursorLikes, cursorID, err := ParseScoreCursor(q.Cursor)
			if err != nil {
				return nil, err
			}
			query = query.Where("(likes, id) < (?, ?)", cursorLikes, cursorID)
		} else {
			cursorTime, cursorID, err := ParseCursor(q.Cursor)
			if err != nil {
				return nil, err
			}
			query = query.Where("(created_at, id) < (?, ?)", cursorTime, cursorID)
		}
	}

	if q.UserID != "" {
		query = query.Where("author_id = ?", q.UserID)
	}

	if len(q.Categories) > 0 {
		query = query.Where("category IN ?", q.Categories)
	}

	if len(q.Languages) > 0 {
		query = query.Where("language IN ?", q.Languages)
	}

	if q.PositiveScoreOnly {
		query = query.Where("score > 0")
	}

	var orderClause string
	if scoreMode {
		orderClause = "likes DESC, id DESC"
	} else {
		orderClause = "created_at DESC, id DESC"
	}

	var articles []Article
	err := query.Order(orderClause).Limit(q.Limit).Find(&articles).Error
	if err != nil {
		return nil, err
	}

	result := &ArticleResult{
		Data: make([]json.RawMessage, len(articles)),
	}

	for i, a := range articles {
		result.Data[i] = json.RawMessage(a.Data)
	}

	if len(articles) == q.Limit {
		last := articles[len(articles)-1]
		if scoreMode {
			result.Cursor = FormatScoreCursor(last.Likes, last.ID)
		} else {
			result.Cursor = FormatCursor(last.CreatedAt, last.ID)
		}
	}

	return result, nil
}
