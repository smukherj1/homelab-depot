package logstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/smukherj/homelab-depot/runner/internal/events"
	"github.com/smukherj/homelab-depot/runner/internal/logparse"
	_ "modernc.org/sqlite"
)

const (
	databaseFileName     = "logs.sqlite"
	maxBatchEntries      = 1024
	rotationThresholdPct = 97
	rotationBatchPct     = 20
	eventStorageFailure  = "logstore.storage_failure"
	eventLogRotation     = "logstore.rotation"
	sqliteDriverName     = "sqlite"
)

// ErrOutOfRange reports that a requested log ID is outside the retained range
// for the selected stream.
var ErrOutOfRange = errors.New("log ID out of retained range")

// EventRecorder records runner events. events.Ring satisfies this interface.
type EventRecorder interface {
	Record(events.Severity, string, string, map[string]string) events.Event
}

// Options configures SQLite-backed child log storage. Directory must be an
// existing writable directory owned by the caller. StdoutBudget and
// StderrBudget are logical per-stream retention budgets measured in derived
// retained bytes. Events may be nil, in which case storage failures are
// returned without also recording runner events.
type Options struct {
	Directory    string
	StdoutBudget uint64
	StderrBudget uint64
	Events       EventRecorder
}

// Source identifies one child output stream retained by the store.
type Source = logparse.Source

const (
	// SourceStdout identifies child stdout.
	SourceStdout = logparse.SourceStdout
	// SourceStderr identifies child stderr.
	SourceStderr = logparse.SourceStderr
)

// SourceLocation stores optional producer source information.
type SourceLocation = logparse.SourceLocation

// Entry is one retained child log entry with its stream-scoped ID assigned by
// the store.
type Entry struct {
	ID             uint64
	Timestamp      time.Time
	Source         Source
	Level          string
	Message        string
	Truncated      bool
	SourceLocation *SourceLocation
	StoredSize     uint64
}

// StreamStatus is the retained half-open ID range for one stream.
type StreamStatus struct {
	BeginID       uint64
	EndID         uint64
	RetainedBytes uint64
}

// Status is a point-in-time snapshot of retained ranges for both streams.
type Status struct {
	Stdout StreamStatus
	Stderr StreamStatus
}

// Store owns one SQLite database used to persist and stream retained child log
// entries. Store methods are safe for concurrent use.
type Store struct {
	mu     sync.Mutex
	cond   *sync.Cond
	db     *sql.DB
	path   string
	events EventRecorder
	states map[Source]*streamState
	closed bool
}

type streamState struct {
	beginID       uint64
	nextID        uint64
	retainedBytes uint64
	budget        uint64
}

// Open removes runner-owned SQLite files from opts.Directory, creates a fresh
// logs.sqlite database, configures WAL mode and synchronous=NORMAL, initializes
// the schema, and returns a ready store. The returned store owns the database
// connection and must be closed by the caller.
func Open(opts Options) (*Store, error) {
	if opts.Directory == "" {
		return nil, errors.New("log directory is required")
	}
	if opts.StdoutBudget == 0 {
		return nil, errors.New("stdout disk budget must be greater than zero")
	}
	if opts.StderrBudget == 0 {
		return nil, errors.New("stderr disk budget must be greater than zero")
	}
	if err := cleanupRunnerSQLiteFiles(opts.Directory); err != nil {
		return nil, err
	}

	path := filepath.Join(opts.Directory, databaseFileName)
	db, err := sql.Open(sqliteDriverName, path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite database %q: %w", path, err)
	}
	store := &Store{
		db:     db,
		path:   path,
		events: opts.Events,
		states: make(map[Source]*streamState),
	}
	store.cond = sync.NewCond(&store.mu)
	store.states[SourceStdout] = &streamState{budget: opts.StdoutBudget}
	store.states[SourceStderr] = &streamState{budget: opts.StderrBudget}

	if err := store.initialize(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// Close closes the owned SQLite connection and wakes blocked readers. Future
// operations return an error.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.cond.Broadcast()
	return s.db.Close()
}

// Path returns the SQLite database file path.
func (s *Store) Path() string {
	return s.path
}

// Status returns retained ranges and retained byte counts for both streams.
func (s *Store) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{
		Stdout: streamStatus(s.states[SourceStdout]),
		Stderr: streamStatus(s.states[SourceStderr]),
	}
}

// EndID returns the next assigned ID for source. Unknown sources return zero.
func (s *Store) EndID(source Source) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.states[source]
	if state == nil {
		return 0
	}
	return state.nextID
}

// Append stores one accepted parsed log entry, assigning the stream-scoped ID.
// If the projected stream usage crosses the retention threshold, Append first
// rotates a batch of old rows in a separate committed transaction. After the
// entry is committed, Append checkpoints the WAL, wakes blocked readers, and
// returns the stored entry snapshot.
func (s *Store) Append(ctx context.Context, parsed logparse.Entry) (Entry, error) {
	if err := validateSource(parsed.Source); err != nil {
		return Entry{}, err
	}
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Entry{}, errors.New("log store is closed")
	}

	state := s.states[parsed.Source]
	entry := Entry{
		Timestamp:      parsed.Timestamp,
		Source:         parsed.Source,
		Level:          parsed.Level,
		Message:        parsed.Message,
		Truncated:      parsed.Truncated,
		SourceLocation: cloneSourceLocation(parsed.SourceLocation),
	}
	entry.StoredSize = storedSize(entry)

	rotated, err := s.rotateBeforeAppend(ctx, parsed.Source, state, entry.StoredSize)
	if err != nil {
		s.recordStorageFailure("rotate before append", parsed.Source, err)
		return Entry{}, fmt.Errorf("rotate before append: %w", err)
	}
	if rotated {
		s.recordRotation(parsed.Source, state)
	}

	entry.ID = state.nextID
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.recordStorageFailure("begin append transaction", parsed.Source, err)
		return Entry{}, fmt.Errorf("begin append transaction: %w", err)
	}
	if err := insertEntry(ctx, tx, entry); err != nil {
		_ = tx.Rollback()
		s.recordStorageFailure("insert log entry", parsed.Source, err)
		return Entry{}, fmt.Errorf("insert log entry: %w", err)
	}
	if err := tx.Commit(); err != nil {
		s.recordStorageFailure("commit append transaction", parsed.Source, err)
		return Entry{}, fmt.Errorf("commit append transaction: %w", err)
	}
	state.nextID++
	state.retainedBytes += entry.StoredSize
	s.cond.Broadcast()
	return cloneEntry(entry), nil
}

// Read returns up to limit retained entries from source starting at startID. A
// limit of zero reads every currently available retained entry from startID.
// The requested start ID must be within [begin_id, end_id].
func (s *Store) Read(ctx context.Context, source Source, startID uint64, limit uint64) ([]Entry, error) {
	if err := validateSource(source); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateReadableLocked(source, startID); err != nil {
		return nil, err
	}
	batchLimit := limit
	if batchLimit == 0 || batchLimit > maxBatchEntries {
		batchLimit = maxBatchEntries
	}
	entries, err := s.readBatchLocked(ctx, source, startID, batchLimit)
	if err != nil {
		s.recordStorageFailure("read log entries", source, err)
		return nil, fmt.Errorf("read log entries: %w", err)
	}
	return entries, nil
}

// Stream blocks until entries are available, then delivers retained entries to
// send in bounded batches. startID must be within [begin_id, end_id].
// maxEntries zero means follow until context cancellation; otherwise the stream
// stops after exactly maxEntries entries or an error.
func (s *Store) Stream(ctx context.Context, source Source, startID uint64, maxEntries uint64, send func([]Entry) error) error {
	if err := validateSource(source); err != nil {
		return err
	}
	if send == nil {
		return errors.New("send callback is required")
	}
	if maxEntries > maxBatchEntries {
		maxEntries = maxBatchEntries
	}
	curID := startID
	var delivered uint64
	for {
		s.mu.Lock()
		// Wait until there are log entries to read.
		for {
			if err := ctx.Err(); err != nil {
				s.mu.Unlock()
				return err
			}
			if s.closed {
				s.mu.Unlock()
				return errors.New("log store is closed")
			}
			if err := s.validateReadableLocked(source, curID); err != nil {
				s.mu.Unlock()
				return err
			}
			state := s.states[source]
			if curID < state.nextID {
				break
			}
			waitDone := make(chan struct{})
			go func() {
				select {
				case <-ctx.Done():
					s.cond.Broadcast()
				case <-waitDone:
				}
			}()
			s.cond.Wait()
			close(waitDone)
		}

		remaining := maxEntries - delivered
		entries, err := s.readBatchLocked(ctx, source, curID, remaining)
		s.mu.Unlock()
		if err != nil {
			s.recordStorageFailure("stream log entries", source, err)
			return fmt.Errorf("stream log entries: %w", err)
		}
		if len(entries) == 0 {
			continue
		}
		if err := send(entries); err != nil {
			return err
		}
		delivered += uint64(len(entries))
		curID = entries[len(entries)-1].ID + 1
		if delivered >= maxEntries {
			return nil
		}
	}
}

func cleanupRunnerSQLiteFiles(directory string) error {
	for _, name := range []string{databaseFileName, databaseFileName + "-wal", databaseFileName + "-shm"} {
		path := filepath.Join(directory, name)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove runner SQLite file %q: %w", path, err)
		}
	}
	return nil
}

func (s *Store) initialize() error {
	if _, err := s.db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return fmt.Errorf("enable WAL journal mode for %q: %w", s.path, err)
	}
	if _, err := s.db.Exec("PRAGMA synchronous=NORMAL"); err != nil {
		return fmt.Errorf("set synchronous=NORMAL for %q: %w", s.path, err)
	}
	if _, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS logs (
	source TEXT NOT NULL,
	id INTEGER NOT NULL,
	timestamp TEXT NOT NULL,
	level TEXT NOT NULL,
	message TEXT NOT NULL,
	truncated INTEGER NOT NULL,
	source_function TEXT,
	source_file TEXT,
	source_line INTEGER,
	PRIMARY KEY (source, id)
) WITHOUT ROWID
`); err != nil {
		return fmt.Errorf("create log schema for %q: %w", s.path, err)
	}
	return nil
}

func insertEntry(ctx context.Context, tx *sql.Tx, entry Entry) error {
	var function, file sql.NullString
	var line sql.NullInt64
	if entry.SourceLocation != nil {
		if entry.SourceLocation.Function != "" {
			function = sql.NullString{String: entry.SourceLocation.Function, Valid: true}
		}
		if entry.SourceLocation.File != "" {
			file = sql.NullString{String: entry.SourceLocation.File, Valid: true}
		}
		if entry.SourceLocation.Line != 0 {
			line = sql.NullInt64{Int64: int64(entry.SourceLocation.Line), Valid: true}
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO logs
		(source, id, timestamp, level, message, truncated, source_function, source_file, source_line)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(entry.Source),
		entry.ID,
		entry.Timestamp.UTC().Format(time.RFC3339Nano),
		entry.Level,
		entry.Message,
		boolToInt(entry.Truncated),
		function,
		file,
		line,
	)
	return err
}

func (s *Store) rotateBeforeAppend(ctx context.Context, source Source, state *streamState, incomingSize uint64) (bool, error) {
	if !projectedUsageExceedsThreshold(state.retainedBytes, incomingSize, state.budget) {
		return false, nil
	}
	retainedCount := state.nextID - state.beginID
	if retainedCount <= 1 {
		return false, nil
	}
	deleteCount := ceilPercent(retainedCount, rotationBatchPct)
	if deleteCount >= retainedCount {
		deleteCount = retainedCount - 1
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin rotation transaction: %w", err)
	}
	expiredEntries, err := s.readTxBatchLocked(ctx, tx, source, state.beginID, deleteCount)
	if err != nil {
		_ = tx.Rollback()
		return false, fmt.Errorf("read rotation batch: %w", err)
	}
	if len(expiredEntries) != int(deleteCount) {
		_ = tx.Rollback()
		return false, fmt.Errorf("inconsistent internal state for source %v: expected %v retained entries starting from id %v but storage returned %v",
			source, deleteCount, state.beginID, len(expiredEntries))
	}
	var expiredBytes uint64
	for i, entry := range expiredEntries {
		wantID := state.beginID + uint64(i)
		if entry.ID != wantID {
			_ = tx.Rollback()
			return false, fmt.Errorf("inconsistent internal state for source %v: got ID %v but wanted %v at rotation position %v",
				source, entry.ID, wantID, i)
		}
		expiredBytes += entry.StoredSize
	}
	lastExpiredID := expiredEntries[len(expiredEntries)-1].ID
	if _, err := tx.ExecContext(ctx, `DELETE FROM logs WHERE source = ? AND id >= ? AND id <= ?`, string(source), state.beginID, lastExpiredID); err != nil {
		_ = tx.Rollback()
		return false, fmt.Errorf("delete rotation batch: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit rotation transaction: %w", err)
	}
	state.beginID = lastExpiredID + 1
	if expiredBytes > state.retainedBytes {
		state.retainedBytes = 0
	} else {
		state.retainedBytes -= expiredBytes
	}
	if err := s.checkpoint(ctx); err != nil {
		s.recordStorageFailure("checkpoint WAL", source, err)
	}
	return true, nil
}

func (s *Store) validateReadableLocked(source Source, startID uint64) error {
	state := s.states[source]
	if state == nil {
		return fmt.Errorf("unknown source %q", source)
	}
	if startID < state.beginID || startID > state.nextID {
		return fmt.Errorf("%w: source=%s start_id=%d retained=[%d,%d]", ErrOutOfRange, source, startID, state.beginID, state.nextID)
	}
	return nil
}

func (s *Store) readTxBatchLocked(ctx context.Context, tx *sql.Tx, source Source, startID, limit uint64) ([]Entry, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, timestamp, level, message, truncated,
		source_function, source_file, source_line
		FROM logs WHERE source = ? AND id >= ? ORDER BY id ASC LIMIT ?`,
		string(source), startID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanEntries(rows, source)
}

func (s *Store) readBatchLocked(ctx context.Context, source Source, startID uint64, limit uint64) ([]Entry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, timestamp, level, message, truncated,
		source_function, source_file, source_line
		FROM logs WHERE source = ? AND id >= ? ORDER BY id ASC LIMIT ?`,
		string(source), startID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return s.scanEntries(rows, source)
}

func (s *Store) scanEntries(rows *sql.Rows, source Source) ([]Entry, error) {
	var entries []Entry
	var plannedBytes uint64
	for rows.Next() {
		entry, err := scanEntry(rows, source)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
		plannedBytes += entry.StoredSize
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func scanEntry(rows *sql.Rows, source Source) (Entry, error) {
	var entry Entry
	var timestamp string
	var truncated int
	var function, file sql.NullString
	var line sql.NullInt64
	if err := rows.Scan(&entry.ID, &timestamp, &entry.Level, &entry.Message, &truncated, &function, &file, &line); err != nil {
		return Entry{}, err
	}
	parsedTime, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return Entry{}, err
	}
	entry.Timestamp = parsedTime
	entry.Source = source
	entry.Truncated = truncated != 0
	if function.Valid || file.Valid || line.Valid {
		entry.SourceLocation = &SourceLocation{}
		if function.Valid {
			entry.SourceLocation.Function = function.String
		}
		if file.Valid {
			entry.SourceLocation.File = file.String
		}
		if line.Valid {
			entry.SourceLocation.Line = uint32(line.Int64)
		}
	}
	entry.StoredSize = storedSize(entry)
	return entry, nil
}

func (s *Store) checkpoint(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}

func (s *Store) recordStorageFailure(operation string, source Source, err error) {
	if s.events == nil {
		return
	}
	details := map[string]string{
		"operation": operation,
		"database":  s.path,
		"reason":    err.Error(),
	}
	if source != "" {
		details["source"] = string(source)
	}
	s.events.Record(events.SeverityError, eventStorageFailure, "log storage operation failed", details)
}

func (s *Store) recordRotation(source Source, state *streamState) {
	if s.events == nil {
		return
	}
	s.events.Record(events.SeverityInfo, eventLogRotation, "child log retention rotated", map[string]string{
		"source":   string(source),
		"begin_id": strconv.FormatUint(state.beginID, 10),
		"end_id":   strconv.FormatUint(state.nextID, 10),
	})
}

func validateSource(source Source) error {
	if source != SourceStdout && source != SourceStderr {
		return fmt.Errorf("invalid log source %q", source)
	}
	return nil
}

func storedSize(entry Entry) uint64 {
	size := uint64(len(entry.Level) + len(entry.Message) + len(entry.Source))
	// ID, Timestamp, Truncated
	size += 8 + 8 + 8
	if entry.SourceLocation != nil {
		size += uint64(len(entry.SourceLocation.Function) + len(entry.SourceLocation.File))
		// SourceLocation.Line
		size += uint64(8)
	}
	return size
}

func rotationThreshold(budget uint64) uint64 {
	return percentFloor(budget, rotationThresholdPct)
}

func ceilPercent(value uint64, percent uint64) uint64 {
	if value == 0 || percent == 0 {
		return 0
	}
	return value/100*percent + ((value%100)*percent+99)/100
}

func projectedUsageExceedsThreshold(retainedBytes uint64, incomingSize uint64, budget uint64) bool {
	threshold := rotationThreshold(budget)
	if retainedBytes > threshold {
		return true
	}
	return incomingSize > threshold-retainedBytes
}

func percentFloor(value uint64, percent uint64) uint64 {
	return value/100*percent + (value%100)*percent/100
}

func streamStatus(state *streamState) StreamStatus {
	return StreamStatus{
		BeginID:       state.beginID,
		EndID:         state.nextID,
		RetainedBytes: state.retainedBytes,
	}
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func cloneEntry(entry Entry) Entry {
	entry.SourceLocation = cloneSourceLocation(entry.SourceLocation)
	return entry
}

func cloneSourceLocation(location *SourceLocation) *SourceLocation {
	if location == nil {
		return nil
	}
	clone := *location
	return &clone
}
