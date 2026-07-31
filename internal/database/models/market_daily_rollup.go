package models

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Hattorius/War-Era-Gateway/internal/market"
	"gorm.io/gorm"
)

const (
	marketTypeTrading            = "trading"
	marketTypeItemMarket         = "itemMarket"
	maxMarketDecimalInputText    = 256
	maxMarketDecimalContractText = 100
	maxMarketSafeInteger         = int64(1<<53 - 1)
)

var marketDecimalInputPattern = regexp.MustCompile(
	`^[+-]?([0-9]+([.][0-9]*)?|[.][0-9]+)([eE][+-]?[0-9]{1,4})?$`,
)

var marketDecimalContractPattern = regexp.MustCompile(
	`^(0|[1-9][0-9]*)([.][0-9]+)?$`,
)

type MarketDailyRollup struct {
	Day               time.Time `gorm:"type:date;primaryKey"`
	TradingTrades     int64     `gorm:"not null;default:0;check:trading_trades >= 0"`
	ItemMarketTrades  int64     `gorm:"not null;default:0;check:item_market_trades >= 0"`
	UnitsTraded       string    `gorm:"type:numeric;not null;default:0;check:units_traded >= 0"`
	RecordedVolumeBTC string    `gorm:"type:numeric;not null;default:0;check:recorded_volume_btc >= 0"`
	UpdatedAt         time.Time `gorm:"not null"`
}

type MarketContribution struct {
	TransactionType   string
	UnitsTraded       string
	RecordedVolumeBTC string
}

type MarketDailyPoint struct {
	Day               string
	TradingTrades     int64
	ItemMarketTrades  int64
	UnitsTraded       string
	RecordedVolumeBTC string
}

type MarketDailySnapshot struct {
	LatestTransactionAt *time.Time
	Days                []MarketDailyPoint
}

func canonicalNonNegativeMarketDecimal(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" ||
		len(value) > maxMarketDecimalInputText ||
		!marketDecimalInputPattern.MatchString(value) {
		return "", false
	}

	negative := false
	switch value[0] {
	case '-':
		negative = true
		value = value[1:]
	case '+':
		value = value[1:]
	}

	exponent := 0
	if exponentAt := strings.IndexAny(value, "eE"); exponentAt >= 0 {
		parsedExponent, err := strconv.Atoi(value[exponentAt+1:])
		if err != nil {
			return "", false
		}
		exponent = parsedExponent
		value = value[:exponentAt]
	}

	integerPart := value
	fractionalPart := ""
	if decimalAt := strings.IndexByte(value, '.'); decimalAt >= 0 {
		integerPart = value[:decimalAt]
		fractionalPart = value[decimalAt+1:]
	}
	digits := integerPart + fractionalPart
	if strings.Trim(digits, "0") == "" {
		return "0", true
	}
	if negative {
		return "", false
	}

	decimalAt := len(integerPart) + exponent
	leadingZeroes := len(digits) - len(strings.TrimLeft(digits, "0"))
	digits = digits[leadingZeroes:]
	decimalAt -= leadingZeroes

	for len(digits) > 0 &&
		len(digits) > max(decimalAt, 0) &&
		digits[len(digits)-1] == '0' {
		digits = digits[:len(digits)-1]
	}

	var outputLength int
	switch {
	case decimalAt <= 0:
		outputLength = 2 - decimalAt + len(digits)
	case decimalAt >= len(digits):
		outputLength = decimalAt
	default:
		outputLength = len(digits) + 1
	}
	if outputLength > maxMarketDecimalContractText {
		return "", false
	}

	var canonical string
	switch {
	case decimalAt <= 0:
		canonical = "0." + strings.Repeat("0", -decimalAt) + digits
	case decimalAt >= len(digits):
		canonical = digits + strings.Repeat("0", decimalAt-len(digits))
	default:
		canonical = digits[:decimalAt] + "." + digits[decimalAt:]
	}
	if len(canonical) > maxMarketDecimalContractText ||
		!marketDecimalContractPattern.MatchString(canonical) {
		return "", false
	}
	return canonical, true
}

func parseNonNegativeMarketDecimal(raw json.RawMessage) (string, bool) {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return "", false
	}

	if strings.HasPrefix(value, `"`) {
		var decoded string
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return "", false
		}
		value = decoded
	}

	return canonicalNonNegativeMarketDecimal(value)
}

func MarketContributionFromJSON(raw json.RawMessage) (*MarketContribution, error) {
	var parsed struct {
		TransactionType string          `json:"transactionType"`
		Quantity        json.RawMessage `json:"quantity"`
		Money           json.RawMessage `json:"money"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("parse market transaction contribution: %w", err)
	}
	if parsed.TransactionType != marketTypeTrading &&
		parsed.TransactionType != marketTypeItemMarket {
		return nil, nil
	}

	units := "0"
	if value, valid := parseNonNegativeMarketDecimal(parsed.Quantity); valid {
		units = value
	}
	volume := "0"
	if value, valid := parseNonNegativeMarketDecimal(parsed.Money); valid {
		volume = value
	}

	return &MarketContribution{
		TransactionType:   parsed.TransactionType,
		UnitsTraded:       units,
		RecordedVolumeBTC: volume,
	}, nil
}

func IncrementMarketDailyRollup(
	db *gorm.DB,
	createdAt time.Time,
	contribution MarketContribution,
) error {
	tradingTrades := int64(0)
	itemMarketTrades := int64(0)
	switch contribution.TransactionType {
	case marketTypeTrading:
		tradingTrades = 1
	case marketTypeItemMarket:
		itemMarketTrades = 1
	default:
		return nil
	}

	unitsTraded, valid := canonicalNonNegativeMarketDecimal(
		contribution.UnitsTraded,
	)
	if !valid {
		unitsTraded = "0"
	}
	recordedVolumeBTC, valid := canonicalNonNegativeMarketDecimal(
		contribution.RecordedVolumeBTC,
	)
	if !valid {
		recordedVolumeBTC = "0"
	}

	result := db.Exec(
		`insert into market_daily_rollups (
		   day,
		   trading_trades,
		   item_market_trades,
		   units_traded,
		   recorded_volume_btc,
		   updated_at
		 ) values (
		   ?::date,
		   ?,
		   ?,
		   ?::numeric,
		   ?::numeric,
		   now()
		 )
		 on conflict (day) do update set
		   trading_trades = market_daily_rollups.trading_trades + excluded.trading_trades,
		   item_market_trades = market_daily_rollups.item_market_trades + excluded.item_market_trades,
		   units_traded = market_daily_rollups.units_traded + excluded.units_traded,
		   recorded_volume_btc = market_daily_rollups.recorded_volume_btc + excluded.recorded_volume_btc,
		   updated_at = now()`,
		market.BrusselsDay(createdAt),
		tradingTrades,
		itemMarketTrades,
		unitsTraded,
		recordedVolumeBTC,
	)
	return result.Error
}

func ReconcileMarketDailyRollups(
	db *gorm.DB,
	availableSince string,
	incrementalCoveredThrough *time.Time,
	now time.Time,
	retentionDays int,
) error {
	if retentionDays <= 0 {
		return fmt.Errorf("market rollup retention days must be positive")
	}

	today, err := market.ParseBrusselsDay(market.BrusselsDay(now))
	if err != nil {
		return err
	}
	retentionStart := today.AddDate(0, 0, -(retentionDays - 1))

	reliableEndDay, hasReliableEnd, err := market.LastReliableBrusselsDay(
		incrementalCoveredThrough,
		now,
	)
	if err != nil {
		return err
	}
	if availableSince == "" || !hasReliableEnd {
		return nil
	}
	availableStart, err := market.ParseBrusselsDay(availableSince)
	if err != nil {
		return fmt.Errorf("parse market availability day: %w", err)
	}
	if availableStart.Before(retentionStart) {
		availableStart = retentionStart
	}
	reliableEnd, err := market.ParseBrusselsDay(reliableEndDay)
	if err != nil {
		return err
	}
	if reliableEnd.After(today) {
		reliableEnd = today
	}
	if availableStart.After(reliableEnd) {
		return nil
	}

	startDay := availableStart.Format(time.DateOnly)
	endDay := reliableEnd.Format(time.DateOnly)
	decimalInputSyntax := marketDecimalInputPattern.String()
	decimalContractSyntax := marketDecimalContractPattern.String()

	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`delete from market_daily_rollups`).Error; err != nil {
			return err
		}

		if err := tx.Exec(
			`with normalized as (
			   select
			     (created_at at time zone 'Europe/Brussels')::date as day,
			     transaction_type,
			     case
			       when jsonb_typeof(data -> 'quantity') in ('number', 'string')
			        and length(btrim(data ->> 'quantity')) between 1 and ?
			        and btrim(data ->> 'quantity') ~ ?
			       then btrim(data ->> 'quantity')::numeric
			     end as quantity_value,
			     case
			       when jsonb_typeof(data -> 'money') in ('number', 'string')
			        and length(btrim(data ->> 'money')) between 1 and ?
			        and btrim(data ->> 'money') ~ ?
			       then btrim(data ->> 'money')::numeric
			     end as money_value
			   from transactions
			   where transaction_type in ('trading', 'itemMarket')
			     and created_at >= (?::date::timestamp at time zone 'Europe/Brussels')
			     and created_at < ((?::date + 1)::timestamp at time zone 'Europe/Brussels')
			 ),
			 contributions as (
			   select
			     day,
			     transaction_type,
			     case
			       when quantity_value >= 0
			        and length(trim_scale(quantity_value)::text) <= ?
			        and trim_scale(quantity_value)::text ~ ?
			       then quantity_value
			       else 0
			     end as units_traded,
			     case
			       when money_value >= 0
			        and length(trim_scale(money_value)::text) <= ?
			        and trim_scale(money_value)::text ~ ?
			       then money_value
			       else 0
			     end as recorded_volume_btc
			   from normalized
			 ),
			 aggregated as (
			   select
			     day,
			     count(*) filter (where transaction_type = 'trading')::bigint as trading_trades,
			     count(*) filter (where transaction_type = 'itemMarket')::bigint as item_market_trades,
			     coalesce(sum(units_traded), 0) as units_traded,
			     coalesce(sum(recorded_volume_btc), 0) as recorded_volume_btc
			   from contributions
			   group by day
			 ),
			 calendar as (
			   select generate_series(?::date, ?::date, interval '1 day')::date as day
			 )
			 insert into market_daily_rollups (
			   day,
			   trading_trades,
			   item_market_trades,
			   units_traded,
			   recorded_volume_btc,
			   updated_at
			 )
			 select
			   calendar.day,
			   coalesce(aggregated.trading_trades, 0),
			   coalesce(aggregated.item_market_trades, 0),
			   coalesce(aggregated.units_traded, 0),
			   coalesce(aggregated.recorded_volume_btc, 0),
			   now()
			 from calendar
			 left join aggregated using (day)`,
			maxMarketDecimalInputText,
			decimalInputSyntax,
			maxMarketDecimalInputText,
			decimalInputSyntax,
			startDay,
			endDay,
			maxMarketDecimalContractText,
			decimalContractSyntax,
			maxMarketDecimalContractText,
			decimalContractSyntax,
			startDay,
			endDay,
		).Error; err != nil {
			return err
		}

		return validateMarketRollupAggregates(tx, startDay, endDay)
	})
}

func validateMarketDailyPoint(point *MarketDailyPoint) error {
	if point.TradingTrades < 0 ||
		point.TradingTrades > maxMarketSafeInteger ||
		point.ItemMarketTrades < 0 ||
		point.ItemMarketTrades > maxMarketSafeInteger {
		return fmt.Errorf("market trade count is outside the public contract")
	}

	unitsTraded, valid := canonicalNonNegativeMarketDecimal(point.UnitsTraded)
	if !valid {
		return fmt.Errorf("market units aggregate is outside the public contract")
	}
	recordedVolumeBTC, valid := canonicalNonNegativeMarketDecimal(
		point.RecordedVolumeBTC,
	)
	if !valid {
		return fmt.Errorf("market volume aggregate is outside the public contract")
	}
	point.UnitsTraded = unitsTraded
	point.RecordedVolumeBTC = recordedVolumeBTC
	return nil
}

func validateMarketRollupAggregates(
	db *gorm.DB,
	startDay string,
	endDay string,
) error {
	var rows []MarketDailyRollup
	if err := db.
		Where("day >= ?::date and day <= ?::date", startDay, endDay).
		Order("day asc").
		Find(&rows).
		Error; err != nil {
		return err
	}
	for _, row := range rows {
		point := MarketDailyPoint{
			Day:               row.Day.Format(time.DateOnly),
			TradingTrades:     row.TradingTrades,
			ItemMarketTrades:  row.ItemMarketTrades,
			UnitsTraded:       row.UnitsTraded,
			RecordedVolumeBTC: row.RecordedVolumeBTC,
		}
		if err := validateMarketDailyPoint(&point); err != nil {
			return fmt.Errorf("validate market rollup %s: %w", point.Day, err)
		}
	}
	return nil
}

func LoadMarketDailySnapshot(
	ctx context.Context,
	db *gorm.DB,
	days int,
	availableSince string,
	incrementalCoveredThrough *time.Time,
	now time.Time,
) (MarketDailySnapshot, error) {
	snapshot := MarketDailySnapshot{
		Days: make([]MarketDailyPoint, 0),
	}

	var latest sql.NullTime
	if err := db.WithContext(ctx).
		Model(&Transaction{}).
		Where("transaction_type in ?", []string{marketTypeTrading, marketTypeItemMarket}).
		Select("max(created_at)").
		Scan(&latest).
		Error; err != nil {
		return snapshot, err
	}
	if latest.Valid {
		value := latest.Time.UTC()
		snapshot.LatestTransactionAt = &value
	}

	reliableEndDay, hasReliableEnd, err := market.LastReliableBrusselsDay(
		incrementalCoveredThrough,
		now,
	)
	if err != nil {
		return snapshot, err
	}
	if availableSince == "" || !hasReliableEnd {
		return snapshot, nil
	}
	availableStart, err := market.ParseBrusselsDay(availableSince)
	if err != nil {
		return snapshot, fmt.Errorf("parse market availability day: %w", err)
	}
	today, err := market.ParseBrusselsDay(market.BrusselsDay(now))
	if err != nil {
		return snapshot, err
	}
	requestStart := today.AddDate(0, 0, -(days - 1))
	if requestStart.Before(availableStart) {
		requestStart = availableStart
	}
	reliableEnd, err := market.ParseBrusselsDay(reliableEndDay)
	if err != nil {
		return snapshot, err
	}
	if reliableEnd.After(today) {
		reliableEnd = today
	}
	if requestStart.After(reliableEnd) {
		return snapshot, nil
	}

	var stored []MarketDailyRollup
	if err := db.WithContext(ctx).
		Where("day >= ?::date and day <= ?::date",
			requestStart.Format(time.DateOnly),
			reliableEnd.Format(time.DateOnly),
		).
		Order("day asc").
		Find(&stored).
		Error; err != nil {
		return snapshot, err
	}

	for _, row := range stored {
		point := MarketDailyPoint{
			Day:               row.Day.Format(time.DateOnly),
			TradingTrades:     row.TradingTrades,
			ItemMarketTrades:  row.ItemMarketTrades,
			UnitsTraded:       row.UnitsTraded,
			RecordedVolumeBTC: row.RecordedVolumeBTC,
		}
		if err := validateMarketDailyPoint(&point); err != nil {
			return snapshot, fmt.Errorf(
				"validate market snapshot day %s: %w",
				point.Day,
				err,
			)
		}
		snapshot.Days = append(snapshot.Days, point)
	}

	return snapshot, nil
}
