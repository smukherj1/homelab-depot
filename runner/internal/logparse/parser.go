package logparse

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/smukherj/homelab-depot/runner/internal/events"
)

const (
	eventInvalidUTF8       = "log.invalid_utf8"
	eventInvalidStructured = "log.invalid_structured"
)

// Source identifies the child output stream that produced a log line.
type Source string

const (
	// SourceStdout identifies child stdout.
	SourceStdout Source = "stdout"
	// SourceStderr identifies child stderr.
	SourceStderr Source = "stderr"
)

// Encoding identifies the configured child log input format.
type Encoding string

const (
	// EncodingPlainText parses newline-delimited plain-text child logs.
	EncodingPlainText Encoding = "plain_text"
	// EncodingSlogJSON parses newline-delimited Go slog JSONHandler records.
	EncodingSlogJSON Encoding = "slog_json"
)

// SourceLocation stores optional producer source information parsed from a
// structured child log line.
type SourceLocation struct {
	Function string
	File     string
	Line     uint32
}

// Entry is one accepted child log entry. It deliberately has no ID; callers
// assign stream-scoped IDs only after an entry is accepted.
type Entry struct {
	Timestamp      time.Time
	Source         Source
	Level          string
	Message        string
	Truncated      bool
	SourceLocation *SourceLocation
}

// EventRecorder records runner events. events.Ring satisfies this interface.
type EventRecorder interface {
	Record(events.Severity, string, string, map[string]string) events.Event
}

// Clock returns the current time for plain-text first-byte timestamps.
type Clock func() time.Time

// Options configures a stream parser. Source, Encoding, and MaxEntryBytes are
// required. Clock defaults to time.Now. Events may be nil, in which case
// rejected lines are silently omitted. CurrentEndID may be nil when callers do
// not have log allocation state yet; rejection events then report end_id as 0.
type Options struct {
	Source        Source
	Encoding      Encoding
	MaxEntryBytes uint64
	Clock         Clock
	Events        EventRecorder
	CurrentEndID  func(Source) uint64
}

// StreamParser incrementally parses one child output stream. It is not safe for
// concurrent use by multiple goroutines.
type StreamParser struct {
	source        Source
	encoding      Encoding
	maxEntryBytes uint64
	clock         Clock
	events        EventRecorder
	currentEndID  func(Source) uint64

	// Parser state.
	// The partially accepted bytes in a line.
	buf []byte
	// True when we've begun accepting bytes for a line.
	lineStarted bool
	// Time when we began accepting bytes for the line.
	lineTimestamp time.Time
	// Whether the line exceeded max bytes and is being truncated.
	truncated bool
	// Whether the parser was closed and should not accept new bytes.
	closed bool
}

// NewStreamParser constructs a parser for one child output stream. The parser
// owns only its internal buffers; passed recorder, clock, and end-ID callback
// must remain valid for as long as the parser is used.
func NewStreamParser(opts Options) (*StreamParser, error) {
	if opts.Source != SourceStdout && opts.Source != SourceStderr {
		return nil, fmt.Errorf("log source %q is invalid", opts.Source)
	}
	if opts.Encoding != EncodingPlainText && opts.Encoding != EncodingSlogJSON {
		return nil, fmt.Errorf("log encoding %q is invalid", opts.Encoding)
	}
	if opts.MaxEntryBytes == 0 {
		return nil, errors.New("max entry bytes must be greater than zero")
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	return &StreamParser{
		source:        opts.Source,
		encoding:      opts.Encoding,
		maxEntryBytes: opts.MaxEntryBytes,
		clock:         opts.Clock,
		events:        opts.Events,
		currentEndID:  opts.CurrentEndID,
		buf:           make([]byte, 0, opts.MaxEntryBytes),
	}, nil
}

// Write consumes one byte chunk and returns every complete accepted log entry
// ending in the chunk. Rejected complete lines are omitted and reported through
// the configured event recorder. Write returns an error only after Close.
func (p *StreamParser) Write(chunk []byte) ([]Entry, error) {
	if p.closed {
		return nil, errors.New("write to closed log parser")
	}
	var entries []Entry
	for _, b := range chunk {
		if !p.lineStarted {
			p.startLine()
		}
		if !p.truncated {
			if uint64(len(p.buf)) < p.maxEntryBytes {
				p.buf = append(p.buf, b)
			} else {
				p.truncated = true
			}
		}
		if b == '\n' {
			if entry, ok := p.finishLine(); ok {
				entries = append(entries, entry)
			}
		}
	}
	return entries, nil
}

// Close flushes a final unterminated line, if present, and prevents further
// writes. Rejected flushed lines are omitted and reported through the configured
// event recorder.
func (p *StreamParser) Close() ([]Entry, error) {
	if p.closed {
		return nil, nil
	}
	p.closed = true
	if !p.lineStarted {
		return nil, nil
	}
	if entry, ok := p.finishLine(); ok {
		return []Entry{entry}, nil
	}
	return nil, nil
}

func (p *StreamParser) startLine() {
	p.lineStarted = true
	p.lineTimestamp = p.clock()
	p.truncated = false
	p.buf = p.buf[:0]
}

func (p *StreamParser) finishLine() (Entry, bool) {
	line := append([]byte(nil), p.buf...)
	timestamp := p.lineTimestamp
	truncated := p.truncated
	p.resetLine()

	switch p.encoding {
	case EncodingPlainText:
		return p.parsePlainText(line, timestamp, truncated)
	case EncodingSlogJSON:
		return p.parseSlogJSON(line, truncated)
	default:
		panic(fmt.Sprintf("unknown parser encoding %q", p.encoding))
	}
}

func (p *StreamParser) resetLine() {
	p.lineStarted = false
	p.lineTimestamp = time.Time{}
	p.truncated = false
	p.buf = p.buf[:0]
}

func (p *StreamParser) parsePlainText(line []byte, timestamp time.Time, truncated bool) (Entry, bool) {
	if !utf8.Valid(line) {
		p.recordInvalidUTF8()
		return Entry{}, false
	}
	level := "INFO"
	if p.source == SourceStderr {
		level = "ERROR"
	}
	return Entry{
		Timestamp: timestamp,
		Source:    p.source,
		Level:     level,
		Message:   string(line),
		Truncated: truncated,
	}, true
}

func (p *StreamParser) parseSlogJSON(line []byte, truncated bool) (Entry, bool) {
	record, err := decodeJSONObject(line)
	if err != nil {
		p.recordInvalidStructured(line, "invalid JSON", truncated)
		return Entry{}, false
	}

	timestamp, err := parseRequiredTime(record)
	if err != nil {
		p.recordInvalidStructured(line, err.Error(), truncated)
		return Entry{}, false
	}
	level, err := requiredString(record, "level")
	if err != nil {
		p.recordInvalidStructured(line, err.Error(), truncated)
		return Entry{}, false
	}
	if level == "" {
		p.recordInvalidStructured(line, "level must not be empty", truncated)
		return Entry{}, false
	}
	message, err := requiredString(record, "msg")
	if err != nil {
		p.recordInvalidStructured(line, err.Error(), truncated)
		return Entry{}, false
	}

	return Entry{
		Timestamp:      timestamp,
		Source:         p.source,
		Level:          level,
		Message:        message,
		Truncated:      truncated,
		SourceLocation: parseSourceLocation(record["source"]),
	}, true
}

func decodeJSONObject(line []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(bytes.TrimRight(line, "\n")))
	decoder.UseNumber()
	var record map[string]any
	if err := decoder.Decode(&record); err != nil {
		return nil, err
	}
	if record == nil {
		return nil, errors.New("JSON record must be an object")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("JSON line must contain one object")
	}
	return record, nil
}

func parseRequiredTime(record map[string]any) (time.Time, error) {
	value, err := requiredString(record, "time")
	if err != nil {
		return time.Time{}, err
	}
	timestamp, parseErr := time.Parse(time.RFC3339Nano, value)
	if parseErr != nil {
		return time.Time{}, fmt.Errorf("time is malformed: %w", parseErr)
	}
	return timestamp, nil
}

func requiredString(record map[string]any, field string) (string, error) {
	value, ok := record[field]
	if !ok {
		return "", fmt.Errorf("%s is required", field)
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", field)
	}
	return text, nil
}

func parseSourceLocation(value any) *SourceLocation {
	source, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	location := &SourceLocation{}
	if function, ok := source["function"].(string); ok {
		location.Function = function
	}
	if file, ok := source["file"].(string); ok {
		location.File = file
	}
	if line, ok := parseSourceLine(source["line"]); ok {
		location.Line = line
	}
	if location.Function == "" && location.File == "" && location.Line == 0 {
		return nil
	}
	return location
}

func parseSourceLine(value any) (uint32, bool) {
	switch line := value.(type) {
	case json.Number:
		n, err := strconv.ParseUint(line.String(), 10, 32)
		if err == nil {
			return uint32(n), true
		}
	case float64:
		if line >= 0 && line == float64(uint32(line)) {
			return uint32(line), true
		}
	}
	return 0, false
}

func (p *StreamParser) recordInvalidUTF8() {
	if p.events == nil {
		return
	}
	p.events.Record(events.SeverityWarn, eventInvalidUTF8, "invalid UTF-8 log line rejected", map[string]string{
		"source": string(p.source),
		"end_id": strconv.FormatUint(p.endID(), 10),
	})
}

func (p *StreamParser) recordInvalidStructured(line []byte, reason string, truncated bool) {
	if p.events == nil {
		return
	}
	p.events.Record(events.SeverityWarn, eventInvalidStructured, "invalid structured log line rejected", map[string]string{
		"source":    string(p.source),
		"end_id":    strconv.FormatUint(p.endID(), 10),
		"reason":    reason,
		"raw_line":  string(line),
		"truncated": strconv.FormatBool(truncated),
	})
}

func (p *StreamParser) endID() uint64 {
	if p.currentEndID == nil {
		return 0
	}
	return p.currentEndID(p.source)
}
