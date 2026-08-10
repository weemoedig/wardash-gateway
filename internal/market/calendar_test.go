package market

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBrusselsDayBoundsFollowDST(t *testing.T) {
	tests := []struct {
		name     string
		day      string
		duration time.Duration
	}{
		{
			name:     "spring forward has 23 hours",
			day:      "2026-03-29",
			duration: 23 * time.Hour,
		},
		{
			name:     "fall back has 25 hours",
			day:      "2026-10-25",
			duration: 25 * time.Hour,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			start, end, err := BrusselsDayBounds(test.day)
			if err != nil {
				t.Fatalf("BrusselsDayBounds returned error: %v", err)
			}
			if got := end.Sub(start); got != test.duration {
				t.Fatalf("duration = %s, want %s", got, test.duration)
			}
			if BrusselsDay(start) != test.day {
				t.Fatalf("start maps to %s, want %s", BrusselsDay(start), test.day)
			}
			if got := BrusselsDay(end.Add(-time.Nanosecond)); got != test.day {
				t.Fatalf("last instant maps to %s, want %s", got, test.day)
			}
		})
	}
}

func TestBrusselsDayUsesLocalMidnightInsteadOfUTC(t *testing.T) {
	tests := []struct {
		at   time.Time
		want string
	}{
		{
			at:   time.Date(2026, time.March, 28, 23, 30, 0, 0, time.UTC),
			want: "2026-03-29",
		},
		{
			at:   time.Date(2026, time.March, 29, 22, 30, 0, 0, time.UTC),
			want: "2026-03-30",
		},
		{
			at:   time.Date(2026, time.October, 24, 22, 30, 0, 0, time.UTC),
			want: "2026-10-25",
		},
		{
			at:   time.Date(2026, time.October, 25, 23, 30, 0, 0, time.UTC),
			want: "2026-10-26",
		},
	}

	for _, test := range tests {
		if got := BrusselsDay(test.at); got != test.want {
			t.Fatalf("BrusselsDay(%s) = %s, want %s", test.at, got, test.want)
		}
	}
}

func TestLastReliableBrusselsDayUsesSuccessfulIncrementalWatermark(t *testing.T) {
	now := time.Date(2026, time.July, 30, 22, 30, 0, 0, time.UTC)

	t.Run("missing watermark claims no upper coverage", func(t *testing.T) {
		if day, ok, err := LastReliableBrusselsDay(nil, now); err != nil {
			t.Fatalf("LastReliableBrusselsDay returned error: %v", err)
		} else if ok || day != "" {
			t.Fatalf("result = (%q, %t), want no reliable day", day, ok)
		}
	})

	t.Run("today is available as partial after a current-day sync", func(t *testing.T) {
		coveredThrough := now.Add(-time.Minute)
		day, ok, err := LastReliableBrusselsDay(&coveredThrough, now)
		if err != nil {
			t.Fatalf("LastReliableBrusselsDay returned error: %v", err)
		}
		if !ok || day != "2026-07-31" {
			t.Fatalf("result = (%q, %t), want partial 2026-07-31", day, ok)
		}
	})

	t.Run("stalled prior day excludes that partially covered day", func(t *testing.T) {
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
		day, ok, err := LastReliableBrusselsDay(&coveredThrough, now)
		if err != nil {
			t.Fatalf("LastReliableBrusselsDay returned error: %v", err)
		}
		if !ok || day != "2026-07-29" {
			t.Fatalf("result = (%q, %t), want last complete day 2026-07-29", day, ok)
		}
	})

	t.Run("future watermark is rejected", func(t *testing.T) {
		coveredThrough := now.Add(time.Second)
		if day, ok, err := LastReliableBrusselsDay(
			&coveredThrough,
			now,
		); err != nil {
			t.Fatalf("LastReliableBrusselsDay returned error: %v", err)
		} else if ok || day != "" {
			t.Fatalf("result = (%q, %t), want no reliable day", day, ok)
		}
	})
}

func TestBackfillStateRoundTripIsBackwardCompatibleAndPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", TransactionStateFilename)
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
	incrementalStartedAt := coveredThrough.Add(-time.Hour)
	want := BackfillState{
		Completed:                 true,
		Cursor:                    "must-not-survive-completion",
		UpdatedAt:                 time.Date(2026, time.July, 30, 20, 0, 0, 0, time.UTC),
		AvailableSince:            "2026-07-01",
		CompletionReason:          "retention_cutoff",
		BackfillOldestProcessedAt: &oldestProcessedAt,
		IncrementalCoveredThrough: &coveredThrough,
		IncrementalCursor:         "incremental-cursor",
		IncrementalStartedAt:      &incrementalStartedAt,
		IncrementalReplay:         true,
	}

	if err := SaveBackfillState(path, want); err != nil {
		t.Fatalf("SaveBackfillState returned error: %v", err)
	}
	got, err := LoadBackfillState(path)
	if err != nil {
		t.Fatalf("LoadBackfillState returned error: %v", err)
	}
	if !got.Completed ||
		got.Cursor != "" ||
		got.AvailableSince != want.AvailableSince ||
		got.CompletionReason != want.CompletionReason ||
		got.BackfillOldestProcessedAt == nil ||
		!got.BackfillOldestProcessedAt.Equal(oldestProcessedAt) ||
		got.IncrementalCoveredThrough == nil ||
		!got.IncrementalCoveredThrough.Equal(coveredThrough) ||
		got.IncrementalCursor != want.IncrementalCursor ||
		got.IncrementalStartedAt == nil ||
		!got.IncrementalStartedAt.Equal(incrementalStartedAt) ||
		!got.IncrementalReplay ||
		!got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Fatalf("state = %+v, want completed state with coverage metadata", got)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat state file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("state mode = %o, want 600", got)
	}

	legacyPath := filepath.Join(t.TempDir(), TransactionStateFilename)
	if err := os.WriteFile(
		legacyPath,
		[]byte(`{"completed":false,"cursor":"legacy","updatedAt":"2026-07-30T20:00:00Z"}`),
		0o600,
	); err != nil {
		t.Fatalf("write legacy state: %v", err)
	}
	legacy, err := LoadBackfillState(legacyPath)
	if err != nil {
		t.Fatalf("load legacy state: %v", err)
	}
	if legacy.Cursor != "legacy" ||
		legacy.AvailableSince != "" ||
		legacy.CompletionReason != "" ||
		legacy.BackfillOldestProcessedAt != nil ||
		legacy.IncrementalCoveredThrough != nil ||
		legacy.IncrementalCursor != "" ||
		legacy.IncrementalStartedAt != nil ||
		legacy.IncrementalReplay {
		t.Fatalf("legacy state = %+v, want zero values for new fields", legacy)
	}
}
