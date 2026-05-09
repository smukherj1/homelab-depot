package logstore

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/smukherj/homelab-depot/runner/internal/events"
	"github.com/smukherj/homelab-depot/runner/internal/logparse"
	_ "modernc.org/sqlite"
)

func TestOpenCleansOnlyRunnerSQLiteFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"logs.sqlite", "logs.sqlite-wal", "logs.sqlite-shm", "keep.sqlite", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("old"), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v, want nil", name, err)
		}
	}

	store := openTestStore(t, dir, 1024, 1024, nil)
	defer closeStore(t, store)

	for _, name := range []string{"keep.sqlite", "notes.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("Stat(%s) error = %v, want preserved file", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "logs.sqlite")); err != nil {
		t.Errorf("Stat(logs.sqlite) error = %v, want newly created database", err)
	}
}

func TestOpenCreatesSchemaAndConfiguresSQLite(t *testing.T) {
	store := openTestStore(t, t.TempDir(), 1024, 1024, nil)
	defer closeStore(t, store)

	db, err := sql.Open(sqliteDriverName, store.Path())
	if err != nil {
		t.Fatalf("sql.Open() error = %v, want nil", err)
	}
	defer db.Close()

	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("PRAGMA journal_mode scan error = %v, want nil", err)
	}
	if journalMode != "wal" {
		t.Errorf("journal_mode = %q, want wal", journalMode)
	}

	var synchronous int
	if err := store.db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil {
		t.Fatalf("PRAGMA synchronous scan error = %v, want nil", err)
	}
	if synchronous != 1 {
		t.Errorf("synchronous = %d, want 1 for NORMAL", synchronous)
	}

	if err := store.initialize(); err != nil {
		t.Errorf("initialize() second call error = %v, want nil", err)
	}
}

func TestAppendAndReadRoundTripAllFields(t *testing.T) {
	store := openTestStore(t, t.TempDir(), 4096, 4096, nil)
	defer closeStore(t, store)

	timestamp := time.Date(2026, 5, 9, 12, 30, 45, 123, time.UTC)
	inserted, err := store.Append(context.Background(), logparse.Entry{
		Timestamp: timestamp,
		Source:    SourceStderr,
		Level:     "WARN",
		Message:   "slow path\n",
		Truncated: true,
		SourceLocation: &SourceLocation{
			Function: "main.run",
			File:     "main.go",
			Line:     42,
		},
	})
	if err != nil {
		t.Fatalf("Append() error = %v, want nil", err)
	}
	if inserted.ID != 0 {
		t.Fatalf("inserted.ID = %d, want first stderr ID 0", inserted.ID)
	}

	got, err := store.Read(context.Background(), SourceStderr, 0, 10)
	if err != nil {
		t.Fatalf("Read() error = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(Read()) = %d, want 1", len(got))
	}
	entry := got[0]
	if entry.ID != 0 || entry.Source != SourceStderr || entry.Level != "WARN" || entry.Message != "slow path\n" || !entry.Truncated {
		t.Errorf("entry = %+v, want stored fields preserved", entry)
	}
	if !entry.Timestamp.Equal(timestamp) {
		t.Errorf("entry.Timestamp = %v, want %v", entry.Timestamp, timestamp)
	}
	if entry.SourceLocation == nil || entry.SourceLocation.File != "main.go" || entry.SourceLocation.Line != 42 {
		t.Errorf("entry.SourceLocation = %+v, want main.go:42", entry.SourceLocation)
	}
	if entry.StoredSize == 0 {
		t.Errorf("entry.StoredSize = 0, want positive size")
	}
}

func TestAppendAdvancesEndIDAndRetentionAdvancesBeginID(t *testing.T) {
	store := openTestStore(t, t.TempDir(), 55, 4096, nil)
	defer closeStore(t, store)

	appendMessage(t, store, SourceStdout, "first line\n")
	appendMessage(t, store, SourceStdout, "second line\n")
	appendMessage(t, store, SourceStdout, "third line\n")

	status := store.Status()
	if status.Stdout.EndID != 3 {
		t.Fatalf("stdout.EndID = %d, want 3", status.Stdout.EndID)
	}
	if status.Stdout.BeginID == 0 {
		t.Fatalf("stdout.BeginID = 0, want retention to advance it")
	}
	if status.Stdout.BeginID >= status.Stdout.EndID {
		t.Fatalf("stdout range = [%d,%d), want at least newest row retained", status.Stdout.BeginID, status.Stdout.EndID)
	}
	if _, err := store.Read(context.Background(), SourceStdout, 0, 1); !errors.Is(err, ErrOutOfRange) {
		t.Fatalf("Read(old ID) error = %v, want ErrOutOfRange", err)
	}
}

func TestStreamsRetainIndependentIDsAndBudgets(t *testing.T) {
	store := openTestStore(t, t.TempDir(), 55, 4096, nil)
	defer closeStore(t, store)

	appendMessage(t, store, SourceStdout, "first stdout\n")
	appendMessage(t, store, SourceStdout, "second stdout\n")
	appendMessage(t, store, SourceStdout, "third stdout\n")
	appendMessage(t, store, SourceStderr, "first stderr\n")

	status := store.Status()
	if status.Stderr.BeginID != 0 || status.Stderr.EndID != 1 {
		t.Errorf("stderr range = [%d,%d), want untouched [0,1)", status.Stderr.BeginID, status.Stderr.EndID)
	}
	if status.Stdout.BeginID == 0 {
		t.Errorf("stdout.BeginID = 0, want stdout retention to rotate independently")
	}

	stderrRows, err := store.Read(context.Background(), SourceStderr, 0, 10)
	if err != nil {
		t.Fatalf("Read(stderr) error = %v, want nil", err)
	}
	if len(stderrRows) != 1 || stderrRows[0].ID != 0 {
		t.Errorf("stderr rows = %+v, want one row with independent ID 0", stderrRows)
	}
}

func TestOutOfRangeAboveEndID(t *testing.T) {
	store := openTestStore(t, t.TempDir(), 4096, 4096, nil)
	defer closeStore(t, store)

	appendMessage(t, store, SourceStdout, "only\n")
	if _, err := store.Read(context.Background(), SourceStdout, 2, 10); !errors.Is(err, ErrOutOfRange) {
		t.Fatalf("Read(start above end) error = %v, want ErrOutOfRange", err)
	}
}

func TestRetentionLeavesNewestRowEvenWhenOverBudget(t *testing.T) {
	store := openTestStore(t, t.TempDir(), 1, 4096, nil)
	defer closeStore(t, store)

	appendMessage(t, store, SourceStdout, "oversized logical row\n")
	status := store.Status()
	if status.Stdout.BeginID != 0 || status.Stdout.EndID != 1 {
		t.Fatalf("stdout range = [%d,%d), want newest row retained as [0,1)", status.Stdout.BeginID, status.Stdout.EndID)
	}
	rows, err := store.Read(context.Background(), SourceStdout, 0, 10)
	if err != nil {
		t.Fatalf("Read(newest over budget) error = %v, want nil", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
}

func TestCheckpointAndRotationEvents(t *testing.T) {
	ring, err := events.NewRing(10, 1024, func() time.Time { return time.Unix(100, 0).UTC() })
	if err != nil {
		t.Fatalf("NewRing() error = %v, want nil", err)
	}
	store := openTestStore(t, t.TempDir(), 55, 4096, ring)
	defer closeStore(t, store)

	appendMessage(t, store, SourceStdout, "first stdout\n")
	appendMessage(t, store, SourceStdout, "second stdout\n")
	appendMessage(t, store, SourceStdout, "third stdout\n")
	if err := store.Checkpoint(context.Background()); err != nil {
		t.Fatalf("Checkpoint() error = %v, want nil", err)
	}

	snapshot, err := ring.Query(context.Background(), events.Query{Mode: events.QueryAll})
	if err != nil {
		t.Fatalf("ring.Query() error = %v, want nil", err)
	}
	foundRotation := false
	for _, event := range snapshot.Events {
		if event.Code == eventLogRotation {
			foundRotation = true
		}
	}
	if !foundRotation {
		t.Errorf("events = %+v, want a log rotation event", snapshot.Events)
	}
}

func TestStreamBlocksAndWakesOnAppend(t *testing.T) {
	store := openTestStore(t, t.TempDir(), 4096, 4096, nil)
	defer closeStore(t, store)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	received := make(chan []Entry, 1)
	errs := make(chan error, 1)
	go func() {
		errs <- store.Stream(ctx, SourceStdout, 0, 1, func(entries []Entry) error {
			received <- entries
			return nil
		})
	}()

	appendMessage(t, store, SourceStdout, "wake\n")

	select {
	case entries := <-received:
		if len(entries) != 1 || entries[0].Message != "wake\n" {
			t.Fatalf("stream entries = %+v, want wake entry", entries)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for blocking stream to wake")
	}
	if err := <-errs; err != nil {
		t.Fatalf("Stream() error = %v, want nil", err)
	}
}

func TestStreamFailsWhenRetentionRemovesNeededID(t *testing.T) {
	store := openTestStore(t, t.TempDir(), 55, 4096, nil)
	defer closeStore(t, store)

	appendMessage(t, store, SourceStdout, "first stdout\n")
	appendMessage(t, store, SourceStdout, "second stdout\n")
	appendMessage(t, store, SourceStdout, "third stdout\n")

	err := store.Stream(context.Background(), SourceStdout, 0, 1, func([]Entry) error {
		return nil
	})
	if !errors.Is(err, ErrOutOfRange) {
		t.Fatalf("Stream(old ID) error = %v, want ErrOutOfRange", err)
	}
}

func TestReadBatchesRespectEntryLimit(t *testing.T) {
	store := openTestStore(t, t.TempDir(), 1<<20, 4096, nil)
	defer closeStore(t, store)

	for i := 0; i < maxBatchEntries+2; i++ {
		appendMessage(t, store, SourceStdout, "line\n")
	}
	rows, err := store.Read(context.Background(), SourceStdout, 0, 0)
	if err != nil {
		t.Fatalf("Read(limit 0) error = %v, want nil", err)
	}
	if len(rows) != maxBatchEntries {
		t.Fatalf("len(rows) = %d, want batch cap %d", len(rows), maxBatchEntries)
	}
}

func openTestStore(t *testing.T, dir string, stdoutBudget uint64, stderrBudget uint64, recorder EventRecorder) *Store {
	t.Helper()
	store, err := Open(Options{
		Directory:    dir,
		StdoutBudget: stdoutBudget,
		StderrBudget: stderrBudget,
		Events:       recorder,
	})
	if err != nil {
		t.Fatalf("Open() error = %v, want nil", err)
	}
	return store
}

func closeStore(t *testing.T, store *Store) {
	t.Helper()
	if err := store.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil", err)
	}
}

func appendMessage(t *testing.T, store *Store, source Source, message string) Entry {
	t.Helper()
	level := "INFO"
	if source == SourceStderr {
		level = "ERROR"
	}
	entry, err := store.Append(context.Background(), logparse.Entry{
		Timestamp: time.Unix(100, 0).UTC(),
		Source:    source,
		Level:     level,
		Message:   message,
	})
	if err != nil {
		t.Fatalf("Append(%s, %q) error = %v, want nil", source, message, err)
	}
	return entry
}
