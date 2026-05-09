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
	maxBatchStoredBytes  = 4 * 1024 * 1024
	eventStorageFailure  = "logstore.storage_failure"
	eventLogRotation     = "logstore.rotation"
	sqliteDriverName     = "sqlite"
	checkpointMode       = "PASSIVE"
	checkpointModeForced = "TRUNCATE"
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
// StderrBudget are logical per-stream retention budgets measured in stored_size
// bytes. Events may be nil, in which case storage failures are returned without
// also recording runner events.
type Options struct {
	Directory          string
	StdoutBudget       uint64
	StderrBudget       uint64
	Events             EventRecorder
	CheckpointInterval time.Duration
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

	checkpointInterval time.Duration
	lastCheckpoint     time.Time
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
		db:                 db,
		path:               path,
		events:             opts.Events,
		states:             make(map[Source]*streamState),
		checkpointInterval: opts.CheckpointInterval,
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

// Append stores one accepted parsed log entry, assigns the stream-scoped ID,
// enforces retention for that entry's source, checkpoints the WAL after
// retention, wakes blocked readers, and returns the stored entry snapshot.
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
		ID:             state.nextID,
		Timestamp:      parsed.Timestamp,
		Source:         parsed.Source,
		Level:          parsed.Level,
		Message:        parsed.Message,
		Truncated:      parsed.Truncated,
		SourceLocation: cloneSourceLocation(parsed.SourceLocation),
	}
	entry.StoredSize = storedSize(entry)

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
	state.nextID++
	state.retainedBytes += entry.StoredSize
	rotated, err := s.enforceRetention(ctx, tx, parsed.Source, state)
	if err != nil {
		_ = tx.Rollback()
		s.recordStorageFailure("enforce retention", parsed.Source, err)
		return Entry{}, fmt.Errorf("enforce retention: %w", err)
	}
	if err := tx.Commit(); err != nil {
		s.recordStorageFailure("commit append transaction", parsed.Source, err)
		return Entry{}, fmt.Errorf("commit append transaction: %w", err)
	}
	if rotated {
		s.recordRotation(parsed.Source, state)
	}
	if err := s.checkpoint(ctx, checkpointMode); err != nil {
		s.recordStorageFailure("checkpoint WAL", parsed.Source, err)
		return Entry{}, fmt.Errorf("checkpoint WAL: %w", err)
	}
	s.lastCheckpoint = time.Now()
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

// Checkpoint forces a truncating WAL checkpoint. It is exposed primarily for
// lifecycle hooks and tests that need an observable checkpoint operation.
func (s *Store) Checkpoint(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkpoint(ctx, checkpointModeForced); err != nil {
		s.recordStorageFailure("checkpoint WAL", "", err)
		return fmt.Errorf("checkpoint WAL: %w", err)
	}
	s.lastCheckpoint = time.Now()
	return nil
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
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS logs (
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
	) WITHOUT ROWID`); err != nil {
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

func (s *Store) enforceRetention(ctx context.Context, tx *sql.Tx, source Source, state *streamState) (bool, error) {
	rotated := false
	for state.retainedBytes > state.budget && state.beginID+1 < state.nextID {
		var id uint64
		var size uint64
		err := tx.QueryRowContext(ctx, `SELECT id, stored_size FROM logs
			WHERE source = ? ORDER BY id ASC LIMIT 1`, string(source)).Scan(&id, &size)
		if err != nil {
			return rotated, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM logs WHERE source = ? AND id = ?`, string(source), id); err != nil {
			return rotated, err
		}
		if size > state.retainedBytes {
			state.retainedBytes = 0
		} else {
			state.retainedBytes -= size
		}
		state.beginID = id + 1
		rotated = true
	}
	return rotated, nil
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

func (s *Store) readBatchLocked(ctx context.Context, source Source, startID uint64, limit uint64) ([]Entry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, timestamp, level, message, truncated,
		source_function, source_file, source_line, stored_size
		FROM logs WHERE source = ? AND id >= ? ORDER BY id ASC LIMIT ?`,
		string(source), startID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []Entry
	var plannedBytes uint64
	for rows.Next() {
		entry, err := scanEntry(rows, source)
		if err != nil {
			return nil, err
		}
		if len(entries) > 0 && plannedBytes+entry.StoredSize > maxBatchStoredBytes {
			break
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

func (s *Store) checkpoint(ctx context.Context, mode string) error {
	if s.checkpointInterval > 0 && !s.lastCheckpoint.IsZero() && mode == checkpointMode && time.Since(s.lastCheckpoint) < s.checkpointInterval {
		return nil
	}
	_, err := s.db.ExecContext(ctx, "PRAGMA wal_checkpoint("+mode+")")
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
	size += 8 + 8 + 1
	if entry.SourceLocation != nil {
		size += uint64(len(entry.SourceLocation.Function) + len(entry.SourceLocation.File) + 4)
	}
	return size
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
