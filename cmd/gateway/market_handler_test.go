package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Hattorius/War-Era-Gateway/internal/database/models"
	"github.com/Hattorius/War-Era-Gateway/internal/market"
)

type fakeMarketDailySource struct {
	days                      int
	availableSince            string
	incrementalCoveredThrough *time.Time
	now                       time.Time
	snapshot                  models.MarketDailySnapshot
	err                       error
}

func (source *fakeMarketDailySource) Load(
	_ context.Context,
	days int,
	availableSince string,
	incrementalCoveredThrough *time.Time,
	now time.Time,
) (models.MarketDailySnapshot, error) {
	source.days = days
	source.availableSince = availableSince
	source.incrementalCoveredThrough = incrementalCoveredThrough
	source.now = now
	return source.snapshot, source.err
}

func TestMarketReadKeyMiddlewareUsesDedicatedHeader(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	t.Run("endpoint hidden without configured key", func(t *testing.T) {
		handler := marketReadKeyMiddleware("")(next)
		req := httptest.NewRequest(http.MethodGet, "/api/market/daily", nil)
		req.Header.Set("X-Gateway-Market-Key", "any-key")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
		}
	})

	t.Run("rejects missing wrong and unrelated keys", func(t *testing.T) {
		handler := marketReadKeyMiddleware("market-secret")(next)
		headers := []map[string]string{
			{},
			{"X-Gateway-Market-Key": "wrong"},
			{"X-Gateway-Admin-Key": "market-secret"},
			{"X-API-Key": "market-secret"},
			{"X-Gateway-Market-Key": strings.Repeat("x", maxAPIKeyBytes+1)},
		}
		for _, headers := range headers {
			req := httptest.NewRequest(http.MethodGet, "/api/market/daily", nil)
			for name, value := range headers {
				req.Header.Set(name, value)
			}
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("headers %v status = %d, want %d",
					headers,
					rec.Code,
					http.StatusUnauthorized,
				)
			}
		}
	})

	t.Run("accepts trimmed dedicated key", func(t *testing.T) {
		handler := marketReadKeyMiddleware("market-secret")(next)
		req := httptest.NewRequest(http.MethodGet, "/api/market/daily", nil)
		req.Header.Set("X-Gateway-Market-Key", "  market-secret  ")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
		}
	})
}

func TestMarketDailyHandlerReturnsStrictInternalContract(t *testing.T) {
	stateFile := market.StateFile(t.TempDir())
	coveredThrough := time.Date(
		2026,
		time.July,
		30,
		20,
		59,
		30,
		0,
		time.UTC,
	)
	if err := market.SaveBackfillState(stateFile, market.BackfillState{
		Completed:                 false,
		Cursor:                    "opaque",
		UpdatedAt:                 time.Date(2026, time.July, 30, 20, 0, 0, 0, time.UTC),
		AvailableSince:            "2026-07-29",
		CompletionReason:          "",
		IncrementalCoveredThrough: &coveredThrough,
	}); err != nil {
		t.Fatalf("save state: %v", err)
	}

	latest := time.Date(2026, time.July, 30, 20, 59, 1, 0, time.UTC)
	source := &fakeMarketDailySource{
		snapshot: models.MarketDailySnapshot{
			LatestTransactionAt: &latest,
			Days: []models.MarketDailyPoint{
				{
					Day:               "2026-07-29",
					TradingTrades:     12,
					ItemMarketTrades:  3,
					UnitsTraded:       "42.500",
					RecordedVolumeBTC: "10.25",
				},
				{
					Day:               "2026-07-30",
					TradingTrades:     0,
					ItemMarketTrades:  0,
					UnitsTraded:       "0",
					RecordedVolumeBTC: "0",
				},
			},
		},
	}
	now := time.Date(2026, time.July, 30, 21, 0, 0, 123, time.UTC)
	handler := marketDailyHandler(source, stateFile, func() time.Time { return now })
	req := httptest.NewRequest(http.MethodGet, "/api/market/daily?days=7", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if source.days != 7 ||
		source.availableSince != "2026-07-29" ||
		source.incrementalCoveredThrough == nil ||
		!source.incrementalCoveredThrough.Equal(coveredThrough) ||
		!source.now.Equal(now) {
		t.Fatalf("source call = days %d availableSince %q coveredThrough %v now %s",
			source.days,
			source.availableSince,
			source.incrementalCoveredThrough,
			source.now,
		)
	}

	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(response) != 5 {
		t.Fatalf("top-level fields = %v, want exact five-field contract", response)
	}
	if response["generatedAt"] != "2026-07-30T21:00:00.000000123Z" ||
		response["availableSince"] != "2026-07-29" ||
		response["latestTransactionAt"] != "2026-07-30T20:59:01Z" ||
		response["backfillComplete"] != false {
		t.Fatalf("metadata = %v", response)
	}

	points, ok := response["days"].([]any)
	if !ok || len(points) != 2 {
		t.Fatalf("days = %#v, want two points", response["days"])
	}
	first, ok := points[0].(map[string]any)
	if !ok {
		t.Fatalf("first point = %#v", points[0])
	}
	if len(first) != 5 ||
		first["day"] != "2026-07-29" ||
		first["tradingTrades"] != float64(12) ||
		first["itemMarketTrades"] != float64(3) ||
		first["unitsTraded"] != "42.500" ||
		first["recordedVolumeBtc"] != "10.25" {
		t.Fatalf("first point = %#v, want strict market contract", first)
	}
}

func TestMarketDailyHandlerValidatesDaysAndKeepsMissingStateEmpty(t *testing.T) {
	source := &fakeMarketDailySource{
		snapshot: models.MarketDailySnapshot{Days: []models.MarketDailyPoint{}},
	}
	handler := marketDailyHandler(
		source,
		market.StateFile(t.TempDir()),
		func() time.Time {
			return time.Date(2026, time.July, 30, 21, 0, 0, 0, time.UTC)
		},
	)

	for _, query := range []string{"?days=0", "?days=31", "?days=seven", "?days=1.5"} {
		req := httptest.NewRequest(http.MethodGet, "/api/market/daily"+query, nil)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want %d", query, rec.Code, http.StatusBadRequest)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/market/daily", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("default request status = %d, want %d", rec.Code, http.StatusOK)
	}
	if source.days != 30 || source.availableSince != "" {
		t.Fatalf("default source args = days %d availableSince %q, want 30 and empty",
			source.days,
			source.availableSince,
		)
	}

	var response marketDailyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode empty response: %v", err)
	}
	if response.AvailableSince != nil ||
		response.LatestTransactionAt != nil ||
		response.BackfillComplete ||
		response.Days == nil ||
		len(response.Days) != 0 {
		t.Fatalf("empty response = %+v", response)
	}
}

func TestMarketDailyHandlerReturnsUnavailableOnSourceError(t *testing.T) {
	source := &fakeMarketDailySource{err: errors.New("database unavailable")}
	handler := marketDailyHandler(
		source,
		market.StateFile(t.TempDir()),
		time.Now,
	)
	req := httptest.NewRequest(http.MethodGet, "/api/market/daily?days=30", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}
