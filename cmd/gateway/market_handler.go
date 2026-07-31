package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Hattorius/War-Era-Gateway/internal/database/models"
	"github.com/Hattorius/War-Era-Gateway/internal/market"
	"gorm.io/gorm"
)

const (
	defaultMarketDailyDays = 30
	maxMarketDailyDays     = 30
)

type marketDailySource interface {
	Load(
		context.Context,
		int,
		string,
		*time.Time,
		time.Time,
	) (models.MarketDailySnapshot, error)
}

type gormMarketDailySource struct {
	db *gorm.DB
}

func (source gormMarketDailySource) Load(
	ctx context.Context,
	days int,
	availableSince string,
	incrementalCoveredThrough *time.Time,
	now time.Time,
) (models.MarketDailySnapshot, error) {
	return models.LoadMarketDailySnapshot(
		ctx,
		source.db,
		days,
		availableSince,
		incrementalCoveredThrough,
		now,
	)
}

type marketDailyPointResponse struct {
	Day               string `json:"day"`
	TradingTrades     int64  `json:"tradingTrades"`
	ItemMarketTrades  int64  `json:"itemMarketTrades"`
	UnitsTraded       string `json:"unitsTraded"`
	RecordedVolumeBTC string `json:"recordedVolumeBtc"`
}

type marketDailyResponse struct {
	GeneratedAt         string                     `json:"generatedAt"`
	AvailableSince      *string                    `json:"availableSince"`
	LatestTransactionAt *string                    `json:"latestTransactionAt"`
	BackfillComplete    bool                       `json:"backfillComplete"`
	Days                []marketDailyPointResponse `json:"days"`
}

func parseMarketDailyDays(raw string) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultMarketDailyDays, true
	}
	days, err := strconv.Atoi(raw)
	if err != nil || days < 1 || days > maxMarketDailyDays {
		return 0, false
	}
	return days, true
}

func marketDailyHandler(
	source marketDailySource,
	stateFile string,
	now func() time.Time,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		days, valid := parseMarketDailyDays(r.URL.Query().Get("days"))
		if !valid {
			http.Error(w, "days must be an integer between 1 and 30", http.StatusBadRequest)
			return
		}

		state, err := market.LoadBackfillState(stateFile)
		if err != nil {
			slog.Error("Failed to load transaction backfill state for market endpoint", "error", err)
			http.Error(w, "market data is temporarily unavailable", http.StatusServiceUnavailable)
			return
		}

		generatedAt := now().UTC()
		snapshot, err := source.Load(
			r.Context(),
			days,
			state.AvailableSince,
			state.IncrementalCoveredThrough,
			generatedAt,
		)
		if err != nil {
			slog.Error("Failed to load market daily snapshot", "error", err)
			http.Error(w, "market data is temporarily unavailable", http.StatusServiceUnavailable)
			return
		}

		response := marketDailyResponse{
			GeneratedAt:      generatedAt.Format(time.RFC3339Nano),
			BackfillComplete: state.Completed,
			Days:             make([]marketDailyPointResponse, len(snapshot.Days)),
		}
		if state.AvailableSince != "" {
			availableSince := state.AvailableSince
			response.AvailableSince = &availableSince
		}
		if snapshot.LatestTransactionAt != nil {
			latest := snapshot.LatestTransactionAt.UTC().Format(time.RFC3339Nano)
			response.LatestTransactionAt = &latest
		}
		for index, point := range snapshot.Days {
			response.Days[index] = marketDailyPointResponse{
				Day:               point.Day,
				TradingTrades:     point.TradingTrades,
				ItemMarketTrades:  point.ItemMarketTrades,
				UnitsTraded:       point.UnitsTraded,
				RecordedVolumeBTC: point.RecordedVolumeBTC,
			}
		}

		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			slog.Error("Failed to encode market daily response", "error", err)
		}
	}
}
