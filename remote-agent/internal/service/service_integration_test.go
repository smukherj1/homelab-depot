package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/smukherj/homelab-depot/remote-agent/gen/go/proto"
	"github.com/smukherj/homelab-depot/remote-agent/internal/config"
	"github.com/smukherj/homelab-depot/remote-agent/internal/runner"
	"github.com/smukherj/homelab-depot/remote-agent/internal/session"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestIntegrationFlowWithFakeRunner(t *testing.T) {
	client, cleanup := integrationClient(t)
	defer cleanup()
	ctx := context.Background()
	if _, err := client.CreateSession(ctx, &agentpb.CreateSessionRequest{SessionId: "s1"}); err != nil {
		t.Fatalf("CreateSession should create active session s1 for integration flow: %v", err)
	}
	data := []byte("hello")
	sum := sha256.Sum256(data)
	up, err := client.Upload(ctx)
	if err != nil {
		t.Fatalf("Upload should open a client stream for active session s1: %v", err)
	}
	if err := up.Send(&agentpb.UploadRequest{SessionId: "s1", Request: &agentpb.UploadRequest_Header{Header: &agentpb.UploadFileHeader{Filename: "in.txt", Permissions: 0o644}}}); err != nil {
		t.Fatalf("Upload should accept initial header for in.txt with safe mode 0644: %v", err)
	}
	if err := up.Send(&agentpb.UploadRequest{SessionId: "s1", Request: &agentpb.UploadRequest_Chunk{Chunk: &agentpb.UploadFileChunk{Offset: 0, Data: data, Final: true, Sha256Digest: hex.EncodeToString(sum[:])}}}); err != nil {
		t.Fatalf("Upload should accept final chunk at offset 0 with matching digest: %v", err)
	}
	if _, err := up.CloseAndRecv(); err != nil {
		t.Fatalf("Upload should commit in.txt and return a close response: %v", err)
	}
	meta, err := client.GetPathMetadata(ctx, &agentpb.GetPathMetadataRequest{SessionId: "s1", Path: "in.txt"})
	if err != nil {
		t.Fatalf("GetPathMetadata should return metadata for uploaded file in.txt: %v", err)
	}
	if meta.GetFileMetadata().GetSize() != uint64(len(data)) {
		t.Fatalf("GetPathMetadata should report uploaded file size %d, got %d", len(data), meta.GetFileMetadata().GetSize())
	}
	down, err := client.Download(ctx, &agentpb.DownloadRequest{SessionId: "s1", Filename: "in.txt"})
	if err != nil {
		t.Fatalf("Download should open stream for uploaded file in.txt: %v", err)
	}
	var got []byte
	for {
		resp, err := down.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Download stream should yield chunks until final chunk or EOF, got error %v", err)
		}
		got = append(got, resp.GetChunk().GetData()...)
		if resp.GetChunk().GetFinal() {
			break
		}
	}
	if string(got) != string(data) {
		t.Fatalf("Download should return uploaded bytes %q, got %q", data, got)
	}
	execResp, err := client.Execute(ctx, &agentpb.ExecuteRequest{SessionId: "s1", Cmd: []string{"echo", "ok"}})
	if err != nil {
		t.Fatalf("Execute should run fake runner for valid command in active session: %v", err)
	}
	if execResp.GetStdout() != "fake\n" {
		t.Fatalf("Execute should return fake runner stdout %q, got %q", "fake\n", execResp.GetStdout())
	}
	if _, err := client.CompleteSession(ctx, &agentpb.CompleteSessionRequest{SessionId: "s1"}); err != nil {
		t.Fatalf("CompleteSession should complete and clean up active session s1: %v", err)
	}
}

func TestIntegrationSessionCollision(t *testing.T) {
	client, cleanup := integrationClient(t)
	defer cleanup()
	ctx := context.Background()
	if _, err := client.CreateSession(ctx, &agentpb.CreateSessionRequest{SessionId: "s1"}); err != nil {
		t.Fatalf("CreateSession should create first session before collision check: %v", err)
	}
	_, err := client.CreateSession(ctx, &agentpb.CreateSessionRequest{SessionId: "s1"})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("CreateSession should return AlreadyExists for duplicate active session, got code %s err %v", status.Code(err), err)
	}
}

func integrationClient(t *testing.T) (agentpb.AgentClient, func()) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	cfg := config.Defaults()
	cfg.MaxUploadSize = 1024
	cfg.MaxUploadChunk = 1024
	cfg.MaxDownloadSize = 1024
	cfg.DownloadChunk = 64
	mgr, err := session.NewManager(t.TempDir(), time.Minute)
	if err != nil {
		t.Fatalf("integrationClient should create session manager rooted in temp dir: %v", err)
	}
	server := grpc.NewServer()
	agentpb.RegisterAgentServer(server, New(cfg, mgr, runner.Fake{RunFunc: func(context.Context, runner.Request) (runner.Result, error) {
		return runner.Result{ExitCode: 0, Stdout: "fake\n"}, nil
	}}))
	go func() {
		_ = server.Serve(lis)
	}()
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	}), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("integrationClient should create gRPC client over bufconn listener: %v", err)
	}
	return agentpb.NewAgentClient(conn), func() {
		conn.Close()
		server.Stop()
		mgr.Close()
	}
}
