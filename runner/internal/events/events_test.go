package events

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestRecordAssignsSequentialIDsStartingAtZero(t *testing.T) {
	ring := newTestRing(t, 4, 1024)

	first := ring.Record(SeverityInfo, "first", "first event", nil)
	second := ring.Record(SeverityWarn, "second", "second event", nil)

	if first.ID != 0 {
		t.Errorf("first.ID = %d, want 0", first.ID)
	}
	if second.ID != 1 {
		t.Errorf("second.ID = %d, want 1", second.ID)
	}
}

func TestRingOverflowDropsOldestAndKeepsNextID(t *testing.T) {
	ring := newTestRing(t, 3, 1024)
	for i := 0; i < 5; i++ {
		ring.Record(SeverityInfo, "event", "event", nil)
	}

	got := queryEvents(t, ring, Query{Mode: QueryAll})

	if got.NextID != 5 {
		t.Errorf("NextID = %d, want 5", got.NextID)
	}
	assertEventIDs(t, got.Events, []uint64{2, 3, 4})
}

func TestQueryLastCountZeroReturnsNoEvents(t *testing.T) {
	ring := newTestRing(t, 3, 1024)
	ring.Record(SeverityInfo, "event", "event", nil)

	got := queryEvents(t, ring, Query{Mode: QueryLast, LastCount: 0})

	if len(got.Events) != 0 {
		t.Errorf("len(events) = %d, want 0", len(got.Events))
	}
	if got.NextID != 1 {
		t.Errorf("NextID = %d, want 1", got.NextID)
	}
}

func TestQueryLastCountAboveCapacityReturnsAllRetainedEvents(t *testing.T) {
	ring := newTestRing(t, 3, 1024)
	for i := 0; i < 5; i++ {
		ring.Record(SeverityInfo, "event", "event", nil)
	}

	got := queryEvents(t, ring, Query{Mode: QueryLast, LastCount: 99})

	assertEventIDs(t, got.Events, []uint64{2, 3, 4})
}

func TestQueryFromIDBeforeRetainedRangeStartsAtOldestRetained(t *testing.T) {
	ring := newTestRing(t, 3, 1024)
	for i := 0; i < 5; i++ {
		ring.Record(SeverityInfo, "event", "event", nil)
	}

	got := queryEvents(t, ring, Query{Mode: QueryFromID, FromID: 1})

	assertEventIDs(t, got.Events, []uint64{2, 3, 4})
}

func TestQueryFromIDAfterNextIDReturnsEmptyEvents(t *testing.T) {
	ring := newTestRing(t, 3, 1024)
	for i := 0; i < 2; i++ {
		ring.Record(SeverityInfo, "event", "event", nil)
	}

	got := queryEvents(t, ring, Query{Mode: QueryFromID, FromID: 3})

	if len(got.Events) != 0 {
		t.Errorf("len(events) = %d, want 0", len(got.Events))
	}
	if got.NextID != 2 {
		t.Errorf("NextID = %d, want 2", got.NextID)
	}
}

func TestQueryRejectsUnknownMode(t *testing.T) {
	ring := newTestRing(t, 3, 1024)

	_, err := ring.Query(context.Background(), Query{Mode: QueryMode(99)})

	if !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("Query() error = %v, want ErrInvalidQuery", err)
	}
}

func TestRecordTruncatesMessageAndDetailsToByteLimit(t *testing.T) {
	ring := newTestRing(t, 3, 5)

	event := ring.Record(SeverityError, "invalid_line", "abcdef", map[string]string{
		"raw_line": "123456",
		"reason":   "bad",
	})

	if event.Message != "abcde" {
		t.Errorf("Message = %q, want %q", event.Message, "abcde")
	}
	if event.Details["raw_line"] != "12345" {
		t.Errorf("raw_line detail = %q, want %q", event.Details["raw_line"], "12345")
	}
	if event.Details["reason"] != "bad" {
		t.Errorf("reason detail = %q, want %q", event.Details["reason"], "bad")
	}
}

func TestRecordTruncatesAtUTF8Boundary(t *testing.T) {
	ring := newTestRing(t, 3, 4)

	event := ring.Record(SeverityInfo, "unicode", "abcé", nil)

	if event.Message != "abc" {
		t.Errorf("Message = %q, want %q without a partial UTF-8 rune", event.Message, "abc")
	}
}

func TestReturnedEventsAreCopies(t *testing.T) {
	ring := newTestRing(t, 3, 1024)
	event := ring.Record(SeverityInfo, "event", "event", map[string]string{"key": "value"})
	event.Details["key"] = "changed"

	firstQuery := queryEvents(t, ring, Query{Mode: QueryAll})
	firstQuery.Events[0].Details["key"] = "changed again"
	secondQuery := queryEvents(t, ring, Query{Mode: QueryAll})

	if secondQuery.Events[0].Details["key"] != "value" {
		t.Errorf("stored detail = %q, want original value", secondQuery.Events[0].Details["key"])
	}
}

func TestConcurrentWritersAndReaders(t *testing.T) {
	ring := newTestRing(t, 128, 1024)
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				ring.Record(SeverityInfo, "event", "event", nil)
			}
		}()
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, err := ring.Query(context.Background(), Query{Mode: QueryLast, LastCount: 10})
				if err != nil {
					t.Errorf("Query() error = %v, want nil", err)
				}
			}
		}()
	}
	wg.Wait()

	got := queryEvents(t, ring, Query{Mode: QueryAll})
	if got.NextID != 800 {
		t.Errorf("NextID = %d, want 800 after concurrent writes", got.NextID)
	}
	if len(got.Events) != 128 {
		t.Errorf("len(events) = %d, want retained capacity 128", len(got.Events))
	}
}

func newTestRing(t *testing.T, capacity uint64, maxEntryBytes uint64) *Ring {
	t.Helper()
	clock := func() time.Time {
		return time.Unix(100, 0).UTC()
	}
	ring, err := NewRing(capacity, maxEntryBytes, clock)
	if err != nil {
		t.Fatalf("NewRing() error = %v, want nil", err)
	}
	return ring
}

func queryEvents(t *testing.T, ring *Ring, query Query) Snapshot {
	t.Helper()
	got, err := ring.Query(context.Background(), query)
	if err != nil {
		t.Fatalf("Query() error = %v, want nil", err)
	}
	return got
}

func assertEventIDs(t *testing.T, events []Event, want []uint64) {
	t.Helper()
	if len(events) != len(want) {
		t.Fatalf("len(events) = %d, want %d", len(events), len(want))
	}
	for i, event := range events {
		if event.ID != want[i] {
			t.Errorf("events[%d].ID = %d, want %d", i, event.ID, want[i])
		}
	}
}
