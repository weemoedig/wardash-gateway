package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

type fakeRequester struct {
	calls     int
	responses []json.RawMessage
}

func (f *fakeRequester) Request(
	_ context.Context,
	_ string,
	_ json.RawMessage,
) (json.RawMessage, error) {
	if f.calls >= len(f.responses) {
		return nil, errors.New("unexpected request")
	}
	response := f.responses[f.calls]
	f.calls++
	return response, nil
}

type fakeTransactionStore struct {
	existing  map[string]struct{}
	persisted []string
}

func (f *fakeTransactionStore) findExistingIDs(ids []string) (map[string]struct{}, error) {
	found := make(map[string]struct{})
	for _, id := range ids {
		if _, ok := f.existing[id]; ok {
			found[id] = struct{}{}
		}
	}
	return found, nil
}

func (f *fakeTransactionStore) persist(records []transactionPageRecord) (int, error) {
	inserted := 0
	for _, record := range records {
		if _, exists := f.existing[record.id]; exists {
			continue
		}
		f.persisted = append(f.persisted, record.id)
		f.existing[record.id] = struct{}{}
		inserted++
	}
	return inserted, nil
}

func transactionItem(id string, createdAt time.Time) json.RawMessage {
	raw, err := json.Marshal(map[string]any{
		"_id":       id,
		"createdAt": createdAt.UTC().Format(time.RFC3339),
	})
	if err != nil {
		panic(err)
	}
	return raw
}

func transactionResponse(items []json.RawMessage, nextCursor string) json.RawMessage {
	raw, err := json.Marshal(map[string]any{
		"result": map[string]any{
			"data": map[string]any{
				"items":      items,
				"nextCursor": nextCursor,
			},
		},
	})
	if err != nil {
		panic(err)
	}
	return raw
}

func TestLoadScraperConfig(t *testing.T) {
	t.Run("uses backward compatible defaults", func(t *testing.T) {
		t.Setenv("SCRAPER_DATASETS", "")
		t.Setenv("SCRAPER_INTERVAL_SECONDS", "")
		t.Setenv("SCRAPER_REQUESTS_PER_MINUTE", "")
		t.Setenv("TRANSACTION_RETENTION_DAYS", "")

		config, err := loadScraperConfig()
		if err != nil {
			t.Fatalf("loadScraperConfig returned error: %v", err)
		}
		if len(config.datasets) != len(allScraperDatasets) {
			t.Fatalf("datasets = %v, want all datasets", config.datasetNames())
		}
		if config.interval != 5*time.Second {
			t.Fatalf("interval = %s, want 5s", config.interval)
		}
		if config.requestsPerMinute != 200 {
			t.Fatalf("requestsPerMinute = %d, want 200", config.requestsPerMinute)
		}
		if config.transactionRetentionDays != 30 {
			t.Fatalf("transactionRetentionDays = %d, want 30", config.transactionRetentionDays)
		}
	})

	t.Run("accepts WarDash production settings", func(t *testing.T) {
		t.Setenv("SCRAPER_DATASETS", "transactions")
		t.Setenv("SCRAPER_INTERVAL_SECONDS", "60")
		t.Setenv("SCRAPER_REQUESTS_PER_MINUTE", "30")
		t.Setenv("TRANSACTION_RETENTION_DAYS", "30")

		config, err := loadScraperConfig()
		if err != nil {
			t.Fatalf("loadScraperConfig returned error: %v", err)
		}
		if len(config.datasets) != 1 || !config.datasetEnabled(datasetTransactions) {
			t.Fatalf("datasets = %v, want transactions only", config.datasetNames())
		}
		if config.interval != time.Minute {
			t.Fatalf("interval = %s, want 1m", config.interval)
		}
		if config.requestsPerMinute != 30 {
			t.Fatalf("requestsPerMinute = %d, want 30", config.requestsPerMinute)
		}
	})

	t.Run("rejects an unknown dataset", func(t *testing.T) {
		t.Setenv("SCRAPER_DATASETS", "transactions,players")

		if _, err := loadScraperConfig(); err == nil {
			t.Fatal("expected unknown dataset error")
		}
	})

	t.Run("falls back for out of range integers", func(t *testing.T) {
		t.Setenv("SCRAPER_DATASETS", "transactions")
		t.Setenv("SCRAPER_INTERVAL_SECONDS", "1")
		t.Setenv("SCRAPER_REQUESTS_PER_MINUTE", "500")
		t.Setenv("TRANSACTION_RETENTION_DAYS", "0")

		config, err := loadScraperConfig()
		if err != nil {
			t.Fatalf("loadScraperConfig returned error: %v", err)
		}
		if config.interval != 5*time.Second {
			t.Fatalf("interval = %s, want default 5s", config.interval)
		}
		if config.requestsPerMinute != 200 {
			t.Fatalf("requestsPerMinute = %d, want default 200", config.requestsPerMinute)
		}
		if config.transactionRetentionDays != 30 {
			t.Fatalf("transactionRetentionDays = %d, want default 30", config.transactionRetentionDays)
		}
	})
}

func TestTransactionCutoffUsesRollingUTCDays(t *testing.T) {
	now := time.Date(2026, time.July, 30, 22, 15, 0, 0, time.FixedZone("CEST", 2*60*60))
	got := transactionCutoff(now, 30)
	want := time.Date(2026, time.June, 30, 20, 15, 0, 0, time.UTC)

	if !got.Equal(want) {
		t.Fatalf("cutoff = %s, want %s", got, want)
	}
}

func TestMarketRollupRetentionNeverExceedsPublicContract(t *testing.T) {
	if got := marketRollupRetentionDays(365); got != 30 {
		t.Fatalf("365-day transaction retention produced %d market days, want 30", got)
	}
	if got := marketRollupRetentionDays(7); got != 7 {
		t.Fatalf("7-day transaction retention produced %d market days, want 7", got)
	}
}

func TestIncrementalTransactionScrapeHealsUnknownItemsOnOverlapPage(t *testing.T) {
	now := time.Date(2026, time.July, 30, 20, 0, 0, 0, time.UTC)
	requester := &fakeRequester{responses: []json.RawMessage{
		transactionResponse([]json.RawMessage{
			transactionItem("new-a", now),
			transactionItem("known-b", now.Add(-time.Minute)),
			transactionItem("new-c", now.Add(-2*time.Minute)),
			json.RawMessage(`{"_id":"","createdAt":"invalid"}`),
		}, "unused-cursor"),
	}}
	store := &fakeTransactionStore{
		existing: map[string]struct{}{"known-b": {}},
	}

	summary, err := scrapeTransactionPages(
		context.Background(),
		requester,
		store,
		now.Add(-30*24*time.Hour),
		false,
		"",
		0,
	)
	if err != nil {
		t.Fatalf("scrapeTransactionPages returned error: %v", err)
	}
	if requester.calls != 1 {
		t.Fatalf("calls = %d, want 1", requester.calls)
	}
	if summary.StopReason != "known_overlap" {
		t.Fatalf("stop reason = %q, want known_overlap", summary.StopReason)
	}
	if summary.New != 2 || summary.Known != 1 || summary.Invalid != 1 {
		t.Fatalf("summary = %+v, want new=2 known=1 invalid=1", summary)
	}
	if len(store.persisted) != 2 ||
		store.persisted[0] != "new-a" ||
		store.persisted[1] != "new-c" {
		t.Fatalf("persisted = %v, want [new-a new-c]", store.persisted)
	}
}

func TestFullTransactionBackfillIgnoresKnownOverlapUntilCutoff(t *testing.T) {
	now := time.Date(2026, time.July, 30, 20, 0, 0, 0, time.UTC)
	cutoff := now.Add(-30 * 24 * time.Hour)
	requester := &fakeRequester{responses: []json.RawMessage{
		transactionResponse([]json.RawMessage{
			transactionItem("known-a", now),
			transactionItem("new-b", now.Add(-time.Minute)),
		}, "page-two"),
		transactionResponse([]json.RawMessage{
			transactionItem("expired-c", cutoff.Add(-time.Second)),
		}, "unused-cursor"),
	}}
	store := &fakeTransactionStore{
		existing: map[string]struct{}{"known-a": {}},
	}

	summary, err := scrapeTransactionPages(
		context.Background(),
		requester,
		store,
		cutoff,
		true,
		"",
		0,
	)
	if err != nil {
		t.Fatalf("scrapeTransactionPages returned error: %v", err)
	}
	if requester.calls != 2 {
		t.Fatalf("calls = %d, want 2", requester.calls)
	}
	if summary.StopReason != "retention_cutoff" {
		t.Fatalf("stop reason = %q, want retention_cutoff", summary.StopReason)
	}
	if summary.New != 1 || summary.Known != 1 || summary.Expired != 1 {
		t.Fatalf("summary = %+v, want new=1 known=1 expired=1", summary)
	}
	if len(store.persisted) != 1 || store.persisted[0] != "new-b" {
		t.Fatalf("persisted = %v, want [new-b]", store.persisted)
	}
}

func TestTransactionBackfillReturnsResumeCursorAtPageBudget(t *testing.T) {
	now := time.Date(2026, time.July, 30, 20, 0, 0, 0, time.UTC)
	requester := &fakeRequester{responses: []json.RawMessage{
		transactionResponse([]json.RawMessage{
			transactionItem("new-a", now),
		}, "page-two"),
	}}
	store := &fakeTransactionStore{existing: map[string]struct{}{}}

	summary, err := scrapeTransactionPages(
		context.Background(),
		requester,
		store,
		now.Add(-30*24*time.Hour),
		true,
		"",
		1,
	)
	if err != nil {
		t.Fatalf("scrapeTransactionPages returned error: %v", err)
	}
	if summary.StopReason != "page_budget" {
		t.Fatalf("stop reason = %q, want page_budget", summary.StopReason)
	}
	if summary.NextCursor != "page-two" {
		t.Fatalf("next cursor = %q, want page-two", summary.NextCursor)
	}
	if summary.BackfillComplete {
		t.Fatal("backfill complete = true, want false")
	}
}

func TestTransactionBackfillFrontierUsesCursorBoundaryNotLooseExpiredMinimum(
	t *testing.T,
) {
	cutoff := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
	boundary := time.Date(2026, time.July, 30, 7, 0, 0, 0, time.UTC)
	requester := &fakeRequester{responses: []json.RawMessage{
		transactionResponse([]json.RawMessage{
			transactionItem(
				"new-a",
				time.Date(2026, time.July, 30, 10, 0, 0, 0, time.UTC),
			),
			transactionItem(
				"new-b",
				time.Date(2026, time.July, 30, 9, 0, 0, 0, time.UTC),
			),
			transactionItem(
				"loose-expired",
				time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC),
			),
			transactionItem(
				"new-c",
				time.Date(2026, time.July, 30, 8, 0, 0, 0, time.UTC),
			),
			transactionItem("new-d", boundary),
			transactionItem(
				"",
				time.Date(2026, time.May, 1, 0, 0, 0, 0, time.UTC),
			),
		}, "page-two"),
	}}
	store := &fakeTransactionStore{existing: map[string]struct{}{}}

	summary, err := scrapeTransactionPages(
		context.Background(),
		requester,
		store,
		cutoff,
		true,
		"",
		1,
	)
	if err != nil {
		t.Fatalf("scrapeTransactionPages returned error: %v", err)
	}
	if summary.StopReason != "page_budget" ||
		summary.BackfillComplete ||
		summary.Expired != 1 ||
		summary.Invalid != 1 ||
		summary.OldestProcessedAt == nil ||
		!summary.OldestProcessedAt.Equal(boundary) {
		t.Fatalf(
			"summary = %+v, want cursor boundary %s without false cutoff completion",
			summary,
			boundary,
		)
	}
	if len(store.persisted) != 4 {
		t.Fatalf("persisted = %v, want four retained valid transactions", store.persisted)
	}
}

func TestTransactionScrapeFailsClosedWithoutValidCursorBoundary(t *testing.T) {
	requester := &fakeRequester{responses: []json.RawMessage{
		transactionResponse([]json.RawMessage{
			transactionItem(
				"",
				time.Date(2026, time.July, 30, 8, 0, 0, 0, time.UTC),
			),
			json.RawMessage(`{"_id":"missing-created-at"}`),
		}, "page-two"),
	}}
	store := &fakeTransactionStore{existing: map[string]struct{}{}}

	summary, err := scrapeTransactionPages(
		context.Background(),
		requester,
		store,
		time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
		true,
		"",
		1,
	)
	if err == nil {
		t.Fatal("scrapeTransactionPages returned nil, want invalid boundary error")
	}
	if summary.StopReason != "invalid_cursor_boundary" ||
		summary.BackfillComplete ||
		summary.OldestProcessedAt != nil {
		t.Fatalf("summary = %+v, want fail-closed invalid boundary", summary)
	}
}

func TestTransactionScrapeRejectsEmptyPageWithContinuationCursor(t *testing.T) {
	requester := &fakeRequester{responses: []json.RawMessage{
		transactionResponse([]json.RawMessage{}, "unexpected-next-page"),
	}}
	store := &fakeTransactionStore{existing: map[string]struct{}{}}

	summary, err := scrapeTransactionPages(
		context.Background(),
		requester,
		store,
		time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
		true,
		"",
		1,
	)
	if err == nil {
		t.Fatal("scrapeTransactionPages returned nil, want cursor-boundary error")
	}
	if summary.StopReason != "invalid_cursor_boundary" ||
		summary.BackfillComplete {
		t.Fatalf("summary = %+v, want fail-closed empty cursor page", summary)
	}
}

func TestTransactionBackfillTreatsEmptyPageAsComplete(t *testing.T) {
	requester := &fakeRequester{responses: []json.RawMessage{
		transactionResponse([]json.RawMessage{}, ""),
	}}
	store := &fakeTransactionStore{existing: map[string]struct{}{}}

	summary, err := scrapeTransactionPages(
		context.Background(),
		requester,
		store,
		time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
		true,
		"",
		0,
	)
	if err != nil {
		t.Fatalf("scrapeTransactionPages returned error: %v", err)
	}
	if summary.StopReason != "empty_page" || !summary.BackfillComplete {
		t.Fatalf("summary = %+v, want completed empty-page backfill", summary)
	}
}

func TestTransactionBackfillStateRoundTrip(t *testing.T) {
	path := t.TempDir() + "/nested/" + transactionStateFilename
	oldestProcessedAt := time.Date(
		2026,
		time.July,
		1,
		12,
		0,
		0,
		0,
		time.UTC,
	)
	coveredThrough := time.Date(
		2026,
		time.July,
		30,
		19,
		59,
		0,
		0,
		time.UTC,
	)
	want := transactionBackfillState{
		Cursor:                    "opaque-cursor",
		UpdatedAt:                 time.Date(2026, time.July, 30, 20, 0, 0, 0, time.UTC),
		AvailableSince:            "2026-07-02",
		CompletionReason:          "",
		BackfillOldestProcessedAt: &oldestProcessedAt,
		IncrementalCoveredThrough: &coveredThrough,
	}

	if err := saveTransactionBackfillState(path, want); err != nil {
		t.Fatalf("saveTransactionBackfillState returned error: %v", err)
	}
	got, err := loadTransactionBackfillState(path)
	if err != nil {
		t.Fatalf("loadTransactionBackfillState returned error: %v", err)
	}
	if got.Cursor != want.Cursor ||
		!got.UpdatedAt.Equal(want.UpdatedAt) ||
		got.AvailableSince != want.AvailableSince ||
		got.CompletionReason != want.CompletionReason ||
		got.BackfillOldestProcessedAt == nil ||
		!got.BackfillOldestProcessedAt.Equal(oldestProcessedAt) ||
		got.IncrementalCoveredThrough == nil ||
		!got.IncrementalCoveredThrough.Equal(coveredThrough) ||
		got.Completed {
		t.Fatalf("state = %+v, want %+v", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat transaction state: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("state mode = %o, want 600", got)
	}
}

func TestMarketAvailabilityUsesExplicitBackfillFrontier(t *testing.T) {
	now := time.Date(2026, time.March, 30, 12, 0, 0, 0, time.UTC)
	frontier := time.Date(2026, time.March, 28, 23, 30, 0, 0, time.UTC)
	state := transactionBackfillState{
		BackfillOldestProcessedAt: &frontier,
	}

	if err := refreshMarketAvailabilityFromFrontier(
		&state,
		now,
		30,
	); err != nil {
		t.Fatalf("refreshMarketAvailabilityFromFrontier returned error: %v", err)
	}
	if state.AvailableSince != "2026-03-30" {
		t.Fatalf("availableSince = %s, want 2026-03-30", state.AvailableSince)
	}
}

func TestMarketAvailabilityIncludesConfirmedFeedStartDay(t *testing.T) {
	frontier := time.Date(2026, time.October, 24, 22, 30, 0, 0, time.UTC)
	state := transactionBackfillState{
		Completed:                 true,
		CompletionReason:          "end_of_feed",
		BackfillOldestProcessedAt: &frontier,
	}

	if err := refreshMarketAvailabilityFromFrontier(
		&state,
		time.Date(2026, time.October, 25, 12, 0, 0, 0, time.UTC),
		30,
	); err != nil {
		t.Fatalf("refreshMarketAvailabilityFromFrontier returned error: %v", err)
	}
	if state.AvailableSince != "2026-10-25" {
		t.Fatalf("availableSince = %s, want 2026-10-25", state.AvailableSince)
	}
}

func TestMarketAvailabilityIsClampedToRetainedCalendarWindow(t *testing.T) {
	frontier := time.Date(2026, time.May, 1, 12, 0, 0, 0, time.UTC)
	state := transactionBackfillState{
		Completed:                 true,
		CompletionReason:          "end_of_feed",
		BackfillOldestProcessedAt: &frontier,
	}
	if err := refreshMarketAvailabilityFromFrontier(
		&state,
		time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC),
		30,
	); err != nil {
		t.Fatalf("refreshMarketAvailabilityFromFrontier returned error: %v", err)
	}
	if state.AvailableSince != "2026-07-01" {
		t.Fatalf("availableSince = %s, want 2026-07-01", state.AvailableSince)
	}
}

func TestMarketAvailabilityDoesNotPublishFutureCoverage(t *testing.T) {
	frontier := time.Date(2026, time.July, 30, 10, 0, 0, 0, time.UTC)
	state := transactionBackfillState{
		BackfillOldestProcessedAt: &frontier,
	}

	if err := refreshMarketAvailabilityFromFrontier(
		&state,
		time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC),
		30,
	); err != nil {
		t.Fatalf("refreshMarketAvailabilityFromFrontier returned error: %v", err)
	}
	if state.AvailableSince != "" {
		t.Fatalf("availableSince = %s, want empty until a full day is reliable", state.AvailableSince)
	}
}

func TestLegacyCoverageMigrationIsConservative(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)

	t.Run("incomplete cursor does not trust old database minimum", func(t *testing.T) {
		state := transactionBackfillState{
			Cursor:         "legacy-cursor",
			UpdatedAt:      now.Add(-time.Hour),
			AvailableSince: "2026-01-01",
		}

		if err := prepareMarketCoverageState(&state, now, 30); err != nil {
			t.Fatalf("prepareMarketCoverageState returned error: %v", err)
		}
		if state.AvailableSince != "" ||
			state.BackfillOldestProcessedAt != nil {
			t.Fatalf("legacy incomplete state = %+v, want no claimed lower coverage", state)
		}
	})

	t.Run("completed state gets explicit conservative frontier", func(t *testing.T) {
		state := transactionBackfillState{
			Completed:        true,
			CompletionReason: "end_of_feed",
			UpdatedAt:        now,
		}

		if err := prepareMarketCoverageState(&state, now, 30); err != nil {
			t.Fatalf("prepareMarketCoverageState returned error: %v", err)
		}
		if state.BackfillOldestProcessedAt == nil ||
			state.CompletionReason != "legacy_complete" ||
			state.AvailableSince != "2026-07-01" {
			t.Fatalf("migrated legacy state = %+v, want conservative July coverage", state)
		}
	})
}

func TestIncrementalCoverageUpdatesEvenAfterBackfillCompletion(t *testing.T) {
	previous := time.Date(2026, time.July, 30, 10, 0, 0, 0, time.UTC)
	coveredThrough := previous.Add(5 * time.Minute)
	state := transactionBackfillState{
		Completed:                 true,
		IncrementalCoveredThrough: &previous,
	}

	if err := recordIncrementalCoverage(
		&state,
		coveredThrough,
		coveredThrough.Add(time.Second),
		30,
	); err != nil {
		t.Fatalf("recordIncrementalCoverage returned error: %v", err)
	}
	if state.IncrementalCoveredThrough == nil ||
		!state.IncrementalCoveredThrough.Equal(coveredThrough) {
		t.Fatalf("coveredThrough = %v, want %s",
			state.IncrementalCoveredThrough,
			coveredThrough,
		)
	}
}

func TestMarketReliableCoverageAdvanced(t *testing.T) {
	now := time.Date(2026, time.July, 30, 22, 30, 0, 0, time.UTC)
	current := now.Add(-time.Minute)
	previousSameDay := current.Add(-5 * time.Minute)
	previousBeforeMidnight := time.Date(
		2026,
		time.July,
		30,
		21,
		50,
		0,
		0,
		time.UTC,
	)

	tests := []struct {
		name     string
		previous *time.Time
		want     bool
	}{
		{
			name: "first proven upper coverage",
			want: true,
		},
		{
			name:     "same Brussels day",
			previous: &previousSameDay,
			want:     false,
		},
		{
			name:     "crossed Brussels midnight",
			previous: &previousBeforeMidnight,
			want:     true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := marketReliableCoverageAdvanced(
				test.previous,
				&current,
				now,
			)
			if err != nil {
				t.Fatalf("marketReliableCoverageAdvanced returned error: %v", err)
			}
			if got != test.want {
				t.Fatalf("coverage advanced = %t, want %t", got, test.want)
			}
		})
	}
}

func TestFailedFirstIncrementalDoesNotClaimCoverage(t *testing.T) {
	tests := []struct {
		name      string
		responses []json.RawMessage
	}{
		{
			name: "transport failure",
		},
		{
			name:      "missing response envelope",
			responses: []json.RawMessage{json.RawMessage(`{}`)},
		},
		{
			name: "application error envelope",
			responses: []json.RawMessage{
				json.RawMessage(`{"error":{"message":"upstream failed"}}`),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stateFile := t.TempDir() + "/" + transactionStateFilename
			state := transactionBackfillState{}
			config := scraperConfig{
				datasets: map[scraperDataset]struct{}{
					datasetTransactions: {},
				},
				stateFile:                stateFile,
				transactionRetentionDays: 30,
			}

			err := scrapeConfigured(
				context.Background(),
				&fakeRequester{responses: test.responses},
				nil,
				config,
				&state,
			)
			if err == nil {
				t.Fatal("scrapeConfigured returned nil, want first incremental failure")
			}
			if state.IncrementalCoveredThrough != nil {
				t.Fatalf("coveredThrough = %v, want nil after failed incremental",
					state.IncrementalCoveredThrough,
				)
			}
			if _, statErr := os.Stat(stateFile); !os.IsNotExist(statErr) {
				t.Fatalf("state file stat error = %v, want not-exist", statErr)
			}
		})
	}
}
