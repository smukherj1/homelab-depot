package events

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"
)

// ErrInvalidQuery reports an event query mode that cannot be served.
var ErrInvalidQuery = errors.New("invalid event query")

// Severity identifies the importance or class of a runner event.
type Severity string

const (
	// SeverityInfo identifies ordinary lifecycle events.
	SeverityInfo Severity = "INFO"
	// SeverityWarn identifies recoverable unexpected conditions.
	SeverityWarn Severity = "WARN"
	// SeverityError identifies failed operations or rejected input.
	SeverityError Severity = "ERROR"
)

// Event is one retained runner lifecycle, process, parsing, storage, or API
// event. Details values are strings so they can be passed directly through the
// v1 gRPC API.
type Event struct {
	ID        uint64
	Timestamp time.Time
	Severity  Severity
	Code      string
	Message   string
	Details   map[string]string
}

// QueryMode identifies which retained event window should be returned.
type QueryMode int

const (
	// QueryAll returns every currently retained event.
	QueryAll QueryMode = iota
	// QueryFromID returns retained events with IDs greater than or equal to a
	// supplied event ID.
	QueryFromID
	// QueryLast returns up to the newest supplied count of retained events.
	QueryLast
)

// Query describes a retained event query. FromID is used only with
// QueryFromID. LastCount is used only with QueryLast.
type Query struct {
	Mode      QueryMode
	FromID    uint64
	LastCount uint64
}

// Snapshot contains retained events returned by a query plus the next event ID
// to use for incremental polling.
type Snapshot struct {
	Events []Event
	NextID uint64
}

// Clock returns the current time for new events.
type Clock func() time.Time

// Ring is a concurrency-safe fixed-capacity in-memory event ring. It owns no
// external resources. The ring assigns IDs beginning at zero and never
// renumbers retained events after overflow.
type Ring struct {
	mu            sync.RWMutex
	events        []Event
	capacity      uint64
	nextID        uint64
	maxEntryBytes uint64
	clock         Clock
}

// NewRing constructs an event ring with fixed retention capacity and a maximum
// retained byte length for event messages and detail values. The clock is used
// when Record is called; if nil, time.Now is used. Capacity must be greater
// than zero.
func NewRing(capacity uint64, maxEntryBytes uint64, clock Clock) (*Ring, error) {
	if capacity == 0 {
		return nil, errors.New("event ring capacity must be greater than zero")
	}
	if maxEntryBytes == 0 {
		return nil, errors.New("event max entry bytes must be greater than zero")
	}
	if clock == nil {
		clock = time.Now
	}
	return &Ring{
		events:        make([]Event, 0, capacity),
		capacity:      capacity,
		maxEntryBytes: maxEntryBytes,
		clock:         clock,
	}, nil
}

// Record appends one runner event, assigning its ID and timestamp. Message and
// detail values are truncated to the ring's configured byte limit before
// retention. The returned Event is a snapshot copy and can be mutated by the
// caller without changing the ring.
func (r *Ring) Record(severity Severity, code string, message string, details map[string]string) Event {
	r.mu.Lock()
	defer r.mu.Unlock()

	event := Event{
		ID:        r.nextID,
		Timestamp: r.clock(),
		Severity:  severity,
		Code:      code,
		Message:   truncateUTF8(message, r.maxEntryBytes),
		Details:   truncateDetails(details, r.maxEntryBytes),
	}
	r.nextID++
	if uint64(len(r.events)) == r.capacity {
		copy(r.events, r.events[1:])
		r.events[len(r.events)-1] = event
	} else {
		r.events = append(r.events, event)
	}
	return cloneEvent(event)
}

// Query returns retained events matching query and the next event ID. The
// context is checked before the query starts so server adapters can honor
// canceled RPCs. Returned events are copies and can be mutated by callers.
func (r *Ring) Query(ctx context.Context, query Query) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	start, end, err := r.queryBounds(query)
	if err != nil {
		return Snapshot{}, err
	}
	events := make([]Event, 0, end-start)
	for _, event := range r.events[start:end] {
		events = append(events, cloneEvent(event))
	}
	return Snapshot{Events: events, NextID: r.nextID}, nil
}

func (r *Ring) queryBounds(query Query) (int, int, error) {
	switch query.Mode {
	case QueryAll:
		return 0, len(r.events), nil
	case QueryLast:
		if query.LastCount == 0 {
			return len(r.events), len(r.events), nil
		}
		if query.LastCount >= uint64(len(r.events)) {
			return 0, len(r.events), nil
		}
		return len(r.events) - int(query.LastCount), len(r.events), nil
	case QueryFromID:
		if len(r.events) == 0 || query.FromID >= r.nextID {
			return len(r.events), len(r.events), nil
		}
		for i, event := range r.events {
			if event.ID >= query.FromID {
				return i, len(r.events), nil
			}
		}
		return len(r.events), len(r.events), nil
	default:
		return 0, 0, fmt.Errorf("%w: unknown query mode %d", ErrInvalidQuery, query.Mode)
	}
}

func cloneEvent(event Event) Event {
	event.Details = cloneDetails(event.Details)
	return event
}

func cloneDetails(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func truncateDetails(in map[string]string, limit uint64) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = truncateUTF8(value, limit)
	}
	return out
}

func truncateUTF8(value string, limit uint64) string {
	if uint64(len(value)) <= limit {
		return value
	}
	cut := int(limit)
	for cut > 0 && !utf8.ValidString(value[:cut]) {
		cut--
	}
	return value[:cut]
}
