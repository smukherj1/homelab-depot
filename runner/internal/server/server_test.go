package server

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	runnerpb "github.com/smukherj/homelab-depot/runner/gen/go/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type fakeStatusReader struct {
	snapshot StatusSnapshot
	err      error
}

func (f fakeStatusReader) Status(context.Context) (StatusSnapshot, error) {
	return f.snapshot, f.err
}

type fakeLogReader struct {
	err error
}

func (f fakeLogReader) StreamLog(_ context.Context, _ LogRequest, send func([]LogEntry) error) error {
	if f.err != nil {
		return f.err
	}
	return send([]LogEntry{{
		Timestamp: time.Unix(100, 200).UTC(),
		ID:        7,
		Source:    LogSourceStdout,
		Level:     "INFO",
		Message:   "ready\n",
	}})
}

type fakeEventReader struct {
	err error
}

func (f fakeEventReader) Events(context.Context, EventRequest) (EventSnapshot, error) {
	if f.err != nil {
		return EventSnapshot{}, f.err
	}
	return EventSnapshot{
		Events: []RunnerEvent{{
			ID:        3,
			Timestamp: time.Unix(101, 0).UTC(),
			Severity:  "INFO",
			Code:      "runner.started",
			Message:   "runner started",
			Details:   map[string]string{"listen_address": "127.0.0.1:0"},
		}},
		NextID: 4,
	}, nil
}

func TestMapStatusPreservesUnsetTimestampsAndOptionalFields(t *testing.T) {
	exitTime := time.Unix(200, 300).UTC()
	exitCode := int32(17)
	snapshot := StatusSnapshot{
		Process: ProcessStatus{
			Running:          false,
			CurrentStartTime: nil,
			LastExitTime:     &exitTime,
			LastExitCode:     &exitCode,
			RestartCount:     2,
			Command:          []string{"/bin/echo", "ok"},
		},
		Stdout: StreamStatus{BeginID: 4, EndID: 9},
		Stderr: StreamStatus{BeginID: 1, EndID: 3},
	}

	got := mapStatus(snapshot)

	if got.GetProcessStatus().GetCurrentStartTime() != nil {
		t.Errorf("CurrentStartTime = %v, want nil for an unset timestamp", got.GetProcessStatus().GetCurrentStartTime())
	}
	if got.GetProcessStatus().GetLastExitTime().AsTime() != exitTime {
		t.Errorf("LastExitTime = %v, want %v", got.GetProcessStatus().GetLastExitTime().AsTime(), exitTime)
	}
	if got.GetProcessStatus().GetLastExitCode() != exitCode {
		t.Errorf("LastExitCode = %d, want %d", got.GetProcessStatus().GetLastExitCode(), exitCode)
	}
	if got.GetProcessStatus().LastExitSignal != nil {
		t.Errorf("LastExitSignal = %q, want nil when unset", got.GetProcessStatus().GetLastExitSignal())
	}
	if got.GetLogStatus().GetStdout().GetBeginId() != 4 || got.GetLogStatus().GetStdout().GetEndId() != 9 {
		t.Errorf("stdout log range = [%d, %d), want [4, 9)", got.GetLogStatus().GetStdout().GetBeginId(), got.GetLogStatus().GetStdout().GetEndId())
	}
}

func TestMapLogEntryPreservesOptionalSourceLocation(t *testing.T) {
	timestamp := time.Unix(300, 400).UTC()
	withSource := mapLogEntry(LogEntry{
		Timestamp: timestamp,
		ID:        12,
		Source:    LogSourceStderr,
		Level:     "WARN",
		Message:   "slow\n",
		SourceLocation: &SourceLocation{
			Function: "main.run",
			File:     "main.go",
			Line:     42,
		},
	})
	withoutSource := mapLogEntry(LogEntry{
		Timestamp: timestamp,
		ID:        13,
		Source:    LogSourceStdout,
		Level:     "INFO",
		Message:   "ok\n",
	})

	if withSource.GetSourceLocation().GetFile() != "main.go" || withSource.GetSourceLocation().GetLine() != 42 {
		t.Errorf("SourceLocation = %+v, want file main.go line 42", withSource.GetSourceLocation())
	}
	if withoutSource.GetSourceLocation() != nil {
		t.Errorf("SourceLocation = %+v, want nil when unset", withoutSource.GetSourceLocation())
	}
}

func TestGetEventsRequestMapping(t *testing.T) {
	all, err := mapEventRequest(&runnerpb.GetEventsRequest{})
	if err != nil {
		t.Fatalf("mapEventRequest(all) error = %v, want nil", err)
	}
	if all.Mode != EventRequestAll {
		t.Errorf("all.Mode = %v, want EventRequestAll", all.Mode)
	}

	last, err := mapEventRequest(&runnerpb.GetEventsRequest{
		RequestMode: &runnerpb.GetEventsRequest_LastCount{LastCount: 5},
	})
	if err != nil {
		t.Fatalf("mapEventRequest(last) error = %v, want nil", err)
	}
	if last.Mode != EventRequestLast || last.LastCount != 5 {
		t.Errorf("last request = %+v, want last_count 5", last)
	}
}

func TestIntegrationStatusCodeMappings(t *testing.T) {
	client, cleanup := startTestServer(t, New(fakeStatusReader{}, fakeLogReader{err: ErrLogOutOfRange}, fakeEventReader{err: ErrInvalidEventRequest}))
	defer cleanup()

	invalidStream, err := client.GetLog(context.Background(), &runnerpb.GetLogRequest{Source: runnerpb.LogSource_LOG_SOURCE_UNSPECIFIED})
	if err != nil {
		t.Fatalf("GetLog(invalid source) start error = %v, want nil before Recv", err)
	}
	_, err = invalidStream.Recv()
	assertCode(t, err, codes.InvalidArgument)

	stream, err := client.GetLog(context.Background(), &runnerpb.GetLogRequest{Source: runnerpb.LogSource_LOG_SOURCE_STDOUT, StartId: 99})
	if err != nil {
		t.Fatalf("GetLog(out of range) start error = %v, want nil before Recv", err)
	}
	_, err = stream.Recv()
	assertCode(t, err, codes.OutOfRange)

	_, err = client.GetEvents(context.Background(), &runnerpb.GetEventsRequest{
		RequestMode: &runnerpb.GetEventsRequest_FromId{FromId: 1},
	})
	assertCode(t, err, codes.InvalidArgument)
}

func TestIntegrationEachRPCSucceeds(t *testing.T) {
	client, cleanup := startTestServer(t, New(fakeStatusReader{
		snapshot: StatusSnapshot{
			Process: ProcessStatus{Running: true, Command: []string{"sleep", "10"}},
			Stdout:  StreamStatus{BeginID: 0, EndID: 1},
		},
	}, fakeLogReader{}, fakeEventReader{}))
	defer cleanup()

	statusResp, err := client.GetStatus(context.Background(), &runnerpb.GetStatusRequest{})
	if err != nil {
		t.Fatalf("GetStatus() error = %v, want nil", err)
	}
	if !statusResp.GetProcessStatus().GetRunning() {
		t.Errorf("GetStatus().running = false, want true")
	}

	logStream, err := client.GetLog(context.Background(), &runnerpb.GetLogRequest{Source: runnerpb.LogSource_LOG_SOURCE_STDOUT})
	if err != nil {
		t.Fatalf("GetLog() start error = %v, want nil", err)
	}
	logResp, err := logStream.Recv()
	if err != nil {
		t.Fatalf("GetLog().Recv() error = %v, want nil", err)
	}
	if len(logResp.GetEntries()) != 1 || logResp.GetEntries()[0].GetId() != 7 {
		t.Errorf("GetLog() entries = %+v, want one entry with id 7", logResp.GetEntries())
	}

	eventsResp, err := client.GetEvents(context.Background(), &runnerpb.GetEventsRequest{})
	if err != nil {
		t.Fatalf("GetEvents() error = %v, want nil", err)
	}
	if eventsResp.GetNextId() != 4 || len(eventsResp.GetEvents()) != 1 {
		t.Errorf("GetEvents() = %+v, want one event and next ID 4", eventsResp)
	}
}

func startTestServer(t *testing.T, service *Service) (runnerpb.RunnerClient, func()) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v, want nil", err)
	}

	grpcServer := grpc.NewServer()
	runnerpb.RegisterRunnerServer(grpcServer, service)
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- grpcServer.Serve(listener)
	}()

	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		grpcServer.Stop()
		t.Fatalf("grpc.NewClient() error = %v, want nil", err)
	}

	cleanup := func() {
		err := conn.Close()
		if err != nil {
			t.Errorf("conn.Close() error = %v, want nil", err)
		}
		grpcServer.Stop()
		if serveErrValue := <-serveErr; serveErrValue != nil && !errors.Is(serveErrValue, grpc.ErrServerStopped) {
			t.Errorf("grpc Serve() error = %v, want nil or ErrServerStopped", serveErrValue)
		}
	}

	return runnerpb.NewRunnerClient(conn), cleanup
}

func assertCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Fatalf("status.Code(%v) = %s, want %s", err, got, want)
	}
}
