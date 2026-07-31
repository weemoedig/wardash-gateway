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
	"github.com/Hattorius/War-Era-Gateway/internal/market"
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
	defaultScrapeIntervalSeconds     = 5
	defaultScraperRequestsPerMinute  = 200
	defaultTransactionRetentionDays  = 30
	maxMarketRollupRetentionDays     = 30
	transactionBackfillPagesPerCycle = 24
	transactionPageLimit             = 100
	transactionStateFilename         = market.TransactionStateFilename
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
	stateFile                string
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
		stateFile:                getTransactionStateFile(),
		transactionRetentionDays: getTransactionRetentionDays(),
	}, nil
}

func getTransactionStateFile() string {
	return market.StateFile(os.Getenv("DATA_DIR"))
}

func transactionCutoff(now time.Time, retentionDays int) time.Time {
	return now.UTC().Add(-time.Duration(retentionDays) * 24 * time.Hour)
}

func marketRollupRetentionDays(transactionRetentionDays int) int {
	if transactionRetentionDays < maxMarketRollupRetentionDays {
		return transactionRetentionDays
	}
	return maxMarketRollupRetentionDays
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

func marketRetentionStart(
	now time.Time,
	retentionDays int,
) (time.Time, error) {
	if retentionDays <= 0 {
		return time.Time{}, fmt.Errorf("transaction retention days must be positive")
	}

	today, err := market.ParseBrusselsDay(market.BrusselsDay(now))
	if err != nil {
		return time.Time{}, err
	}
	return today.AddDate(0, 0, -(retentionDays - 1)), nil
}

func refreshMarketAvailabilityFromFrontier(
	state *transactionBackfillState,
	now time.Time,
	retentionDays int,
) error {
	if state.BackfillOldestProcessedAt == nil ||
		state.BackfillOldestProcessedAt.IsZero() {
		state.AvailableSince = ""
		return nil
	}

	frontierDay := market.BrusselsDay(*state.BackfillOldestProcessedAt)
	availableSince := frontierDay
	if !state.Completed ||
		(state.CompletionReason != "end_of_feed" &&
			state.CompletionReason != "empty_page") {
		var err error
		availableSince, err = market.NextBrusselsDay(frontierDay)
		if err != nil {
			return err
		}
	}

	availableDay, err := market.ParseBrusselsDay(availableSince)
	if err != nil {
		return err
	}
	retentionStart, err := marketRetentionStart(now, retentionDays)
	if err != nil {
		return err
	}
	if availableDay.Before(retentionStart) {
		availableDay = retentionStart
	}
	if availableDay.After(retentionStart.AddDate(0, 0, retentionDays-1)) {
		state.AvailableSince = ""
		return nil
	}
	state.AvailableSince = availableDay.Format(time.DateOnly)
	return nil
}

func prepareMarketCoverageState(
	state *transactionBackfillState,
	now time.Time,
	retentionDays int,
) error {
	if state.BackfillOldestProcessedAt == nil &&
		state.Completed &&
		!state.UpdatedAt.IsZero() {
		legacyFrontier := state.UpdatedAt.UTC().
			Add(-time.Duration(retentionDays) * 24 * time.Hour)
		state.BackfillOldestProcessedAt = &legacyFrontier
		state.CompletionReason = "legacy_complete"
	}
	return refreshMarketAvailabilityFromFrontier(state, now, retentionDays)
}

func mergeBackfillFrontier(
	state *transactionBackfillState,
	summary transactionScrapeSummary,
	cutoff time.Time,
	now time.Time,
	retentionDays int,
) error {
	if summary.OldestProcessedAt != nil {
		oldest := summary.OldestProcessedAt.UTC()
		if state.BackfillOldestProcessedAt != nil &&
			oldest.After(*state.BackfillOldestProcessedAt) {
			return fmt.Errorf(
				"market backfill frontier moved forward from %s to %s",
				state.BackfillOldestProcessedAt.UTC().Format(time.RFC3339Nano),
				oldest.Format(time.RFC3339Nano),
			)
		}
		if state.BackfillOldestProcessedAt == nil ||
			oldest.Before(*state.BackfillOldestProcessedAt) {
			state.BackfillOldestProcessedAt = &oldest
		}
	}

	if summary.BackfillComplete &&
		summary.StopReason == "retention_cutoff" {
		frontier := cutoff.UTC()
		state.BackfillOldestProcessedAt = &frontier
	}
	if summary.BackfillComplete &&
		(summary.StopReason == "end_of_feed" ||
			summary.StopReason == "empty_page") &&
		state.BackfillOldestProcessedAt == nil {
		retentionStart, err := marketRetentionStart(now, retentionDays)
		if err != nil {
			return err
		}
		frontier := retentionStart.UTC()
		state.BackfillOldestProcessedAt = &frontier
	}

	return refreshMarketAvailabilityFromFrontier(state, now, retentionDays)
}

func recordIncrementalCoverage(
	state *transactionBackfillState,
	coveredThrough time.Time,
	now time.Time,
	retentionDays int,
) error {
	covered := coveredThrough.UTC()
	state.IncrementalCoveredThrough = &covered
	state.UpdatedAt = now.UTC()
	return refreshMarketAvailabilityFromFrontier(state, now, retentionDays)
}

func marketReliableCoverageAdvanced(
	previousCoveredThrough *time.Time,
	currentCoveredThrough *time.Time,
	now time.Time,
) (bool, error) {
	currentDay, currentAvailable, err := market.LastReliableBrusselsDay(
		currentCoveredThrough,
		now,
	)
	if err != nil || !currentAvailable {
		return false, err
	}
	previousDay, previousAvailable, err := market.LastReliableBrusselsDay(
		previousCoveredThrough,
		now,
	)
	if err != nil {
		return false, err
	}
	return !previousAvailable || currentDay > previousDay, nil
}

func reconcileMarketRollups(
	db *gorm.DB,
	state transactionBackfillState,
	now time.Time,
	retentionDays int,
) error {
	if err := models.ReconcileMarketDailyRollups(
		db,
		state.AvailableSince,
		state.IncrementalCoveredThrough,
		now,
		retentionDays,
	); err != nil {
		return fmt.Errorf("reconcile market daily rollups: %w", err)
	}
	return nil
}

func persistPreparedMarketState(
	stateFile string,
	state *transactionBackfillState,
	now time.Time,
) error {
	state.UpdatedAt = now.UTC()
	if err := saveTransactionBackfillState(stateFile, *state); err != nil {
		return fmt.Errorf("save transaction backfill state: %w", err)
	}
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

	backfillState := transactionBackfillState{}
	if config.datasetEnabled(datasetTransactions) {
		backfillState, err = loadTransactionBackfillState(config.stateFile)
		if err != nil {
			slog.Error("Failed to load transaction backfill state", "error", err)
			os.Exit(1)
		}
	}

	slog.Info(
		"Scraper started",
		"backfill_complete", backfillState.Completed,
		"datasets", config.datasetNames(),
		"interval_seconds", int(config.interval/time.Second),
		"requests_per_minute", config.requestsPerMinute,
		"transaction_retention_days", config.transactionRetentionDays,
	)

	ticker := time.NewTicker(config.interval)
	defer ticker.Stop()
	retentionTicker := time.NewTicker(24 * time.Hour)
	defer retentionTicker.Stop()

	startedAt := time.Now()
	if err := pruneExpiredTransactions(db, startedAt, config.transactionRetentionDays); err != nil {
		os.Exit(1)
	}
	if config.datasetEnabled(datasetTransactions) {
		retentionDays := marketRollupRetentionDays(config.transactionRetentionDays)
		if err := prepareMarketCoverageState(
			&backfillState,
			startedAt,
			retentionDays,
		); err != nil {
			slog.Error("Failed to prepare market coverage state", "error", err)
			os.Exit(1)
		}
		if err := persistPreparedMarketState(
			config.stateFile,
			&backfillState,
			startedAt,
		); err != nil {
			slog.Error("Failed to persist market coverage state", "error", err)
			os.Exit(1)
		}
	}
	if err := scrapeConfigured(ctx, s, db, config, &backfillState); err != nil {
		os.Exit(1)
	}
	if config.datasetEnabled(datasetTransactions) {
		if err := reconcileMarketRollups(
			db,
			backfillState,
			time.Now(),
			marketRollupRetentionDays(config.transactionRetentionDays),
		); err != nil {
			slog.Error("Failed startup market rollup reconciliation", "error", err)
			os.Exit(1)
		}
	}

	for {
		select {
		case <-ctx.Done():
			slog.Info("Scraper shutting down")
			return
		case <-ticker.C:
			_ = scrapeConfigured(ctx, s, db, config, &backfillState)
		case <-retentionTicker.C:
			now := time.Now()
			if err := pruneExpiredTransactions(db, now, config.transactionRetentionDays); err != nil {
				continue
			}
			if config.datasetEnabled(datasetTransactions) {
				retentionDays := marketRollupRetentionDays(
					config.transactionRetentionDays,
				)
				if err := prepareMarketCoverageState(
					&backfillState,
					now,
					retentionDays,
				); err != nil {
					slog.Error("Failed to refresh daily market coverage", "error", err)
					continue
				}
				if err := persistPreparedMarketState(
					config.stateFile,
					&backfillState,
					now,
				); err != nil {
					slog.Error("Failed to persist daily market coverage", "error", err)
					continue
				}
				if err := reconcileMarketRollups(
					db,
					backfillState,
					now,
					retentionDays,
				); err != nil {
					slog.Error("Failed daily market rollup reconciliation", "error", err)
				}
			}
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
	backfillState *transactionBackfillState,
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
			incrementalStartedAt := time.Now()
			incrementalSummary, err := scrapeTransactionPages(
				ctx,
				s,
				gormTransactionStore{db: db},
				transactionCutoff(incrementalStartedAt, config.transactionRetentionDays),
				false,
				"",
				0,
			)
			logTransactionScrapeSummary(
				ctx,
				"incremental",
				incrementalStartedAt,
				incrementalSummary,
				err,
			)
			if err != nil {
				transactionErr = err
				return
			}

			retentionDays := marketRollupRetentionDays(
				config.transactionRetentionDays,
			)
			coverageRecordedAt := time.Now()
			incrementalState := *backfillState
			if err := recordIncrementalCoverage(
				&incrementalState,
				incrementalStartedAt,
				coverageRecordedAt,
				retentionDays,
			); err != nil {
				slog.Error("Failed to record incremental market coverage", "error", err)
				transactionErr = err
				return
			}
			coverageAdvanced, err := marketReliableCoverageAdvanced(
				backfillState.IncrementalCoveredThrough,
				incrementalState.IncrementalCoveredThrough,
				coverageRecordedAt,
			)
			if err != nil {
				slog.Error("Failed to compare incremental market coverage", "error", err)
				transactionErr = err
				return
			}
			if coverageAdvanced {
				if err := reconcileMarketRollups(
					db,
					incrementalState,
					coverageRecordedAt,
					retentionDays,
				); err != nil {
					slog.Error(
						"Failed advanced-coverage market reconciliation",
						"error",
						err,
					)
					transactionErr = err
					return
				}
			}
			if err := saveTransactionBackfillState(
				config.stateFile,
				incrementalState,
			); err != nil {
				slog.Error("Failed to save incremental market coverage", "error", err)
				transactionErr = err
				return
			}
			*backfillState = incrementalState

			if backfillState.Completed {
				return
			}

			backfillStartedAt := time.Now()
			backfillCutoff := transactionCutoff(
				backfillStartedAt,
				config.transactionRetentionDays,
			)
			backfillSummary, err := scrapeTransactionPages(
				ctx,
				s,
				gormTransactionStore{db: db},
				backfillCutoff,
				true,
				backfillState.Cursor,
				transactionBackfillPagesPerCycle,
			)
			logTransactionScrapeSummary(
				ctx,
				"backfill",
				backfillStartedAt,
				backfillSummary,
				err,
			)
			if err != nil {
				transactionErr = err
				return
			}

			nextState := *backfillState
			nextState.Cursor = backfillSummary.NextCursor
			nextState.Completed = backfillSummary.BackfillComplete
			nextState.CompletionReason = ""
			if nextState.Completed {
				nextState.CompletionReason = backfillSummary.StopReason
			}

			progressAt := time.Now()
			if err := mergeBackfillFrontier(
				&nextState,
				backfillSummary,
				backfillCutoff,
				progressAt,
				retentionDays,
			); err != nil {
				slog.Error("Failed to update market backfill frontier", "error", err)
				transactionErr = err
				return
			}
			nextState.UpdatedAt = progressAt.UTC()

			if err := reconcileMarketRollups(
				db,
				nextState,
				progressAt,
				retentionDays,
			); err != nil {
				slog.Error("Failed backfill market reconciliation", "error", err)
				transactionErr = err
				return
			}

			if err := saveTransactionBackfillState(config.stateFile, nextState); err != nil {
				slog.Error("Failed to save transaction backfill state", "error", err)
				transactionErr = err
				return
			}
			*backfillState = nextState
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

func logTransactionScrapeSummary(
	ctx context.Context,
	mode string,
	startedAt time.Time,
	summary transactionScrapeSummary,
	err error,
) {
	level := slog.LevelInfo
	if err != nil {
		level = slog.LevelError
	}
	slog.Log(
		ctx,
		level,
		"Transaction scrape cycle complete",
		"mode", mode,
		"duration_ms", time.Since(startedAt).Milliseconds(),
		"pages", summary.Pages,
		"received", summary.Received,
		"new", summary.New,
		"known", summary.Known,
		"expired", summary.Expired,
		"invalid", summary.Invalid,
		"stop_reason", summary.StopReason,
		"backfill_complete", summary.BackfillComplete,
		"oldest_processed_at", summary.OldestProcessedAt,
		"error", err,
	)
}

type transactionScrapeSummary struct {
	Pages             int
	Received          int
	New               int
	Known             int
	Expired           int
	Invalid           int
	StopReason        string
	NextCursor        string
	BackfillComplete  bool
	OldestProcessedAt *time.Time
}

type transactionBackfillState = market.BackfillState

func loadTransactionBackfillState(path string) (transactionBackfillState, error) {
	return market.LoadBackfillState(path)
}

func saveTransactionBackfillState(path string, state transactionBackfillState) error {
	return market.SaveBackfillState(path, state)
}

type transactionPageRecord struct {
	id        string
	raw       json.RawMessage
	createdAt time.Time
}

type transactionStore interface {
	findExistingIDs([]string) (map[string]struct{}, error)
	persist([]transactionPageRecord) (int, error)
}

func parseTransactionPageResponse(
	raw json.RawMessage,
) ([]json.RawMessage, string, error) {
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, "", err
	}
	errorPayload := strings.TrimSpace(string(envelope.Error))
	if errorPayload != "" && errorPayload != "null" {
		return nil, "", fmt.Errorf("transaction API returned an error envelope")
	}
	resultPayload := strings.TrimSpace(string(envelope.Result))
	if resultPayload == "" || resultPayload == "null" {
		return nil, "", fmt.Errorf("transaction API response is missing result")
	}

	var result struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(envelope.Result, &result); err != nil {
		return nil, "", fmt.Errorf("parse transaction result: %w", err)
	}
	dataPayload := strings.TrimSpace(string(result.Data))
	if dataPayload == "" || dataPayload == "null" {
		return nil, "", fmt.Errorf("transaction API response is missing result.data")
	}

	var data struct {
		Items      json.RawMessage `json:"items"`
		NextCursor string          `json:"nextCursor"`
	}
	if err := json.Unmarshal(result.Data, &data); err != nil {
		return nil, "", fmt.Errorf("parse transaction result data: %w", err)
	}
	itemsPayload := strings.TrimSpace(string(data.Items))
	if itemsPayload == "" || itemsPayload == "null" {
		return nil, "", fmt.Errorf(
			"transaction API response is missing result.data.items",
		)
	}

	var items []json.RawMessage
	if err := json.Unmarshal(data.Items, &items); err != nil {
		return nil, "", fmt.Errorf("parse transaction items: %w", err)
	}
	if items == nil {
		return nil, "", fmt.Errorf("transaction API items must be an array")
	}
	return items, data.NextCursor, nil
}

type gormTransactionStore struct {
	db *gorm.DB
}

func (s gormTransactionStore) findExistingIDs(ids []string) (map[string]struct{}, error) {
	return models.FindExistingTransactionIDs(s.db, ids)
}

func (s gormTransactionStore) persist(records []transactionPageRecord) (int, error) {
	if len(records) == 0 {
		return 0, nil
	}

	inserted := 0
	err := s.db.Transaction(func(tx *gorm.DB) error {
		for _, record := range records {
			created, err := models.InsertTransactionFromJSON(tx, record.raw)
			if err != nil {
				return fmt.Errorf("insert transaction %s: %w", record.id, err)
			}
			if created {
				inserted++
			}
		}
		return nil
	})
	return inserted, err
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
	if parsed.CreatedAt.IsZero() {
		return transactionPageRecord{}, false, false
	}
	record := transactionPageRecord{
		id:        parsed.ID,
		raw:       raw,
		createdAt: parsed.CreatedAt.UTC(),
	}
	if parsed.ID == "" {
		return record, false, false
	}
	if parsed.CreatedAt.Before(cutoff) {
		return record, true, true
	}

	return record, true, false
}

func scrapeTransactionPages(
	ctx context.Context,
	s requester,
	store transactionStore,
	cutoff time.Time,
	fullBackfill bool,
	startCursor string,
	maxPages int,
) (transactionScrapeSummary, error) {
	summary := transactionScrapeSummary{}
	cursor := startCursor
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

		items, nextCursor, err := parseTransactionPageResponse(raw)
		if err != nil {
			summary.StopReason = "response_error"
			return summary, err
		}

		summary.Pages++
		summary.Received += len(items)
		if len(items) == 0 {
			if nextCursor != "" {
				summary.StopReason = "invalid_cursor_boundary"
				return summary, fmt.Errorf(
					"empty transaction page returned a continuation cursor",
				)
			}
			summary.StopReason = "empty_page"
			summary.BackfillComplete = fullBackfill
			break
		}

		pageRecords := make([]transactionPageRecord, 0, len(items))
		var pageFrontier *time.Time
		for _, item := range items {
			record, valid, expired := parseTransactionPageRecord(item, cutoff)
			if !valid {
				summary.Invalid++
				continue
			}
			frontier := record.createdAt
			pageFrontier = &frontier
			if expired {
				summary.Expired++
				continue
			}
			pageRecords = append(pageRecords, record)
		}
		if pageFrontier == nil {
			summary.StopReason = "invalid_cursor_boundary"
			return summary, fmt.Errorf(
				"transaction page has no valid cursor-boundary item",
			)
		}
		frontier := pageFrontier.UTC()
		if summary.OldestProcessedAt != nil &&
			frontier.After(*summary.OldestProcessedAt) {
			summary.StopReason = "invalid_cursor_order"
			return summary, fmt.Errorf(
				"transaction cursor boundary moved forward from %s to %s",
				summary.OldestProcessedAt.UTC().Format(time.RFC3339Nano),
				frontier.Format(time.RFC3339Nano),
			)
		}
		cutoffReached := !pageFrontier.After(cutoff)

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
			inserted, err := store.persist(newRecords)
			if err != nil {
				summary.StopReason = "database_write_error"
				return summary, err
			}
			summary.New += inserted
			summary.Known += len(newRecords) - inserted
		} else {
			pendingIncremental = append(pendingIncremental, newRecords...)
		}

		summary.OldestProcessedAt = &frontier
		switch {
		case cutoffReached:
			summary.StopReason = "retention_cutoff"
			summary.BackfillComplete = fullBackfill
			retentionFrontier := cutoff.UTC()
			summary.OldestProcessedAt = &retentionFrontier
		case !fullBackfill && knownOverlap:
			summary.StopReason = "known_overlap"
		case nextCursor == "":
			summary.StopReason = "end_of_feed"
			summary.BackfillComplete = fullBackfill
		case maxPages > 0 && summary.Pages >= maxPages:
			summary.StopReason = "page_budget"
			summary.NextCursor = nextCursor
		default:
			cursor = nextCursor
			continue
		}
		break
	}

	if !fullBackfill {
		inserted, err := store.persist(pendingIncremental)
		if err != nil {
			summary.StopReason = "database_write_error"
			return summary, err
		}
		summary.New += inserted
		summary.Known += len(pendingIncremental) - inserted
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
