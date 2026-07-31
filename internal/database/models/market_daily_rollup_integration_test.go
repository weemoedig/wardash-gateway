package models

import (
	"context"
	"encoding/json"
	"math/big"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func openMarketIntegrationDB(t *testing.T) *gorm.DB {
	t.Helper()

	dsn := strings.TrimSpace(os.Getenv("GATEWAY_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL is not set")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse GATEWAY_TEST_DATABASE_URL: %v", err)
	}
	if strings.TrimPrefix(parsed.Path, "/") != "gateway_test" {
		t.Fatalf("refusing integration test database %q; want gateway_test", parsed.Path)
	}

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("connect integration database: %v", err)
	}
	resetMarketIntegrationTables(t, db)
	t.Cleanup(func() {
		_ = db.Migrator().DropTable(
			&TransactionParticipant{},
			&Transaction{},
			&MarketDailyRollup{},
		)
	})
	return db
}

func resetMarketIntegrationTables(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.Migrator().DropTable(
		&TransactionParticipant{},
		&Transaction{},
		&MarketDailyRollup{},
	); err != nil {
		t.Fatalf("reset integration tables: %v", err)
	}
	if err := db.AutoMigrate(
		&Transaction{},
		&TransactionParticipant{},
		&MarketDailyRollup{},
	); err != nil {
		t.Fatalf("migrate integration tables: %v", err)
	}
}

func marketTransactionJSON(
	id string,
	transactionType string,
	createdAt time.Time,
	quantity any,
	money any,
) json.RawMessage {
	raw, err := json.Marshal(map[string]any{
		"_id":             id,
		"transactionType": transactionType,
		"createdAt":       createdAt.UTC().Format(time.RFC3339),
		"updatedAt":       createdAt.UTC().Format(time.RFC3339),
		"quantity":        quantity,
		"money":           money,
	})
	if err != nil {
		panic(err)
	}
	return raw
}

func requireNumericEqual(t *testing.T, got string, want string) {
	t.Helper()
	gotNumber, ok := new(big.Rat).SetString(got)
	if !ok {
		t.Fatalf("parse numeric result %q", got)
	}
	wantNumber, ok := new(big.Rat).SetString(want)
	if !ok {
		t.Fatalf("parse expected numeric %q", want)
	}
	if gotNumber.Cmp(wantNumber) != 0 {
		t.Fatalf("numeric value = %s, want %s", got, want)
	}
}

func TestMarketInsertAndRollupAreAtomicAndIdempotentIntegration(t *testing.T) {
	db := openMarketIntegrationDB(t)
	createdAt := time.Date(2026, time.July, 30, 20, 30, 0, 0, time.UTC)
	raw := marketTransactionJSON(
		"aaaaaaaaaaaaaaaaaaaaaaaa",
		"trading",
		createdAt,
		3,
		10,
	)

	if err := db.Migrator().DropTable(&MarketDailyRollup{}); err != nil {
		t.Fatalf("drop rollup table: %v", err)
	}
	if inserted, err := InsertTransactionFromJSON(db, raw); err == nil || inserted {
		t.Fatalf("insert without rollup table = (%v, %v), want rollback error", inserted, err)
	}
	var transactionCount int64
	if err := db.Model(&Transaction{}).Count(&transactionCount).Error; err != nil {
		t.Fatalf("count rolled-back transactions: %v", err)
	}
	if transactionCount != 0 {
		t.Fatalf("transaction count after rollup error = %d, want 0", transactionCount)
	}
	if err := db.AutoMigrate(&MarketDailyRollup{}); err != nil {
		t.Fatalf("restore rollup table: %v", err)
	}

	inserted, err := InsertTransactionFromJSON(db, raw)
	if err != nil {
		t.Fatalf("first InsertTransactionFromJSON returned error: %v", err)
	}
	if !inserted {
		t.Fatal("first insert reported inserted=false")
	}
	inserted, err = InsertTransactionFromJSON(db, raw)
	if err != nil {
		t.Fatalf("retry InsertTransactionFromJSON returned error: %v", err)
	}
	if inserted {
		t.Fatal("retry insert reported inserted=true")
	}

	var row MarketDailyRollup
	if err := db.First(&row, "day = ?::date", "2026-07-30").Error; err != nil {
		t.Fatalf("load rollup row: %v", err)
	}
	if row.TradingTrades != 1 || row.ItemMarketTrades != 0 {
		t.Fatalf("trade counts = (%d, %d), want (1, 0)",
			row.TradingTrades,
			row.ItemMarketTrades,
		)
	}
	requireNumericEqual(t, row.UnitsTraded, "3")
	requireNumericEqual(t, row.RecordedVolumeBTC, "10")
}

func TestConcurrentTransactionRetriesIncrementRollupOnceIntegration(t *testing.T) {
	db := openMarketIntegrationDB(t)
	raw := marketTransactionJSON(
		"bbbbbbbbbbbbbbbbbbbbbbbb",
		"itemMarket",
		time.Date(2026, time.July, 30, 21, 0, 0, 0, time.UTC),
		"2.5",
		"4.75",
	)

	var insertedCount atomic.Int64
	var errorsMu sync.Mutex
	var errorsSeen []error
	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			inserted, err := InsertTransactionFromJSON(db, raw)
			if err != nil {
				errorsMu.Lock()
				errorsSeen = append(errorsSeen, err)
				errorsMu.Unlock()
				return
			}
			if inserted {
				insertedCount.Add(1)
			}
		}()
	}
	wait.Wait()

	if len(errorsSeen) > 0 {
		t.Fatalf("concurrent inserts returned errors: %v", errorsSeen)
	}
	if got := insertedCount.Load(); got != 1 {
		t.Fatalf("inserted count = %d, want 1", got)
	}

	var row MarketDailyRollup
	if err := db.First(&row, "day = ?::date", "2026-07-30").Error; err != nil {
		t.Fatalf("load rollup row: %v", err)
	}
	if row.ItemMarketTrades != 1 {
		t.Fatalf("item market trades = %d, want 1", row.ItemMarketTrades)
	}
	requireNumericEqual(t, row.UnitsTraded, "2.5")
	requireNumericEqual(t, row.RecordedVolumeBTC, "4.75")
}

func TestMarketReconciliationFiltersValidDataAndRepairsDriftIntegration(t *testing.T) {
	db := openMarketIntegrationDB(t)
	transactions := []Transaction{
		{
			ID:              "cccccccccccccccccccccccc",
			TransactionType: "trading",
			Data: datatypes.JSON(marketTransactionJSON(
				"cccccccccccccccccccccccc",
				"trading",
				time.Date(2026, time.March, 28, 23, 30, 0, 0, time.UTC),
				3,
				10,
			)),
			CreatedAt: time.Date(2026, time.March, 28, 23, 30, 0, 0, time.UTC),
			UpdatedAt: time.Date(2026, time.March, 28, 23, 30, 0, 0, time.UTC),
		},
		{
			ID:              "dddddddddddddddddddddddd",
			TransactionType: "itemMarket",
			Data: datatypes.JSON(marketTransactionJSON(
				"dddddddddddddddddddddddd",
				"itemMarket",
				time.Date(2026, time.March, 28, 23, 45, 0, 0, time.UTC),
				"2.5",
				"4.75",
			)),
			CreatedAt: time.Date(2026, time.March, 28, 23, 45, 0, 0, time.UTC),
			UpdatedAt: time.Date(2026, time.March, 28, 23, 45, 0, 0, time.UTC),
		},
		{
			ID:              "eeeeeeeeeeeeeeeeeeeeeeee",
			TransactionType: "trading",
			Data: datatypes.JSON(marketTransactionJSON(
				"eeeeeeeeeeeeeeeeeeeeeeee",
				"trading",
				time.Date(2026, time.March, 29, 22, 30, 0, 0, time.UTC),
				-5,
				"invalid",
			)),
			CreatedAt: time.Date(2026, time.March, 29, 22, 30, 0, 0, time.UTC),
			UpdatedAt: time.Date(2026, time.March, 29, 22, 30, 0, 0, time.UTC),
		},
		{
			ID:              "ffffffffffffffffffffffff",
			TransactionType: "donation",
			Data: datatypes.JSON(marketTransactionJSON(
				"ffffffffffffffffffffffff",
				"donation",
				time.Date(2026, time.March, 29, 22, 45, 0, 0, time.UTC),
				100,
				100,
			)),
			CreatedAt: time.Date(2026, time.March, 29, 22, 45, 0, 0, time.UTC),
			UpdatedAt: time.Date(2026, time.March, 29, 22, 45, 0, 0, time.UTC),
		},
	}
	if err := db.Create(&transactions).Error; err != nil {
		t.Fatalf("insert reconciliation fixtures: %v", err)
	}
	if err := db.Create(&MarketDailyRollup{
		Day:               time.Date(2026, time.March, 29, 0, 0, 0, 0, time.UTC),
		TradingTrades:     999,
		ItemMarketTrades:  999,
		UnitsTraded:       "999",
		RecordedVolumeBTC: "999",
		UpdatedAt:         time.Now(),
	}).Error; err != nil {
		t.Fatalf("insert drift fixture: %v", err)
	}

	now := time.Date(2026, time.March, 30, 12, 0, 0, 0, time.UTC)
	for range 2 {
		if err := ReconcileMarketDailyRollups(
			db,
			"2026-03-29",
			&now,
			now,
			30,
		); err != nil {
			t.Fatalf("ReconcileMarketDailyRollups returned error: %v", err)
		}
	}

	var rows []MarketDailyRollup
	if err := db.Order("day asc").Find(&rows).Error; err != nil {
		t.Fatalf("load reconciled rows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rollup rows = %d, want 2", len(rows))
	}
	if rows[0].Day.Format(time.DateOnly) != "2026-03-29" ||
		rows[0].TradingTrades != 1 ||
		rows[0].ItemMarketTrades != 1 {
		t.Fatalf("first rollup = %+v, want both supported trades on Brussels 2026-03-29", rows[0])
	}
	requireNumericEqual(t, rows[0].UnitsTraded, "5.5")
	requireNumericEqual(t, rows[0].RecordedVolumeBTC, "14.75")
	if rows[1].Day.Format(time.DateOnly) != "2026-03-30" ||
		rows[1].TradingTrades != 1 ||
		rows[1].ItemMarketTrades != 0 {
		t.Fatalf("second rollup = %+v, want one trading trade on Brussels 2026-03-30", rows[1])
	}
	requireNumericEqual(t, rows[1].UnitsTraded, "0")
	requireNumericEqual(t, rows[1].RecordedVolumeBTC, "0")

	snapshot, err := LoadMarketDailySnapshot(
		context.Background(),
		db,
		2,
		"2026-03-29",
		&now,
		now,
	)
	if err != nil {
		t.Fatalf("LoadMarketDailySnapshot returned error: %v", err)
	}
	if len(snapshot.Days) != 2 ||
		snapshot.Days[0].Day != "2026-03-29" ||
		snapshot.Days[1].Day != "2026-03-30" {
		t.Fatalf("snapshot days = %+v, want two ordered Brussels days", snapshot.Days)
	}
	wantLatest := time.Date(2026, time.March, 29, 22, 30, 0, 0, time.UTC)
	if snapshot.LatestTransactionAt == nil ||
		!snapshot.LatestTransactionAt.Equal(wantLatest) {
		t.Fatalf(
			"latest relevant transaction = %v, want %s",
			snapshot.LatestTransactionAt,
			wantLatest,
		)
	}
}

func TestMarketReconciliationUsesFallDSTBoundaryIntegration(t *testing.T) {
	db := openMarketIntegrationDB(t)
	timestamps := []time.Time{
		time.Date(2026, time.October, 24, 21, 59, 59, 0, time.UTC),
		time.Date(2026, time.October, 24, 22, 0, 0, 0, time.UTC),
		time.Date(2026, time.October, 25, 22, 59, 59, 0, time.UTC),
		time.Date(2026, time.October, 25, 23, 0, 0, 0, time.UTC),
	}
	ids := []string{
		"gggggggggggggggggggggggg",
		"hhhhhhhhhhhhhhhhhhhhhhhh",
		"iiiiiiiiiiiiiiiiiiiiiiii",
		"jjjjjjjjjjjjjjjjjjjjjjjj",
	}
	transactions := make([]Transaction, len(timestamps))
	for index, createdAt := range timestamps {
		raw := marketTransactionJSON(ids[index], "trading", createdAt, 1, 1)
		transactions[index] = Transaction{
			ID:              ids[index],
			TransactionType: "trading",
			Data:            datatypes.JSON(raw),
			CreatedAt:       createdAt,
			UpdatedAt:       createdAt,
		}
	}
	if err := db.Create(&transactions).Error; err != nil {
		t.Fatalf("insert fall DST fixtures: %v", err)
	}

	now := time.Date(2026, time.October, 26, 12, 0, 0, 0, time.UTC)
	if err := ReconcileMarketDailyRollups(
		db,
		"2026-10-24",
		&now,
		now,
		30,
	); err != nil {
		t.Fatalf("ReconcileMarketDailyRollups returned error: %v", err)
	}

	var rows []MarketDailyRollup
	if err := db.Order("day asc").Find(&rows).Error; err != nil {
		t.Fatalf("load fall DST rollups: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rollup rows = %d, want 3", len(rows))
	}
	wantCounts := []int64{1, 2, 1}
	for index, want := range wantCounts {
		if rows[index].TradingTrades != want {
			t.Fatalf(
				"day %s trading trades = %d, want %d",
				rows[index].Day.Format(time.DateOnly),
				rows[index].TradingTrades,
				want,
			)
		}
	}
}

func TestMarketRollupRetentionIntegration(t *testing.T) {
	db := openMarketIntegrationDB(t)
	if err := db.Create(&MarketDailyRollup{
		Day:               time.Date(2026, time.June, 30, 0, 0, 0, 0, time.UTC),
		UnitsTraded:       "999",
		RecordedVolumeBTC: "999",
		UpdatedAt:         time.Now(),
	}).Error; err != nil {
		t.Fatalf("insert retention fixtures: %v", err)
	}

	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	if err := ReconcileMarketDailyRollups(
		db,
		"2026-06-30",
		&now,
		now,
		30,
	); err != nil {
		t.Fatalf("ReconcileMarketDailyRollups returned error: %v", err)
	}

	var count int64
	if err := db.Model(&MarketDailyRollup{}).Count(&count).Error; err != nil {
		t.Fatalf("count retained rollups: %v", err)
	}
	if count != 30 {
		t.Fatalf("retained rollups = %d, want 30 calendar days", count)
	}
	var first MarketDailyRollup
	if err := db.Order("day asc").First(&first).Error; err != nil {
		t.Fatalf("load first retained rollup: %v", err)
	}
	if got := first.Day.Format(time.DateOnly); got != "2026-07-01" {
		t.Fatalf("first retained day = %s, want 2026-07-01", got)
	}
	requireNumericEqual(t, first.UnitsTraded, "0")
	requireNumericEqual(t, first.RecordedVolumeBTC, "0")
}

func TestMarketCoverageBoundsNeverUseLooseDatabaseRowsIntegration(t *testing.T) {
	db := openMarketIntegrationDB(t)
	looseOldAt := time.Date(2026, time.July, 1, 10, 0, 0, 0, time.UTC)
	reliableAt := time.Date(2026, time.July, 29, 10, 0, 0, 0, time.UTC)
	transactions := []Transaction{
		{
			ID:              "kkkkkkkkkkkkkkkkkkkkkkkk",
			TransactionType: "trading",
			Data: datatypes.JSON(marketTransactionJSON(
				"kkkkkkkkkkkkkkkkkkkkkkkk",
				"trading",
				looseOldAt,
				99,
				99,
			)),
			CreatedAt: looseOldAt,
			UpdatedAt: looseOldAt,
		},
		{
			ID:              "llllllllllllllllllllllll",
			TransactionType: "trading",
			Data: datatypes.JSON(marketTransactionJSON(
				"llllllllllllllllllllllll",
				"trading",
				reliableAt,
				2,
				3,
			)),
			CreatedAt: reliableAt,
			UpdatedAt: reliableAt,
		},
	}
	if err := db.Create(&transactions).Error; err != nil {
		t.Fatalf("insert coverage fixtures: %v", err)
	}

	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	if err := ReconcileMarketDailyRollups(
		db,
		"2026-07-29",
		&now,
		now,
		30,
	); err != nil {
		t.Fatalf("ReconcileMarketDailyRollups returned error: %v", err)
	}

	var rows []MarketDailyRollup
	if err := db.Order("day asc").Find(&rows).Error; err != nil {
		t.Fatalf("load coverage-bounded rows: %v", err)
	}
	if len(rows) != 2 ||
		rows[0].Day.Format(time.DateOnly) != "2026-07-29" ||
		rows[1].Day.Format(time.DateOnly) != "2026-07-30" {
		t.Fatalf("rows = %+v, want only explicit 2026-07-29..30 coverage", rows)
	}
	requireNumericEqual(t, rows[0].UnitsTraded, "2")

	snapshot, err := LoadMarketDailySnapshot(
		context.Background(),
		db,
		30,
		"2026-07-29",
		nil,
		now,
	)
	if err != nil {
		t.Fatalf("LoadMarketDailySnapshot without watermark returned error: %v", err)
	}
	if len(snapshot.Days) != 0 {
		t.Fatalf("snapshot days = %+v, want no days without upper watermark", snapshot.Days)
	}

	if err := ReconcileMarketDailyRollups(
		db,
		"2026-07-29",
		nil,
		now,
		30,
	); err != nil {
		t.Fatalf("no-coverage reconciliation returned error: %v", err)
	}
	var preserved int64
	if err := db.Model(&MarketDailyRollup{}).Count(&preserved).Error; err != nil {
		t.Fatalf("count preserved rollups: %v", err)
	}
	if preserved != 2 {
		t.Fatalf("preserved rollups = %d, want last-known-good 2", preserved)
	}
}

func TestMarketSnapshotExcludesStalePartialDayAfterMidnightIntegration(t *testing.T) {
	db := openMarketIntegrationDB(t)
	for _, day := range []string{"2026-07-29", "2026-07-30"} {
		parsedDay, err := time.Parse(time.DateOnly, day)
		if err != nil {
			t.Fatalf("parse fixture day: %v", err)
		}
		if err := db.Create(&MarketDailyRollup{
			Day:               parsedDay,
			TradingTrades:     1,
			UnitsTraded:       "1",
			RecordedVolumeBTC: "1",
			UpdatedAt:         time.Now(),
		}).Error; err != nil {
			t.Fatalf("insert partial-day fixture: %v", err)
		}
	}

	coveredThrough := time.Date(
		2026,
		time.July,
		30,
		21,
		50,
		0,
		0,
		time.UTC,
	)
	now := time.Date(2026, time.July, 30, 22, 30, 0, 0, time.UTC)
	snapshot, err := LoadMarketDailySnapshot(
		context.Background(),
		db,
		30,
		"2026-07-29",
		&coveredThrough,
		now,
	)
	if err != nil {
		t.Fatalf("LoadMarketDailySnapshot returned error: %v", err)
	}
	if len(snapshot.Days) != 1 || snapshot.Days[0].Day != "2026-07-29" {
		t.Fatalf("snapshot days = %+v, want only last fully covered day", snapshot.Days)
	}
}

func TestMarketSnapshotOmitsMissingRowAndPreservesExplicitZeroIntegration(
	t *testing.T,
) {
	db := openMarketIntegrationDB(t)
	rows := []MarketDailyRollup{
		{
			Day:               time.Date(2026, time.July, 28, 0, 0, 0, 0, time.UTC),
			UnitsTraded:       "0",
			RecordedVolumeBTC: "0",
			UpdatedAt:         time.Now(),
		},
		{
			Day:               time.Date(2026, time.July, 30, 0, 0, 0, 0, time.UTC),
			TradingTrades:     2,
			ItemMarketTrades:  1,
			UnitsTraded:       "3",
			RecordedVolumeBTC: "4",
			UpdatedAt:         time.Now(),
		},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("insert sparse rollup fixtures: %v", err)
	}

	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	snapshot, err := LoadMarketDailySnapshot(
		context.Background(),
		db,
		3,
		"2026-07-28",
		&now,
		now,
	)
	if err != nil {
		t.Fatalf("LoadMarketDailySnapshot returned error: %v", err)
	}
	if len(snapshot.Days) != 2 ||
		snapshot.Days[0].Day != "2026-07-28" ||
		snapshot.Days[1].Day != "2026-07-30" {
		t.Fatalf(
			"snapshot days = %+v, want explicit rows with missing 2026-07-29 omitted",
			snapshot.Days,
		)
	}
	if snapshot.Days[0].TradingTrades != 0 ||
		snapshot.Days[0].ItemMarketTrades != 0 ||
		snapshot.Days[0].UnitsTraded != "0" ||
		snapshot.Days[0].RecordedVolumeBTC != "0" {
		t.Fatalf(
			"explicit zero row = %+v, want preserved canonical zeroes",
			snapshot.Days[0],
		)
	}
}

func TestMarketReconciliationRejectsAggregateOverflowAndPreservesLastGoodIntegration(
	t *testing.T,
) {
	db := openMarketIntegrationDB(t)
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	if err := db.Create(&MarketDailyRollup{
		Day:               time.Date(2026, time.July, 30, 0, 0, 0, 0, time.UTC),
		TradingTrades:     7,
		UnitsTraded:       "7",
		RecordedVolumeBTC: "7",
		UpdatedAt:         time.Now(),
	}).Error; err != nil {
		t.Fatalf("insert last-known-good rollup: %v", err)
	}

	maximumIndividual := strings.Repeat("9", maxMarketDecimalContractText)
	transactions := []Transaction{
		{
			ID:              "mmmmmmmmmmmmmmmmmmmmmmmm",
			TransactionType: "trading",
			Data: datatypes.JSON(marketTransactionJSON(
				"mmmmmmmmmmmmmmmmmmmmmmmm",
				"trading",
				now,
				maximumIndividual,
				1,
			)),
			CreatedAt: now,
			UpdatedAt: now,
		},
		{
			ID:              "nnnnnnnnnnnnnnnnnnnnnnnn",
			TransactionType: "itemMarket",
			Data: datatypes.JSON(marketTransactionJSON(
				"nnnnnnnnnnnnnnnnnnnnnnnn",
				"itemMarket",
				now.Add(time.Second),
				1,
				1,
			)),
			CreatedAt: now.Add(time.Second),
			UpdatedAt: now.Add(time.Second),
		},
	}
	if err := db.Create(&transactions).Error; err != nil {
		t.Fatalf("insert overflow fixtures: %v", err)
	}

	if err := ReconcileMarketDailyRollups(
		db,
		"2026-07-30",
		&now,
		now,
		30,
	); err == nil {
		t.Fatal("ReconcileMarketDailyRollups returned nil, want aggregate overflow error")
	}

	var preserved MarketDailyRollup
	if err := db.First(&preserved, "day = ?::date", "2026-07-30").Error; err != nil {
		t.Fatalf("load preserved rollup: %v", err)
	}
	if preserved.TradingTrades != 7 {
		t.Fatalf("preserved trades = %d, want 7", preserved.TradingTrades)
	}
	requireNumericEqual(t, preserved.UnitsTraded, "7")
}

func TestMarketSnapshotFailsClosedOnStoredAggregateOverflowIntegration(t *testing.T) {
	db := openMarketIntegrationDB(t)
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	if err := db.Create(&MarketDailyRollup{
		Day:           time.Date(2026, time.July, 30, 0, 0, 0, 0, time.UTC),
		TradingTrades: 1,
		UnitsTraded: strings.Repeat(
			"9",
			maxMarketDecimalContractText+1,
		),
		RecordedVolumeBTC: "1",
		UpdatedAt:         time.Now(),
	}).Error; err != nil {
		t.Fatalf("insert overflow rollup: %v", err)
	}

	if _, err := LoadMarketDailySnapshot(
		context.Background(),
		db,
		1,
		"2026-07-30",
		&now,
		now,
	); err == nil {
		t.Fatal("LoadMarketDailySnapshot returned nil, want fail-closed error")
	}
}

func TestMarketReconciliationKeepsTradesForOutOfBoundIndividualsIntegration(
	t *testing.T,
) {
	db := openMarketIntegrationDB(t)
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	transactions := []Transaction{
		{
			ID:              "oooooooooooooooooooooooo",
			TransactionType: "trading",
			Data: datatypes.JSON(marketTransactionJSON(
				"oooooooooooooooooooooooo",
				"trading",
				now,
				"1e100",
				strings.Repeat("9", maxMarketDecimalContractText+1),
			)),
			CreatedAt: now,
			UpdatedAt: now,
		},
		{
			ID:              "pppppppppppppppppppppppp",
			TransactionType: "itemMarket",
			Data: datatypes.JSON(marketTransactionJSON(
				"pppppppppppppppppppppppp",
				"itemMarket",
				now.Add(time.Second),
				"1e2",
				"42.31000000",
			)),
			CreatedAt: now.Add(time.Second),
			UpdatedAt: now.Add(time.Second),
		},
	}
	if err := db.Create(&transactions).Error; err != nil {
		t.Fatalf("insert decimal fixtures: %v", err)
	}

	if err := ReconcileMarketDailyRollups(
		db,
		"2026-07-30",
		&now,
		now,
		30,
	); err != nil {
		t.Fatalf("ReconcileMarketDailyRollups returned error: %v", err)
	}

	snapshot, err := LoadMarketDailySnapshot(
		context.Background(),
		db,
		1,
		"2026-07-30",
		&now,
		now,
	)
	if err != nil {
		t.Fatalf("LoadMarketDailySnapshot returned error: %v", err)
	}
	if len(snapshot.Days) != 1 {
		t.Fatalf("snapshot days = %+v, want one day", snapshot.Days)
	}
	point := snapshot.Days[0]
	if point.TradingTrades != 1 ||
		point.ItemMarketTrades != 1 ||
		point.UnitsTraded != "100" ||
		point.RecordedVolumeBTC != "42.31" {
		t.Fatalf("snapshot point = %+v, want counted trades and bounded sums", point)
	}
}
