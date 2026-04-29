package database

import (
	"fmt"
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

	for _, table := range []string{"transactions", "events", "work_offers", "articles"} {
		db.Exec(fmt.Sprintf(
			"UPDATE %s SET created_at = date_trunc('second', created_at) WHERE created_at != date_trunc('second', created_at)",
			table,
		))
	}

	return db, nil
}
