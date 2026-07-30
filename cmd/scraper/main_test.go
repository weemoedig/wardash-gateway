package main

import (
	"context"
	"encoding/json"
	"errors"
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

func (f *fakeTransactionStore) persist(records []transactionPageRecord) error {
	for _, record := range records {
		f.persisted = append(f.persisted, record.id)
		f.existing[record.id] = struct{}{}
	}
	return nil
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
