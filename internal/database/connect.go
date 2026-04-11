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

	if err := db.AutoMigrate(&models.Event{}, &models.EventCountry{}); err != nil {
		return nil, fmt.Errorf("failed to migrate database: %w", err)
	}

	models.AddEventIndexes(db)

	return db, nil
}
