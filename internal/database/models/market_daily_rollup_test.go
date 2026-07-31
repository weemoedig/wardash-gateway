package models

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestMarketContributionUsesOnlySupportedTypesAndDoesNotMultiplyMoney(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantNil    bool
		wantType   string
		wantUnits  string
		wantVolume string
	}{
		{
			name:       "trading",
			raw:        `{"transactionType":"trading","quantity":3,"money":10}`,
			wantType:   "trading",
			wantUnits:  "3",
			wantVolume: "10",
		},
		{
			name:       "item market decimal strings",
			raw:        `{"transactionType":"itemMarket","quantity":"2.500","money":"4.75"}`,
			wantType:   "itemMarket",
			wantUnits:  "2.5",
			wantVolume: "4.75",
		},
		{
			name:    "unrelated transaction type",
			raw:     `{"transactionType":"donation","quantity":999,"money":999}`,
			wantNil: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := MarketContributionFromJSON(json.RawMessage(test.raw))
			if err != nil {
				t.Fatalf("MarketContributionFromJSON returned error: %v", err)
			}
			if test.wantNil {
				if got != nil {
					t.Fatalf("contribution = %+v, want nil", got)
				}
				return
			}
			if got == nil ||
				got.TransactionType != test.wantType ||
				got.UnitsTraded != test.wantUnits ||
				got.RecordedVolumeBTC != test.wantVolume {
				t.Fatalf(
					"contribution = %+v, want type=%s units=%s volume=%s",
					got,
					test.wantType,
					test.wantUnits,
					test.wantVolume,
				)
			}
		})
	}
}

func TestMarketContributionIgnoresMissingNegativeAndInvalidDecimals(t *testing.T) {
	tests := []struct {
		name       string
		quantity   string
		money      string
		wantUnits  string
		wantVolume string
	}{
		{
			name:       "missing values",
			quantity:   "",
			money:      "",
			wantUnits:  "0",
			wantVolume: "0",
		},
		{
			name:       "negative values",
			quantity:   `-1`,
			money:      `"-0.01"`,
			wantUnits:  "0",
			wantVolume: "0",
		},
		{
			name:       "invalid values",
			quantity:   `"three"`,
			money:      `true`,
			wantUnits:  "0",
			wantVolume: "0",
		},
		{
			name:       "zero and exponent become canonical plain decimals",
			quantity:   `0`,
			money:      `"1.25e2"`,
			wantUnits:  "0",
			wantVolume: "125",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := `{"transactionType":"trading"`
			if test.quantity != "" {
				raw += `,"quantity":` + test.quantity
			}
			if test.money != "" {
				raw += `,"money":` + test.money
			}
			raw += `}`

			got, err := MarketContributionFromJSON(json.RawMessage(raw))
			if err != nil {
				t.Fatalf("MarketContributionFromJSON returned error: %v", err)
			}
			if got.UnitsTraded != test.wantUnits ||
				got.RecordedVolumeBTC != test.wantVolume {
				t.Fatalf(
					"contribution = %+v, want units=%s volume=%s",
					got,
					test.wantUnits,
					test.wantVolume,
				)
			}
		})
	}
}

func TestCanonicalMarketDecimalsMatchWorkerContract(t *testing.T) {
	maximumContractInteger := "1" + strings.Repeat("0", 99)
	tests := []struct {
		name  string
		input string
		want  string
		valid bool
	}{
		{
			name:  "plain integer",
			input: "0",
			want:  "0",
			valid: true,
		},
		{
			name:  "leading and trailing zeroes",
			input: "00042.31000000",
			want:  "42.31",
			valid: true,
		},
		{
			name:  "positive exponent",
			input: "+1.015e2",
			want:  "101.5",
			valid: true,
		},
		{
			name:  "negative exponent",
			input: "12e-3",
			want:  "0.012",
			valid: true,
		},
		{
			name:  "negative zero",
			input: "-0.000e9999",
			want:  "0",
			valid: true,
		},
		{
			name:  "maximum output length",
			input: maximumContractInteger,
			want:  maximumContractInteger,
			valid: true,
		},
		{
			name:  "expanded exponent exceeds output contract",
			input: "1e100",
			valid: false,
		},
		{
			name:  "mantissa exceeds input bound",
			input: strings.Repeat("1", maxMarketDecimalInputText+1),
			valid: false,
		},
		{
			name:  "negative nonzero",
			input: "-0.01",
			valid: false,
		},
		{
			name:  "worker rejects exponent output",
			input: "NaN",
			valid: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, valid := canonicalNonNegativeMarketDecimal(test.input)
			if valid != test.valid || got != test.want {
				t.Fatalf("canonical decimal = (%q, %t), want (%q, %t)",
					got,
					valid,
					test.want,
					test.valid,
				)
			}
			if valid &&
				(len(got) > maxMarketDecimalContractText ||
					!marketDecimalContractPattern.MatchString(got)) {
				t.Fatalf("canonical output %q does not match worker schema", got)
			}
		})
	}
}

func TestOutOfBoundIndividualDecimalKeepsTradeContribution(t *testing.T) {
	raw := json.RawMessage(`{
		"transactionType":"trading",
		"quantity":"1e100",
		"money":"99999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999"
	}`)

	contribution, err := MarketContributionFromJSON(raw)
	if err != nil {
		t.Fatalf("MarketContributionFromJSON returned error: %v", err)
	}
	if contribution == nil ||
		contribution.TransactionType != "trading" ||
		contribution.UnitsTraded != "0" ||
		contribution.RecordedVolumeBTC != "0" {
		t.Fatalf("contribution = %+v, want counted trade with zero numeric sums", contribution)
	}
}

func TestTransactionIdentityNeverFallsBackToOfferCreatedAt(t *testing.T) {
	raw := json.RawMessage(`{
		"transactionType":"itemMarket",
		"offerCreatedAt":"not-an-upstream-transaction-id",
		"createdAt":"2026-07-30T20:00:00Z",
		"updatedAt":"2026-07-30T20:00:00Z",
		"quantity":1,
		"money":2
	}`)

	if _, _, _, err := parseTransactionJSON(raw); err == nil {
		t.Fatal("parseTransactionJSON accepted transaction without _id")
	}
}

func TestParseTransactionJSONUsesUpstreamIDAndBrusselsTimestamp(t *testing.T) {
	raw := json.RawMessage(`{
		"_id":"aaaaaaaaaaaaaaaaaaaaaaaa",
		"transactionType":"trading",
		"offerCreatedAt":"must-stay-ordinary-payload-data",
		"createdAt":"2026-03-28T23:30:00Z",
		"updatedAt":"2026-03-28T23:30:00Z",
		"quantity":1,
		"money":2
	}`)

	transaction, _, contribution, err := parseTransactionJSON(raw)
	if err != nil {
		t.Fatalf("parseTransactionJSON returned error: %v", err)
	}
	if transaction.ID != "aaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("transaction ID = %q, want upstream _id", transaction.ID)
	}
	if transaction.CreatedAt != time.Date(
		2026,
		time.March,
		28,
		23,
		30,
		0,
		0,
		time.UTC,
	) {
		t.Fatalf("createdAt = %s, want exact upstream timestamp", transaction.CreatedAt)
	}
	if contribution == nil || contribution.RecordedVolumeBTC != "2" {
		t.Fatalf("contribution = %+v, want recorded money 2", contribution)
	}
}
