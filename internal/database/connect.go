package database

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/Hattorius/War-Era-Gateway/internal/database/models"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func Connect() (*gorm.DB, error) {
	databaseUrl := os.Getenv("DATABASE_URL")
	if databaseUrl == "" {
		return nil, fmt.Errorf("DATABASE_URL environment variable is not set")
	}

	db, err := gorm.Open(postgres.Open(databaseUrl), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("failed to connect database: %w", err)
	}

	err = db.AutoMigrate(
		&models.Event{}, &models.EventCountry{},
		&models.WorkOffer{},
		&models.Article{},
		&models.Transaction{}, &models.TransactionParticipant{},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to migrate database: %w", err)
	}

	models.AddEventIndexes(db)
	models.AddWorkOfferIndexes(db)
	models.AddArticleIndexes(db)
	models.AddTransactionIndexes(db)

	go func() {
		for _, table := range []string{"transactions", "events", "work_offers", "articles"} {
			var needsMigration bool
			db.Raw(fmt.Sprintf(
				"SELECT EXISTS(SELECT 1 FROM %s WHERE created_at != date_trunc('second', created_at) LIMIT 1)",
				table,
			)).Scan(&needsMigration)
			if !needsMigration {
				continue
			}

			slog.Info("Truncating timestamps", "table", table)
			total := int64(0)
			for {
				result := db.Exec(fmt.Sprintf(
					"UPDATE %s SET created_at = date_trunc('second', created_at) WHERE id IN (SELECT id FROM %s WHERE created_at != date_trunc('second', created_at) LIMIT 5000)",
					table, table,
				))
				if result.Error != nil {
					slog.Error("Failed to truncate timestamps", "table", table, "error", result.Error)
					break
				}
				if result.RowsAffected == 0 {
					break
				}
				total += result.RowsAffected
			}
			if total > 0 {
				slog.Info("Truncated timestamps done", "table", table, "rows", total)
			}
		}
	}()

	return db, nil
}
