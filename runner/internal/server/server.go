// Package server exposes runner status, log, and event APIs over gRPC.
package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	runnerpb "github.com/smukherj/homelab-depot/runner/gen/go/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ErrLogOutOfRange reports that a requested log ID is outside the retained
// range for the selected stream.
var ErrLogOutOfRange = errors.New("log ID out of retained range")

// ErrInvalidEventRequest reports an event query that cannot be represented by a
// valid GetEvents request mode.
var ErrInvalidEventRequest = errors.New("invalid event request")

// LogSource identifies one child output stream.
type LogSource string

const (
	// LogSourceStdout identifies the child stdout stream.
	LogSourceStdout LogSource = "stdout"
	// LogSourceStderr identifies the child stderr stream.
	LogSourceStderr LogSource = "stderr"
)

// SourceLocation stores an optional source location parsed from a structured
// child log entry.
type SourceLocation struct {
	Function string
	File     string
	Line     uint32
}

// ProcessStatus is a point-in-time snapshot of the supervised child process.
// Timestamps use nil pointers when the corresponding value is unset.
type ProcessStatus struct {
	Running          bool
	CurrentStartTime *time.Time
	LastExitTime     *time.Time
	LastExitCode     *int32
	LastExitSignal   *string
	RestartCount     uint64
	Command          []string
}

// StreamStatus is the retained half-open log ID range for one child log
// stream.
type StreamStatus struct {
	BeginID uint64
	EndID   uint64
}

// StatusSnapshot combines process state with retained log ranges for both
// child output streams.
type StatusSnapshot struct {
	Process ProcessStatus
	Stdout  StreamStatus
	Stderr  StreamStatus
}

// LogEntry is the server-internal representation of a child log entry. The
// timestamp is required because accepted child log entries always carry one.
type LogEntry struct {
	Timestamp      time.Time
	ID             uint64
	Source         LogSource
	Level          string
	Message        string
	Truncated      bool
	SourceLocation *SourceLocation
}

// LogRequest describes one GetLog stream request after proto validation.
type LogRequest struct {
	Source     LogSource
	StartID    uint64
	MaxEntries uint64
}

// EventRequestMode identifies which GetEvents request mode was selected.
type EventRequestMode int

const (
	// EventRequestAll requests all currently retained runner events.
	EventRequestAll EventRequestMode = iota
	// EventRequestFromID requests retained events at or after an event ID.
	EventRequestFromID
	// EventRequestLast requests up to the newest N retained events.
	EventRequestLast
)

// EventRequest describes one validated GetEvents request.
type EventRequest struct {
	Mode      EventRequestMode
	FromID    uint64
	LastCount uint64
}

// RunnerEvent is the server-internal representation of one runner lifecycle
// event.
type RunnerEvent struct {
	ID        uint64
	Timestamp time.Time
	Severity  string
	Code      string
	Message   string
	Details   map[string]string
}

// EventSnapshot contains the retained event query result and the next event ID
// clients should use for incremental polling.
type EventSnapshot struct {
	Events []RunnerEvent
	NextID uint64
}

// StatusReader returns point-in-time process and log status. Implementations
// must be safe for concurrent calls from gRPC handlers.
type StatusReader interface {
	Status(context.Context) (StatusSnapshot, error)
}

// LogReader streams child log batches to the supplied callback. Implementations
// own read blocking and must stop when the context is canceled.
type LogReader interface {
	StreamLog(context.Context, LogRequest, func([]LogEntry) error) error
}

// EventReader returns retained runner events for a validated request.
// Implementations must be safe for concurrent polling.
type EventReader interface {
	Events(context.Context, EventRequest) (EventSnapshot, error)
}

// Service implements runnerpb.RunnerServer by adapting narrow internal
// interfaces to the protobuf API. The passed dependencies are not owned by the
// service and must remain valid for the service lifetime.
type Service struct {
	runnerpb.UnimplementedRunnerServer

	status StatusReader
	logs   LogReader
	events EventReader
}

// New constructs a gRPC Runner service. Nil dependencies are accepted so tests
// and partially wired binaries can instantiate the service; calling the matching
// RPC without a dependency returns an internal error.
func New(status StatusReader, logs LogReader, events EventReader) *Service {
	return &Service{status: status, logs: logs, events: events}
}

// GetStatus returns current process state and retained log ranges.
func (s *Service) GetStatus(ctx context.Context, _ *runnerpb.GetStatusRequest) (*runnerpb.GetStatusResponse, error) {
	if s.status == nil {
		return nil, status.Error(codes.Internal, "status reader is not configured")
	}
	snapshot, err := s.status.Status(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read status: %v", err)
	}
	return mapStatus(snapshot), nil
}

// GetLog streams retained child log entries for one source.
func (s *Service) GetLog(req *runnerpb.GetLogRequest, stream runnerpb.Runner_GetLogServer) error {
	if s.logs == nil {
		return status.Error(codes.Internal, "log reader is not configured")
	}
	query, err := mapLogRequest(req)
	if err != nil {
		return err
	}
	err = s.logs.StreamLog(stream.Context(), query, func(entries []LogEntry) error {
		return stream.Send(&runnerpb.GetLogResponse{Entries: mapLogEntries(entries)})
	})
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrLogOutOfRange) {
		return status.Errorf(codes.OutOfRange, "read log: %v", err)
	}
	return status.Errorf(codes.Internal, "read log: %v", err)
}

// GetEvents returns retained runner lifecycle events.
func (s *Service) GetEvents(ctx context.Context, req *runnerpb.GetEventsRequest) (*runnerpb.GetEventsResponse, error) {
	if s.events == nil {
		return nil, status.Error(codes.Internal, "event reader is not configured")
	}
	query, err := mapEventRequest(req)
	if err != nil {
		return nil, err
	}
	snapshot, err := s.events.Events(ctx, query)
	if err == nil {
		return mapEventSnapshot(snapshot), nil
	}
	if errors.Is(err, ErrInvalidEventRequest) {
		return nil, status.Errorf(codes.InvalidArgument, "read events: %v", err)
	}
	return nil, status.Errorf(codes.Internal, "read events: %v", err)
}

func mapStatus(snapshot StatusSnapshot) *runnerpb.GetStatusResponse {
	return &runnerpb.GetStatusResponse{
		ProcessStatus: mapProcessStatus(snapshot.Process),
		LogStatus: &runnerpb.ChildLogStatus{
			Stdout: mapStreamStatus(snapshot.Stdout),
			Stderr: mapStreamStatus(snapshot.Stderr),
		},
	}
}

func mapProcessStatus(process ProcessStatus) *runnerpb.ProcessStatus {
	return &runnerpb.ProcessStatus{
		Running:          process.Running,
		CurrentStartTime: toTimeProto(process.CurrentStartTime),
		LastExitTime:     toTimeProto(process.LastExitTime),
		LastExitCode:     process.LastExitCode,
		LastExitSignal:   process.LastExitSignal,
		RestartCount:     process.RestartCount,
		Command:          append([]string(nil), process.Command...),
	}
}

func mapStreamStatus(stream StreamStatus) *runnerpb.LogStatus {
	return &runnerpb.LogStatus{
		BeginId: stream.BeginID,
		EndId:   stream.EndID,
	}
}

func mapLogRequest(req *runnerpb.GetLogRequest) (LogRequest, error) {
	if req == nil {
		return LogRequest{}, status.Error(codes.InvalidArgument, "log request is required")
	}
	source, err := toLogSource(req.GetSource())
	if err != nil {
		return LogRequest{}, err
	}
	return LogRequest{
		Source:     source,
		StartID:    req.GetStartId(),
		MaxEntries: req.GetMaxEntries(),
	}, nil
}

func mapLogEntries(entries []LogEntry) []*runnerpb.LogEntry {
	out := make([]*runnerpb.LogEntry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, mapLogEntry(entry))
	}
	return out
}

func mapLogEntry(entry LogEntry) *runnerpb.LogEntry {
	return &runnerpb.LogEntry{
		Timestamp:      timestamppb.New(entry.Timestamp),
		Id:             entry.ID,
		Source:         toLogSourceProto(entry.Source),
		Level:          entry.Level,
		Message:        entry.Message,
		Truncated:      entry.Truncated,
		SourceLocation: mapSourceLocation(entry.SourceLocation),
	}
}

func mapSourceLocation(location *SourceLocation) *runnerpb.SourceLocation {
	if location == nil {
		return nil
	}
	return &runnerpb.SourceLocation{
		Function: location.Function,
		File:     location.File,
		Line:     location.Line,
	}
}

func mapEventRequest(req *runnerpb.GetEventsRequest) (EventRequest, error) {
	if req == nil || req.GetRequestMode() == nil {
		return EventRequest{Mode: EventRequestAll}, nil
	}
	switch mode := req.GetRequestMode().(type) {
	case *runnerpb.GetEventsRequest_FromId:
		return EventRequest{Mode: EventRequestFromID, FromID: mode.FromId}, nil
	case *runnerpb.GetEventsRequest_LastCount:
		return EventRequest{Mode: EventRequestLast, LastCount: mode.LastCount}, nil
	default:
		return EventRequest{}, status.Errorf(codes.InvalidArgument, "unsupported event request mode %T", mode)
	}
}

func mapEventSnapshot(snapshot EventSnapshot) *runnerpb.GetEventsResponse {
	events := make([]*runnerpb.RunnerEvent, 0, len(snapshot.Events))
	for _, event := range snapshot.Events {
		events = append(events, mapRunnerEvent(event))
	}
	return &runnerpb.GetEventsResponse{
		Events: events,
		NextId: snapshot.NextID,
	}
}

func mapRunnerEvent(event RunnerEvent) *runnerpb.RunnerEvent {
	return &runnerpb.RunnerEvent{
		Id:        event.ID,
		Timestamp: timestamppb.New(event.Timestamp),
		Severity:  event.Severity,
		Code:      event.Code,
		Message:   event.Message,
		Details:   cloneStringMap(event.Details),
	}
}

func toTimeProto(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}

func toLogSource(source runnerpb.LogSource) (LogSource, error) {
	switch source {
	case runnerpb.LogSource_LOG_SOURCE_STDOUT:
		return LogSourceStdout, nil
	case runnerpb.LogSource_LOG_SOURCE_STDERR:
		return LogSourceStderr, nil
	default:
		return "", status.Errorf(codes.InvalidArgument, "invalid log source %s", source.String())
	}
}

func toLogSourceProto(source LogSource) runnerpb.LogSource {
	switch source {
	case LogSourceStdout:
		return runnerpb.LogSource_LOG_SOURCE_STDOUT
	case LogSourceStderr:
		return runnerpb.LogSource_LOG_SOURCE_STDERR
	default:
		panic(fmt.Sprintf("unknown internal log source %q", source))
	}
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
