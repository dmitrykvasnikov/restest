package core

import (
	"testing"
	"time"
)

func TestPartitionNaming(t *testing.T) {
	tests := []struct {
		month time.Time
		want  string
	}{
		{time.Date(2026, 8, 5, 13, 45, 0, 0, time.UTC), "exchanges_2026_08"},
		{time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC), "exchanges_2026_12"},
		{time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), "exchanges_2027_01"},
	}

	for _, tt := range tests {
		if got := partitionName(monthStart(tt.month)); got != tt.want {
			t.Errorf("partitionName(%s) = %q, want %q", tt.month, got, tt.want)
		}
	}
}

// Retention reads the month back out of the table name, so the two have to be
// exact inverses. A name that does not round-trip would be a partition nothing
// ever drops.
func TestPartitionMonthIsTheInverseOfPartitionName(t *testing.T) {
	for year := 2025; year <= 2027; year++ {
		for m := 1; m <= 12; m++ {
			month := time.Date(year, time.Month(m), 1, 0, 0, 0, 0, time.UTC)

			got, ok := partitionMonth(partitionName(month))
			if !ok {
				t.Fatalf("partitionMonth(%q) refused a name partitionName produced", partitionName(month))
			}
			if !got.Equal(month) {
				t.Errorf("round trip of %s came back as %s", month, got)
			}
		}
	}
}

// Anything that is not one of ours is left alone — the default partition above
// all, which is the safety net that must never be dropped.
func TestPartitionMonthRefusesWhatIsNotOurs(t *testing.T) {
	for _, name := range []string{
		"exchanges_default",
		"exchanges",
		"exchanges_2026",
		"exchanges_2026_13",
		"exchanges_2026_00",
		"exchanges_26_08",
		"exchanges_2026_8",
		"exchanges_two_thousand",
		"documents_2026_08",
	} {
		if month, ok := partitionMonth(name); ok {
			t.Errorf("partitionMonth(%q) = %s, want it to be left alone", name, month)
		}
	}
}

// The month a request lands in is decided in UTC, wherever the process thinks
// it is: the partition bounds are absolute timestamps, and a second instance in
// another timezone must not disagree about where a month begins.
func TestMonthStartIsUTC(t *testing.T) {
	// A quarter past midnight on the first of the month in Sydney is still the
	// last day of the previous month in UTC.
	sydney, err := time.LoadLocation("Australia/Sydney")
	if err != nil {
		t.Skipf("no timezone database: %v", err)
	}

	local := time.Date(2026, 9, 1, 0, 15, 0, 0, sydney)
	want := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	if got := monthStart(local); !got.Equal(want) {
		t.Errorf("monthStart(%s) = %s, want %s", local, got, want)
	}
}

// The retention window counts the current month, so one month keeps only the
// month being written into and never detaches it.
func TestRetentionCutoff(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		keep int
		want time.Time
	}{
		{1, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)},
		{3, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)},
		{12, time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC)},
	}

	for _, tt := range tests {
		got := monthStart(now).AddDate(0, -(tt.keep - 1), 0)
		if !got.Equal(tt.want) {
			t.Errorf("keeping %d months from %s: cutoff = %s, want %s", tt.keep, now, got, tt.want)
		}
	}
}

func TestQuoteIdentifier(t *testing.T) {
	if got := quoteIdentifier("exchanges_2026_08"); got != `"exchanges_2026_08"` {
		t.Errorf("quoteIdentifier = %s", got)
	}
	// Nothing user-supplied reaches it, but a name that could end the quoting
	// should not be able to.
	if got := quoteIdentifier(`odd"name`); got != `"odd""name"` {
		t.Errorf("quoteIdentifier = %s, want the embedded quote doubled", got)
	}
}
