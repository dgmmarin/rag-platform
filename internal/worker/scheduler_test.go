package worker

import (
	"testing"
	"time"
)

// fullSync fires on run 0 and every Nth run thereafter (SPEC-08 §2).
func TestFullSyncEveryNthRun(t *testing.T) {
	full := map[int]bool{}
	for i := 0; i < 15; i++ {
		full[i] = fullSync(i)
	}
	for _, want := range []int{0, 7, 14} {
		if !full[want] {
			t.Fatalf("run %d should be a full sync", want)
		}
	}
	for _, incr := range []int{1, 2, 6, 8, 13} {
		if full[incr] {
			t.Fatalf("run %d should be incremental, not full", incr)
		}
	}
}

// nextRun computes the next fire time from a standard 5-field cron and rejects a bad
// expression.
func TestNextRun(t *testing.T) {
	base := time.Date(2026, 9, 7, 10, 15, 0, 0, time.UTC)

	// Top of every hour: next fire is 11:00.
	got, err := nextRun("0 * * * *", base)
	if err != nil {
		t.Fatalf("nextRun hourly: %v", err)
	}
	if want := time.Date(2026, 9, 7, 11, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("hourly next = %v, want %v", got, want)
	}

	// Daily at 02:30: next fire is the following day at 02:30.
	got, err = nextRun("30 2 * * *", base)
	if err != nil {
		t.Fatalf("nextRun daily: %v", err)
	}
	if want := time.Date(2026, 9, 8, 2, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("daily next = %v, want %v", got, want)
	}

	if _, err := nextRun("not a cron", base); err == nil {
		t.Fatal("want an error for an unparseable cron expression")
	}
}
