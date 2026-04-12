package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Hattorius/War-Era-Gateway/internal/database"
	"github.com/Hattorius/War-Era-Gateway/internal/database/models"
	"github.com/Hattorius/War-Era-Gateway/internal/scraper"
	"gorm.io/gorm"
)

type apiResponse struct {
	Result struct {
		Data struct {
			Items      []json.RawMessage `json:"items"`
			NextCursor string            `json:"nextCursor"`
		} `json:"data"`
	} `json:"result"`
}

func main() {
	db, err := database.Connect()
	if err != nil {
		slog.Error("Failed to connect to database", "error", err)
		os.Exit(1)
	}

	s := scraper.NewScraper(scraper.WithBaseURL("http://gateway:8080/trpc/"), scraper.WithFlushTimeout(nil))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	slog.Info("Scraper started")

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	scrapeAll(ctx, s, db)

	for {
		select {
		case <-ctx.Done():
			slog.Info("Scraper shutting down")
			return
		case <-ticker.C:
			scrapeAll(ctx, s, db)
		}
	}
}

func scrapeAll(ctx context.Context, s *scraper.Scraper, db *gorm.DB) {
	scrapePages(ctx, s, db, "event.getEventsPaginated", map[string]any{"limit": 100}, "events", 0, models.UpsertEventFromJSON)
	scrapePages(ctx, s, db, "workOffer.getWorkOffersPaginated", map[string]any{"limit": 100}, "work_offers", 0, models.UpsertWorkOfferFromJSON)
	scrapePages(ctx, s, db, "transaction.getPaginatedTransactions", map[string]any{"limit": 100}, "transactions", 0, models.UpsertTransactionFromJSON)

	// Articles: get newest
	scrapePages(ctx, s, db, "article.getArticlesPaginated", map[string]any{"type": "last", "limit": 100}, "articles", 0, models.UpsertArticleFromJSON)

	// Articles: top 1000 for daily, weekly, top (sorted by likes, not time)
	for _, t := range []string{"daily", "weekly", "top"} {
		scrapePages(ctx, s, db, "article.getArticlesPaginated", map[string]any{"type": t, "limit": 100}, "articles", 1000, models.UpsertArticleFromJSON)
	}
}

func existsInDB(db *gorm.DB, table string, id string) bool {
	var count int64
	db.Table(table).Where("id = ?", id).Count(&count)
	return count > 0
}

func scrapePages(
	ctx context.Context,
	s *scraper.Scraper,
	db *gorm.DB,
	method string,
	baseInput map[string]any,
	table string,
	maxItems int,
	upsertFn func(*gorm.DB, json.RawMessage) error,
) {
	cursor := ""
	totalUpserted := 0

	for {
		if ctx.Err() != nil {
			return
		}

		input := make(map[string]any, len(baseInput)+1)
		for k, v := range baseInput {
			input[k] = v
		}
		if cursor != "" {
			input["cursor"] = cursor
		}

		inputJSON, err := json.Marshal(input)
		if err != nil {
			slog.Error("Failed to marshal scraper input", "method", method, "error", err)
			return
		}

		raw, err := s.Request(ctx, method, inputJSON)
		if err != nil {
			slog.Error("Scraper API request failed", "method", method, "error", err)
			return
		}

		var resp apiResponse
		err = json.Unmarshal(raw, &resp)
		if err != nil {
			slog.Error("Failed to parse API response", "method", method, "error", err)
			return
		}

		items := resp.Result.Data.Items
		if len(items) == 0 {
			break
		}

		var firstID struct {
			ID string `json:"_id"`
		}
		err = json.Unmarshal(items[0], &firstID)
		if err == nil && firstID.ID != "" {
			if existsInDB(db, table, firstID.ID) {
				break
			}
		}

		for _, item := range items {
			err := upsertFn(db, item)
			if err != nil {
				slog.Error("Failed to upsert item", "method", method, "error", err)
			}
		}

		totalUpserted += len(items)
		cursor = resp.Result.Data.NextCursor
		if cursor == "" {
			break
		}

		if maxItems > 0 && totalUpserted >= maxItems {
			break
		}
	}
}
