package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
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

const (
	defaultScrapeIntervalSeconds    = 5
	defaultScraperRequestsPerMinute = 200
	defaultTransactionRetentionDays = 30
	transactionPageLimit            = 100
)

type scraperDataset string

const (
	datasetArticles     scraperDataset = "articles"
	datasetEvents       scraperDataset = "events"
	datasetTransactions scraperDataset = "transactions"
	datasetWorkOffers   scraperDataset = "work_offers"
)

var allScraperDatasets = []scraperDataset{
	datasetArticles,
	datasetEvents,
	datasetTransactions,
	datasetWorkOffers,
}

type scraperConfig struct {
	datasets                 map[scraperDataset]struct{}
	interval                 time.Duration
	requestsPerMinute        int
	transactionRetentionDays int
}

func (c scraperConfig) datasetEnabled(dataset scraperDataset) bool {
	_, ok := c.datasets[dataset]
	return ok
}

func (c scraperConfig) datasetNames() []string {
	names := make([]string, 0, len(c.datasets))
	for dataset := range c.datasets {
		names = append(names, string(dataset))
	}
	sort.Strings(names)
	return names
}

func getBoundedIntegerEnv(name string, defaultValue, minValue, maxValue int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return defaultValue
	}

	value, err := strconv.Atoi(raw)
	if err != nil || value < minValue || value > maxValue {
		slog.Warn(
			"Invalid bounded integer setting; using default",
			"name", name,
			"value", raw,
			"default", defaultValue,
			"min", minValue,
			"max", maxValue,
		)
		return defaultValue
	}

	return value
}

func getTransactionRetentionDays() int {
	return getBoundedIntegerEnv(
		"TRANSACTION_RETENTION_DAYS",
		defaultTransactionRetentionDays,
		1,
		365,
	)
}

func getScrapeIntervalSeconds() int {
	return getBoundedIntegerEnv(
		"SCRAPER_INTERVAL_SECONDS",
		defaultScrapeIntervalSeconds,
		5,
		3600,
	)
}

func getScraperRequestsPerMinute() int {
	return getBoundedIntegerEnv(
		"SCRAPER_REQUESTS_PER_MINUTE",
		defaultScraperRequestsPerMinute,
		1,
		200,
	)
}

func getScraperDatasets() (map[scraperDataset]struct{}, error) {
	raw := strings.TrimSpace(os.Getenv("SCRAPER_DATASETS"))
	if raw == "" || strings.EqualFold(raw, "all") {
		datasets := make(map[scraperDataset]struct{}, len(allScraperDatasets))
		for _, dataset := range allScraperDatasets {
			datasets[dataset] = struct{}{}
		}
		return datasets, nil
	}

	allowed := make(map[scraperDataset]struct{}, len(allScraperDatasets))
	for _, dataset := range allScraperDatasets {
		allowed[dataset] = struct{}{}
	}

	datasets := make(map[scraperDataset]struct{})
	for _, field := range strings.Split(raw, ",") {
		dataset := scraperDataset(strings.ToLower(strings.TrimSpace(field)))
		if dataset == "" {
			continue
		}
		if _, ok := allowed[dataset]; !ok {
			return nil, fmt.Errorf("unsupported SCRAPER_DATASETS value %q", dataset)
		}
		datasets[dataset] = struct{}{}
	}
	if len(datasets) == 0 {
		return nil, fmt.Errorf("SCRAPER_DATASETS must enable at least one dataset")
	}

	return datasets, nil
}

func loadScraperConfig() (scraperConfig, error) {
	datasets, err := getScraperDatasets()
	if err != nil {
		return scraperConfig{}, err
	}

	return scraperConfig{
		datasets:                 datasets,
		interval:                 time.Duration(getScrapeIntervalSeconds()) * time.Second,
		requestsPerMinute:        getScraperRequestsPerMinute(),
		transactionRetentionDays: getTransactionRetentionDays(),
	}, nil
}

func transactionCutoff(now time.Time, retentionDays int) time.Time {
	return now.UTC().Add(-time.Duration(retentionDays) * 24 * time.Hour)
}

func pruneExpiredTransactions(db *gorm.DB, now time.Time, retentionDays int) error {
	cutoff := transactionCutoff(now, retentionDays)
	deleted, err := models.DeleteTransactionsBefore(db, cutoff)
	if err != nil {
		slog.Error("Failed to prune expired transactions", "error", err)
		return err
	}

	slog.Info(
		"Transaction retention complete",
		"deleted", deleted,
		"retention_days", retentionDays,
	)
	return nil
}

func main() {
	config, err := loadScraperConfig()
	if err != nil {
		slog.Error("Invalid scraper configuration", "error", err)
		os.Exit(1)
	}
	if strings.TrimSpace(os.Getenv("WARERA_API_KEY")) == "" {
		slog.Error("WARERA_API_KEY environment variable is not set")
		os.Exit(1)
	}

	db, err := database.Connect()
	if err != nil {
		slog.Error("Failed to connect to database", "error", err)
		os.Exit(1)
	}

	timeout := 400 * time.Millisecond
	s := scraper.NewScraper(
		scraper.WithFlushTimeout(&timeout),
		scraper.WithRequestRateLimit(config.requestsPerMinute, 1),
	)
	defer s.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	slog.Info(
		"Scraper started",
		"datasets", config.datasetNames(),
		"interval_seconds", int(config.interval/time.Second),
		"requests_per_minute", config.requestsPerMinute,
		"transaction_retention_days", config.transactionRetentionDays,
	)

	if err := pruneExpiredTransactions(db, time.Now(), config.transactionRetentionDays); err != nil {
		os.Exit(1)
	}
	if err := scrapeConfigured(ctx, s, db, config, true); err != nil {
		os.Exit(1)
	}

	ticker := time.NewTicker(config.interval)
	defer ticker.Stop()
	retentionTicker := time.NewTicker(24 * time.Hour)
	defer retentionTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("Scraper shutting down")
			return
		case <-ticker.C:
			_ = scrapeConfigured(ctx, s, db, config, false)
		case <-retentionTicker.C:
			_ = pruneExpiredTransactions(db, time.Now(), config.transactionRetentionDays)
		}
	}
}

type requester interface {
	Request(context.Context, string, json.RawMessage) (json.RawMessage, error)
}

func scrapeConfigured(
	ctx context.Context,
	s requester,
	db *gorm.DB,
	config scraperConfig,
	fullTransactionBackfill bool,
) error {
	var wg sync.WaitGroup
	var transactionErr error

	run := func(method string, input map[string]any, table string, maxItems int, upsertFn func(*gorm.DB, json.RawMessage) error) {
		wg.Go(func() {
			scrapePages(ctx, s, db, method, input, table, maxItems, upsertFn)
		})
	}

	if config.datasetEnabled(datasetEvents) {
		run("event.getEventsPaginated", map[string]any{"limit": 100}, "events", 0, models.UpsertEventFromJSON)
	}
	if config.datasetEnabled(datasetWorkOffers) {
		run("workOffer.getWorkOffersPaginated", map[string]any{"limit": 100}, "work_offers", 0, models.UpsertWorkOfferFromJSON)
	}
	if config.datasetEnabled(datasetTransactions) {
		wg.Go(func() {
			startedAt := time.Now()
			summary, err := scrapeTransactionPages(
				ctx,
				s,
				gormTransactionStore{db: db},
				transactionCutoff(startedAt, config.transactionRetentionDays),
				fullTransactionBackfill,
			)
			level := slog.LevelInfo
			if err != nil {
				level = slog.LevelError
				transactionErr = err
			}
			slog.Log(
				ctx,
				level,
				"Transaction scrape cycle complete",
				"backfill", fullTransactionBackfill,
				"duration_ms", time.Since(startedAt).Milliseconds(),
				"pages", summary.Pages,
				"received", summary.Received,
				"new", summary.New,
				"known", summary.Known,
				"expired", summary.Expired,
				"invalid", summary.Invalid,
				"stop_reason", summary.StopReason,
				"error", err,
			)
		})
	}
	if config.datasetEnabled(datasetArticles) {
		run("article.getArticlesPaginated", map[string]any{"type": "last", "limit": 100}, "articles", 0, models.UpsertArticleFromJSON)
		for _, articleType := range []string{"daily", "weekly", "top"} {
			run(
				"article.getArticlesPaginated",
				map[string]any{"type": articleType, "limit": 100},
				"articles",
				1000,
				models.UpsertArticleFromJSON,
			)
		}
	}

	wg.Wait()
	return transactionErr
}

type transactionScrapeSummary struct {
	Pages      int
	Received   int
	New        int
	Known      int
	Expired    int
	Invalid    int
	StopReason string
}

type transactionPageRecord struct {
	id  string
	raw json.RawMessage
}

type transactionStore interface {
	findExistingIDs([]string) (map[string]struct{}, error)
	persist([]transactionPageRecord) error
}

type gormTransactionStore struct {
	db *gorm.DB
}

func (s gormTransactionStore) findExistingIDs(ids []string) (map[string]struct{}, error) {
	return models.FindExistingTransactionIDs(s.db, ids)
}

func (s gormTransactionStore) persist(records []transactionPageRecord) error {
	if len(records) == 0 {
		return nil
	}

	return s.db.Transaction(func(tx *gorm.DB) error {
		for _, record := range records {
			if err := models.UpsertTransactionFromJSON(tx, record.raw); err != nil {
				return fmt.Errorf("upsert transaction %s: %w", record.id, err)
			}
		}
		return nil
	})
}

func parseTransactionPageRecord(
	raw json.RawMessage,
	cutoff time.Time,
) (transactionPageRecord, bool, bool) {
	var parsed struct {
		ID        string    `json:"_id"`
		CreatedAt time.Time `json:"createdAt"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return transactionPageRecord{}, false, false
	}

	parsed.ID = strings.TrimSpace(parsed.ID)
	if parsed.ID == "" || parsed.CreatedAt.IsZero() {
		return transactionPageRecord{}, false, false
	}
	if parsed.CreatedAt.Before(cutoff) {
		return transactionPageRecord{}, true, true
	}

	return transactionPageRecord{
		id:  parsed.ID,
		raw: raw,
	}, true, false
}

func scrapeTransactionPages(
	ctx context.Context,
	s requester,
	store transactionStore,
	cutoff time.Time,
	fullBackfill bool,
) (transactionScrapeSummary, error) {
	summary := transactionScrapeSummary{}
	cursor := ""
	pendingIncremental := make([]transactionPageRecord, 0)

	for {
		if err := ctx.Err(); err != nil {
			summary.StopReason = "context_cancelled"
			return summary, err
		}

		input := map[string]any{"limit": transactionPageLimit}
		if cursor != "" {
			input["cursor"] = cursor
		}
		inputJSON, err := json.Marshal(input)
		if err != nil {
			summary.StopReason = "input_error"
			return summary, err
		}

		raw, err := s.Request(ctx, "transaction.getPaginatedTransactions", inputJSON)
		if err != nil {
			summary.StopReason = "request_error"
			return summary, err
		}

		var resp apiResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			summary.StopReason = "response_error"
			return summary, err
		}

		summary.Pages++
		items := resp.Result.Data.Items
		summary.Received += len(items)
		if len(items) == 0 {
			summary.StopReason = "empty_page"
			break
		}

		pageRecords := make([]transactionPageRecord, 0, len(items))
		cutoffReached := false
		for _, item := range items {
			record, valid, expired := parseTransactionPageRecord(item, cutoff)
			if !valid {
				summary.Invalid++
				continue
			}
			if expired {
				summary.Expired++
				cutoffReached = true
				continue
			}
			pageRecords = append(pageRecords, record)
		}

		ids := make([]string, len(pageRecords))
		for i, record := range pageRecords {
			ids[i] = record.id
		}
		existing, err := store.findExistingIDs(ids)
		if err != nil {
			summary.StopReason = "database_read_error"
			return summary, err
		}

		newRecords := make([]transactionPageRecord, 0, len(pageRecords))
		knownOverlap := false
		for _, record := range pageRecords {
			if _, ok := existing[record.id]; ok {
				summary.Known++
				knownOverlap = true
				continue
			}
			newRecords = append(newRecords, record)
		}

		if fullBackfill {
			if err := store.persist(newRecords); err != nil {
				summary.StopReason = "database_write_error"
				return summary, err
			}
			summary.New += len(newRecords)
		} else {
			pendingIncremental = append(pendingIncremental, newRecords...)
		}

		switch {
		case cutoffReached:
			summary.StopReason = "retention_cutoff"
		case !fullBackfill && knownOverlap:
			summary.StopReason = "known_overlap"
		case resp.Result.Data.NextCursor == "":
			summary.StopReason = "end_of_feed"
		default:
			cursor = resp.Result.Data.NextCursor
			continue
		}
		break
	}

	if !fullBackfill {
		if err := store.persist(pendingIncremental); err != nil {
			summary.StopReason = "database_write_error"
			return summary, err
		}
		summary.New += len(pendingIncremental)
	}

	return summary, nil
}

func existsInDB(db *gorm.DB, table string, id string) bool {
	var count int64
	db.Table(table).Where("id = ?", id).Count(&count)
	return count > 0
}

func scrapePages(
	ctx context.Context,
	s requester,
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
