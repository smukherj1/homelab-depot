package events_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	runnerpb "github.com/smukherj/homelab-depot/runner/gen/go/proto"
	"github.com/smukherj/homelab-depot/runner/internal/events"
	"github.com/smukherj/homelab-depot/runner/internal/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type eventReaderAdapter struct {
	ring *events.Ring
}

func (a eventReaderAdapter) Events(ctx context.Context, req server.EventRequest) (server.EventSnapshot, error) {
	query := events.Query{Mode: events.QueryAll}
	switch req.Mode {
	case server.EventRequestFromID:
		query = events.Query{Mode: events.QueryFromID, FromID: req.FromID}
	case server.EventRequestLast:
		query = events.Query{Mode: events.QueryLast, LastCount: req.LastCount}
	case server.EventRequestAll:
	default:
		return server.EventSnapshot{}, server.ErrInvalidEventRequest
	}
	snapshot, err := a.ring.Query(ctx, query)
	if err != nil {
		return server.EventSnapshot{}, err
	}
	return toServerEventSnapshot(snapshot), nil
}

func TestIntegrationGetEventsPollingDeduplicatesWithNextID(t *testing.T) {
	ring, err := events.NewRing(10, 1024, func() time.Time { return time.Unix(100, 0).UTC() })
	if err != nil {
		t.Fatalf("NewRing() error = %v, want nil", err)
	}
	ring.Record(events.SeverityInfo, "runner.startup", "runner started", nil)
	ring.Record(events.SeverityInfo, "child.started", "child started", map[string]string{"pid": "123"})

	client, cleanup := startEventTestServer(t, server.New(nil, nil, eventReaderAdapter{ring: ring}))
	defer cleanup()

	first, err := client.GetEvents(context.Background(), &runnerpb.GetEventsRequest{})
	if err != nil {
		t.Fatalf("GetEvents(all) error = %v, want nil", err)
	}
	if first.GetNextId() != 2 {
		t.Fatalf("first.next_id = %d, want 2", first.GetNextId())
	}
	if len(first.GetEvents()) != 2 {
		t.Fatalf("len(first.events) = %d, want 2", len(first.GetEvents()))
	}

	ring.Record(events.SeverityWarn, "restart.scheduled", "restart scheduled", nil)
	second, err := client.GetEvents(context.Background(), &runnerpb.GetEventsRequest{
		RequestMode: &runnerpb.GetEventsRequest_FromId{FromId: first.GetNextId()},
	})
	if err != nil {
		t.Fatalf("GetEvents(from next_id) error = %v, want nil", err)
	}

	if second.GetNextId() != 3 {
		t.Errorf("second.next_id = %d, want 3", second.GetNextId())
	}
	if len(second.GetEvents()) != 1 || second.GetEvents()[0].GetId() != 2 {
		t.Fatalf("second.events = %+v, want only event ID 2", second.GetEvents())
	}
}

func toServerEventSnapshot(snapshot events.Snapshot) server.EventSnapshot {
	out := server.EventSnapshot{
		Events: make([]server.RunnerEvent, 0, len(snapshot.Events)),
		NextID: snapshot.NextID,
	}
	for _, event := range snapshot.Events {
		out.Events = append(out.Events, server.RunnerEvent{
			ID:        event.ID,
			Timestamp: event.Timestamp,
			Severity:  string(event.Severity),
			Code:      event.Code,
			Message:   event.Message,
			Details:   event.Details,
		})
	}
	return out
}

func startEventTestServer(t *testing.T, service *server.Service) (runnerpb.RunnerClient, func()) {
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
		if err := conn.Close(); err != nil {
			t.Errorf("conn.Close() error = %v, want nil", err)
		}
		grpcServer.Stop()
		if serveErrValue := <-serveErr; serveErrValue != nil && !errors.Is(serveErrValue, grpc.ErrServerStopped) {
			t.Errorf("grpc Serve() error = %v, want nil or ErrServerStopped", serveErrValue)
		}
	}

	return runnerpb.NewRunnerClient(conn), cleanup
}
