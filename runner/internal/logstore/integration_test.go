package logstore

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestIntegrationRetentionAndHistoricalReads(t *testing.T) {
	store := openTestStore(t, t.TempDir(), 55, 4096, nil)
	defer closeStore(t, store)

	appendMessage(t, store, SourceStdout, "first stdout\n")
	appendMessage(t, store, SourceStdout, "second stdout\n")
	appendMessage(t, store, SourceStdout, "third stdout\n")
	appendMessage(t, store, SourceStderr, "stderr stays\n")

	status := store.Status()
	if status.Stdout.BeginID == 0 || status.Stdout.EndID != 3 {
		t.Fatalf("stdout range = [%d,%d), want rotated range ending at 3", status.Stdout.BeginID, status.Stdout.EndID)
	}
	if _, err := store.Read(context.Background(), SourceStdout, 0, 1); !errors.Is(err, ErrOutOfRange) {
		t.Fatalf("Read(rotated stdout ID) error = %v, want ErrOutOfRange", err)
	}
	retained, err := store.Read(context.Background(), SourceStdout, status.Stdout.BeginID, 10)
	if err != nil {
		t.Fatalf("Read(retained stdout) error = %v, want nil", err)
	}
	if len(retained) == 0 || retained[0].ID != status.Stdout.BeginID {
		t.Fatalf("retained rows = %+v, want rows beginning at %d", retained, status.Stdout.BeginID)
	}

	stderr, err := store.Read(context.Background(), SourceStderr, 0, 10)
	if err != nil {
		t.Fatalf("Read(stderr) error = %v, want nil", err)
	}
	if len(stderr) != 1 || stderr[0].Message != "stderr stays\n" {
		t.Fatalf("stderr rows = %+v, want stderr unaffected by stdout retention", stderr)
	}
}

func TestIntegrationBlockingReaderWakesOnAppend(t *testing.T) {
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

	appendMessage(t, store, SourceStdout, "live line\n")

	select {
	case entries := <-received:
		if len(entries) != 1 || entries[0].Message != "live line\n" {
			t.Fatalf("stream entries = %+v, want live line", entries)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for live reader")
	}
	if err := <-errs; err != nil {
		t.Fatalf("Stream() error = %v, want nil", err)
	}
}

func TestIntegrationSlowReaderFailsWhenRetentionRemovesNextID(t *testing.T) {
	store := openTestStore(t, t.TempDir(), 55, 4096, nil)
	defer closeStore(t, store)

	appendMessage(t, store, SourceStdout, "first stdout\n")
	releaseSend := make(chan struct{})
	firstBatchSeen := make(chan struct{})
	errs := make(chan error, 1)
	go func() {
		errs <- store.Stream(context.Background(), SourceStdout, 0, 2, func(entries []Entry) error {
			close(firstBatchSeen)
			<-releaseSend
			return nil
		})
	}()

	select {
	case <-firstBatchSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for stream to receive first batch")
	}

	appendMessage(t, store, SourceStdout, "second stdout\n")
	appendMessage(t, store, SourceStdout, "third stdout\n")
	appendMessage(t, store, SourceStdout, "fourth stdout\n")
	close(releaseSend)

	if err := <-errs; !errors.Is(err, ErrOutOfRange) {
		t.Fatalf("Stream() error = %v, want ErrOutOfRange after retention removes needed ID", err)
	}
}
