package logparse

import (
	"strings"
	"testing"
	"time"

	"github.com/smukherj/homelab-depot/runner/internal/events"
)

type recordingEvents struct {
	events []events.Event
}

func (r *recordingEvents) Record(severity events.Severity, code string, message string, details map[string]string) events.Event {
	event := events.Event{
		ID:       uint64(len(r.events)),
		Severity: severity,
		Code:     code,
		Message:  message,
		Details:  details,
	}
	r.events = append(r.events, event)
	return event
}

func TestPlainTextAssignsLevelsByStream(t *testing.T) {
	stdout := newTestParser(t, Options{Source: SourceStdout, Encoding: EncodingPlainText, MaxEntryBytes: 1024})
	stderr := newTestParser(t, Options{Source: SourceStderr, Encoding: EncodingPlainText, MaxEntryBytes: 1024})

	stdoutEntries := writeEntries(t, stdout, "ready\n")
	stderrEntries := writeEntries(t, stderr, "failed\n")

	if stdoutEntries[0].Level != "INFO" {
		t.Errorf("stdout level = %q, want INFO", stdoutEntries[0].Level)
	}
	if stderrEntries[0].Level != "ERROR" {
		t.Errorf("stderr level = %q, want ERROR", stderrEntries[0].Level)
	}
	if stdoutEntries[0].Message != "ready\n" {
		t.Errorf("stdout message = %q, want trailing newline retained", stdoutEntries[0].Message)
	}
}

func TestPlainTextTimestampUsesFirstByteObservation(t *testing.T) {
	times := []time.Time{
		time.Unix(10, 0).UTC(),
		time.Unix(20, 0).UTC(),
	}
	parser := newTestParser(t, Options{
		Source:        SourceStdout,
		Encoding:      EncodingPlainText,
		MaxEntryBytes: 1024,
		Clock: func() time.Time {
			next := times[0]
			times = times[1:]
			return next
		},
	})

	if entries := writeEntries(t, parser, "hel"); len(entries) != 0 {
		t.Fatalf("entries after partial write = %d, want 0", len(entries))
	}
	got := writeEntries(t, parser, "lo\n")

	if got[0].Timestamp != time.Unix(10, 0).UTC() {
		t.Errorf("Timestamp = %v, want first byte time", got[0].Timestamp)
	}
}

func TestCloseFlushesUnterminatedLine(t *testing.T) {
	parser := newTestParser(t, Options{Source: SourceStdout, Encoding: EncodingPlainText, MaxEntryBytes: 1024})
	if entries := writeEntries(t, parser, "unterminated"); len(entries) != 0 {
		t.Fatalf("entries after partial write = %d, want 0", len(entries))
	}

	got, err := parser.Close()
	if err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}

	if len(got) != 1 || got[0].Message != "unterminated" {
		t.Fatalf("Close() entries = %+v, want flushed unterminated line", got)
	}
}

func TestPlainTextInvalidUTF8EmitsEventAndRejectsLine(t *testing.T) {
	recorder := &recordingEvents{}
	parser := newTestParser(t, Options{
		Source:        SourceStderr,
		Encoding:      EncodingPlainText,
		MaxEntryBytes: 1024,
		Events:        recorder,
		CurrentEndID:  func(Source) uint64 { return 41 },
	})

	got, err := parser.Write([]byte{0xff, '\n'})
	if err != nil {
		t.Fatalf("Write() error = %v, want nil", err)
	}

	if len(got) != 0 {
		t.Errorf("entries = %+v, want invalid UTF-8 line rejected", got)
	}
	assertOneEvent(t, recorder, eventInvalidUTF8, "stderr", "41")
}

func TestPlainTextOversizedEntryRetainsPrefixAndDiscardsSuffix(t *testing.T) {
	parser := newTestParser(t, Options{Source: SourceStdout, Encoding: EncodingPlainText, MaxEntryBytes: 5})

	got := writeEntries(t, parser, "abcdef\n")

	if got[0].Message != "abcde" {
		t.Errorf("Message = %q, want retained prefix only", got[0].Message)
	}
	if !got[0].Truncated {
		t.Errorf("Truncated = false, want true")
	}
}

func TestSlogJSONParsesValidLinesWithAndWithoutSource(t *testing.T) {
	parser := newTestParser(t, Options{Source: SourceStdout, Encoding: EncodingSlogJSON, MaxEntryBytes: 1024})

	got := writeEntries(t, parser, `{"time":"2026-05-09T12:00:00.123456789Z","level":"INFO","msg":"ready","source":{"function":"main.run","file":"main.go","line":42}}`+"\n")
	withoutSource := writeEntries(t, parser, `{"time":"2026-05-09T12:00:01Z","level":"INFO","msg":"next"}`+"\n")

	if got[0].Timestamp.Format(time.RFC3339Nano) != "2026-05-09T12:00:00.123456789Z" {
		t.Errorf("Timestamp = %s, want producer timestamp", got[0].Timestamp.Format(time.RFC3339Nano))
	}
	if got[0].Message != "ready" || got[0].Level != "INFO" {
		t.Errorf("entry = %+v, want parsed level and msg", got[0])
	}
	if got[0].SourceLocation == nil || got[0].SourceLocation.File != "main.go" || got[0].SourceLocation.Line != 42 {
		t.Errorf("SourceLocation = %+v, want parsed source", got[0].SourceLocation)
	}
	if withoutSource[0].SourceLocation != nil {
		t.Errorf("SourceLocation = %+v, want nil when source absent", withoutSource[0].SourceLocation)
	}
}

func TestSlogJSONPreservesCustomLevels(t *testing.T) {
	parser := newTestParser(t, Options{Source: SourceStdout, Encoding: EncodingSlogJSON, MaxEntryBytes: 1024})

	got := writeEntries(t, parser, `{"time":"2026-05-09T12:00:00Z","level":"NOTICE+3","msg":"custom"}`+"\n")

	if got[0].Level != "NOTICE+3" {
		t.Errorf("Level = %q, want custom level preserved", got[0].Level)
	}
}

func TestSlogJSONRejectsInvalidRequiredFields(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{name: "invalid JSON", line: `{"time":`},
		{name: "missing time", line: `{"level":"INFO","msg":"ready"}`},
		{name: "malformed time", line: `{"time":"nope","level":"INFO","msg":"ready"}`},
		{name: "missing level", line: `{"time":"2026-05-09T12:00:00Z","msg":"ready"}`},
		{name: "empty level", line: `{"time":"2026-05-09T12:00:00Z","level":"","msg":"ready"}`},
		{name: "missing msg", line: `{"time":"2026-05-09T12:00:00Z","level":"INFO"}`},
		{name: "non-string msg", line: `{"time":"2026-05-09T12:00:00Z","level":"INFO","msg":42}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &recordingEvents{}
			parser := newTestParser(t, Options{
				Source:        SourceStdout,
				Encoding:      EncodingSlogJSON,
				MaxEntryBytes: 1024,
				Events:        recorder,
				CurrentEndID:  func(Source) uint64 { return 7 },
			})

			got := writeEntries(t, parser, tc.line+"\n")

			if len(got) != 0 {
				t.Errorf("entries = %+v, want rejected line", got)
			}
			assertOneEvent(t, recorder, eventInvalidStructured, "stdout", "7")
			if recorder.events[0].Details["raw_line"] != tc.line+"\n" {
				t.Errorf("raw_line = %q, want original line", recorder.events[0].Details["raw_line"])
			}
		})
	}
}

func TestSlogJSONIgnoresInvalidOptionalSourceFields(t *testing.T) {
	parser := newTestParser(t, Options{Source: SourceStdout, Encoding: EncodingSlogJSON, MaxEntryBytes: 1024})

	got := writeEntries(t, parser, `{"time":"2026-05-09T12:00:00Z","level":"INFO","msg":"ready","source":{"function":99,"file":false,"line":"bad","extra":"ignored"}}`+"\n")

	if got[0].SourceLocation != nil {
		t.Errorf("SourceLocation = %+v, want nil when recognized source fields are invalid", got[0].SourceLocation)
	}
}

func TestSlogJSONTruncatedPrefixCanBeAccepted(t *testing.T) {
	prefix := `{"time":"2026-05-09T12:00:00Z","level":"INFO","msg":"ready"}`
	parser := newTestParser(t, Options{Source: SourceStdout, Encoding: EncodingSlogJSON, MaxEntryBytes: uint64(len(prefix))})

	got := writeEntries(t, parser, prefix+` trailing bytes`+"\n")

	if len(got) != 1 {
		t.Fatalf("entries = %d, want accepted truncated prefix", len(got))
	}
	if !got[0].Truncated {
		t.Errorf("Truncated = false, want true")
	}
	if got[0].Message != "ready" {
		t.Errorf("Message = %q, want parsed prefix message", got[0].Message)
	}
}

func TestSlogJSONTruncatedRejectedPrefixEmitsEvent(t *testing.T) {
	recorder := &recordingEvents{}
	parser := newTestParser(t, Options{
		Source:        SourceStdout,
		Encoding:      EncodingSlogJSON,
		MaxEntryBytes: 12,
		Events:        recorder,
	})

	got := writeEntries(t, parser, `{"time":"2026-05-09T12:00:00Z","level":"INFO","msg":"ready"}`+"\n")

	if len(got) != 0 {
		t.Errorf("entries = %+v, want truncated invalid prefix rejected", got)
	}
	assertOneEvent(t, recorder, eventInvalidStructured, "stdout", "0")
	if recorder.events[0].Details["raw_line"] != `{"time":"202` {
		t.Errorf("raw_line = %q, want retained prefix", recorder.events[0].Details["raw_line"])
	}
	if recorder.events[0].Details["truncated"] != "true" {
		t.Errorf("truncated detail = %q, want true", recorder.events[0].Details["truncated"])
	}
}

func newTestParser(t *testing.T, opts Options) *StreamParser {
	t.Helper()
	if opts.Clock == nil {
		opts.Clock = func() time.Time { return time.Unix(100, 0).UTC() }
	}
	parser, err := NewStreamParser(opts)
	if err != nil {
		t.Fatalf("NewStreamParser() error = %v, want nil", err)
	}
	return parser
}

func writeEntries(t *testing.T, parser *StreamParser, value string) []Entry {
	t.Helper()
	entries, err := parser.Write([]byte(value))
	if err != nil {
		t.Fatalf("Write() error = %v, want nil", err)
	}
	return entries
}

func assertOneEvent(t *testing.T, recorder *recordingEvents, code string, source string, endID string) {
	t.Helper()
	if len(recorder.events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(recorder.events))
	}
	event := recorder.events[0]
	if event.Code != code {
		t.Errorf("Code = %q, want %q", event.Code, code)
	}
	if event.Details["source"] != source {
		t.Errorf("source detail = %q, want %q", event.Details["source"], source)
	}
	if event.Details["end_id"] != endID {
		t.Errorf("end_id detail = %q, want %q", event.Details["end_id"], endID)
	}
	if strings.TrimSpace(event.Message) == "" {
		t.Errorf("Message is empty, want human-readable rejection message")
	}
}
