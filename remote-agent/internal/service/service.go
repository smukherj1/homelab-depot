package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	agentpb "github.com/smukherj/homelab-depot/remote-agent/gen/go/proto"
	"github.com/smukherj/homelab-depot/remote-agent/internal/config"
	"github.com/smukherj/homelab-depot/remote-agent/internal/pathutil"
	"github.com/smukherj/homelab-depot/remote-agent/internal/runner"
	"github.com/smukherj/homelab-depot/remote-agent/internal/session"
	"github.com/smukherj/homelab-depot/remote-agent/internal/transfer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Server implements the Agent gRPC service.
//
// Config, Session, and Runner must be fully initialized before the server is
// registered with gRPC. Server methods are safe for concurrent RPC calls as long
// as the supplied session.Manager and runner.Runner implementations are safe for
// their documented usage.
type Server struct {
	agentpb.UnimplementedAgentServer
	// Config contains immutable limits and defaults used by RPC handlers.
	Config config.Config
	// Session manages the single active session and must be non-nil.
	Session *session.Manager
	// Runner executes validated commands for Execute and must be non-nil.
	Runner runner.Runner
}

// New constructs a Server from validated configuration and dependencies.
//
// cfg should have passed config.Validate. mgr and run must be non-nil. New does
// not start listeners or mutate dependencies; it only stores them for RPC
// handlers.
func New(cfg config.Config, mgr *session.Manager, run runner.Runner) *Server {
	return &Server{Config: cfg, Session: mgr, Runner: run}
}

// GetStatus returns process-scoped server status.
//
// It does not require a session ID and has no side effects. The response
// contains the number of active sessions, currently 0 or 1. It returns nil error
// unless a future dependency is added.
func (s *Server) GetStatus(context.Context, *agentpb.GetStatusRequest) (*agentpb.GetStatusResponse, error) {
	return &agentpb.GetStatusResponse{ActiveSessions: int32(s.Session.ActiveCount())}, nil
}

// CreateSession validates req.SessionId and creates the active workspace.
//
// The request must include a valid session ID and no other session may be
// active. On success it creates the workspace and returns an empty response. It
// maps invalid IDs, duplicate sessions, and filesystem errors to gRPC status
// errors.
func (s *Server) CreateSession(_ context.Context, req *agentpb.CreateSessionRequest) (*agentpb.CreateSessionResponse, error) {
	if _, err := s.Session.Create(req.GetSessionId()); err != nil {
		return nil, mapErr(err)
	}
	log.Printf("Created session %v.", req.GetSessionId())
	return &agentpb.CreateSessionResponse{}, nil
}

// GetSession validates that req.SessionId refers to the active session.
//
// It updates session activity through the manager and returns an empty response
// on success. Invalid, unknown, or completing sessions are returned as gRPC
// status errors.
func (s *Server) GetSession(_ context.Context, req *agentpb.GetSessionRequest) (*agentpb.GetSessionResponse, error) {
	if _, err := s.Session.Get(req.GetSessionId()); err != nil {
		return nil, mapErr(err)
	}
	return &agentpb.GetSessionResponse{}, nil
}

// CompleteSession completes the requested active session and removes its
// workspace.
//
// req.SessionId must be valid and match the active session. On success new work
// for that session is rejected and the workspace is removed. Errors are mapped
// to gRPC status codes.
func (s *Server) CompleteSession(_ context.Context, req *agentpb.CompleteSessionRequest) (*agentpb.CompleteSessionResponse, error) {
	if err := s.Session.Complete(req.GetSessionId()); err != nil {
		return nil, mapErr(err)
	}
	log.Printf("Completed session %v.", req.GetSessionId())
	return &agentpb.CompleteSessionResponse{}, nil
}

// Upload receives one file through a client-streaming RPC and commits it
// atomically.
//
// The first stream message must be a header with a valid active session ID,
// safe relative filename, and safe permission mode. Later messages must use the
// same session ID, contain chunks with exact monotonically increasing offsets,
// respect configured size limits, and include a SHA-256 digest on the final
// chunk. On success the destination file is replaced and UploadResponse reports
// the next offset. Validation, digest, I/O, cancellation, and session errors are
// returned as gRPC status errors and the temporary file is removed.
func (s *Server) Upload(stream agentpb.Agent_UploadServer) error {
	var (
		lease    *session.Lease
		snap     *session.Session
		target   string
		tmp      *os.File
		tmpName  string
		expected uint64
		hash     = sha256.New()
		mode     uint32
	)
	cleanup := func() {
		if tmp != nil {
			_ = tmp.Close()
		}
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
		if lease != nil {
			lease.Done()
		}
	}
	defer cleanup()

	first, err := stream.Recv()
	if err != nil {
		return status.Error(codes.InvalidArgument, "upload header is required")
	}
	header := first.GetHeader()
	if header == nil {
		return status.Error(codes.InvalidArgument, "first upload message must be header")
	}
	lease, err = s.Session.Acquire(first.GetSessionId())
	if err != nil {
		return mapErr(err)
	}
	snap, err = lease.Session()
	if err != nil {
		return mapErr(err)
	}
	mode = header.GetPermissions()
	if err := pathutil.SafeMode(mode); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	target, err = pathutil.ResolveForCreate(snap.Workspace, header.GetFilename())
	if err != nil {
		return mapErr(err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return status.Error(codes.Internal, "create upload parent")
	}
	tmp, err = os.CreateTemp(filepath.Dir(target), ".upload-*")
	if err != nil {
		return status.Error(codes.Internal, "create upload temp")
	}
	tmpName = tmp.Name()
	log.Printf("Session %v: Starting upload for %v to temp location %v.", first.GetSessionId(), header.GetFilename(), tmpName)
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return status.Error(codes.InvalidArgument, "upload missing final chunk")
		}
		if err != nil {
			return mapErr(err)
		}
		if msg.GetSessionId() != snap.ID {
			return status.Error(codes.InvalidArgument, "stream session id changed")
		}
		if msg.GetHeader() != nil {
			return status.Errorf(codes.InvalidArgument, "unexpected header in stream, only the first message must be a header")
		}
		chunk := msg.GetChunk()
		if chunk == nil {
			return status.Error(codes.InvalidArgument, "chunk missing in received message")
		}
		if chunk.GetOffset() != expected {
			return status.Errorf(codes.InvalidArgument, "unexpected upload offset, got %v, expected %v", chunk.GetOffset(), expected)
		}
		if int64(len(chunk.GetData())) > s.Config.MaxUploadChunk {
			return status.Errorf(codes.ResourceExhausted, "upload chunk too large, chunk size %v, limit %v", len(chunk.GetData()), s.Config.MaxUploadChunk)
		}
		if total := int64(expected) + int64(len(chunk.GetData())); total > s.Config.MaxUploadSize {
			return status.Errorf(codes.ResourceExhausted, "upload file too large, %v total bytes exceeds limit %v bytes", total, s.Config.MaxUploadSize)
		}
		if len(chunk.GetData()) > 0 {
			if _, err := tmp.Write(chunk.GetData()); err != nil {
				return status.Error(codes.Internal, "write upload temp")
			}
			_, err = hash.Write(chunk.GetData())
			if err != nil {
				return status.Errorf(codes.Internal, "error digesting received chunk: %v", err)
			}
			expected += uint64(len(chunk.GetData()))
		}
		if !chunk.GetFinal() {
			continue
		}
		if chunk.GetSha256Digest() == "" {
			return status.Error(codes.InvalidArgument, "final upload chunk requires sha256 digest")
		}
		if got := hex.EncodeToString(hash.Sum(nil)); !strings.EqualFold(got, chunk.GetSha256Digest()) {
			return status.Errorf(codes.InvalidArgument, "sha256 digest mismatch, requested %v doesn't match calculated %v", chunk.GetSha256Digest(), got)
		}
		if err := tmp.Close(); err != nil {
			log.Printf("Error: Session %v: Upload for %v, unable to close temp file: %v", first.GetSessionId(), header.GetFilename(), err)
			return status.Error(codes.Internal, "unable to close temp file created for upload")
		}
		tmp = nil
		if err := os.Chmod(tmpName, os.FileMode(mode)); err != nil {
			log.Printf("Error: Session %v: Upload for %v, unable to chmod temp file: %v", first.GetSessionId(), header.GetFilename(), err)
			return status.Error(codes.Internal, "unable to chmod uploaded temp file")
		}
		if err := os.Rename(tmpName, target); err != nil {
			log.Printf("Error: Session %v: Upload for %v, unable to commit upload: %v", first.GetSessionId(), header.GetFilename(), err)
			return status.Error(codes.Internal, "unable to commit upload")
		}
		tmpName = ""
		log.Printf("Session %v: Completed upload for %v, size %v bytes, digest %v.", first.GetSessionId(), header.GetFilename(), expected, chunk.GetSha256Digest())
		if err := stream.SendAndClose(&agentpb.UploadResponse{NextOffset: expected}); err != nil {
			return mapErr(err)
		}
		return nil
	}
}

// Download streams one regular file from an active session.
//
// req.SessionId must be valid and active, and req.Filename must name an existing
// regular file inside the workspace whose size does not exceed configuration.
// Responses contain ordered chunks beginning at offset 0; the final chunk
// includes the SHA-256 digest of streamed bytes. Errors cover invalid sessions,
// unsafe paths, non-regular files, size limits, filesystem reads, and stream
// sends.
func (s *Server) Download(req *agentpb.DownloadRequest, stream agentpb.Agent_DownloadServer) error {
	lease, err := s.Session.Acquire(req.GetSessionId())
	if err != nil {
		return mapErr(err)
	}
	defer lease.Done()
	snap, err := lease.Session()
	if err != nil {
		return mapErr(err)
	}
	f, _, err := transfer.OpenDownload(transfer.DownloadOptions{
		Workspace: snap.Workspace,
		Filename:  req.GetFilename(),
		MaxSize:   s.Config.MaxDownloadSize,
		ChunkSize: s.Config.DownloadChunk,
	})
	if err != nil {
		return mapErr(err)
	}
	defer f.Close()
	hash := sha256.New()
	buf := make([]byte, s.Config.DownloadChunk)
	var offset uint64
	st, err := f.Stat()
	if err != nil {
		log.Printf("Session %v: Error stating file %v during download: %v", req.GetSessionId(), req.GetFilename(), err)
		return mapErr(fmt.Errorf("unable to stat file to download"))
	}
	log.Printf("Session %v: Starting download for %v of size %v bytes.", req.GetSessionId(), req.GetFilename(), st.Size())
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			_, err = hash.Write(data)
			if err != nil {
				log.Printf("Error: Session %v: Download for %v, unable to digest chunk: %v", req.GetSessionId(), req.GetFilename(), err)
				return status.Error(codes.Internal, "error digesting chunk")
			}
			final := errors.Is(readErr, io.EOF)
			chunk := &agentpb.DownloadFileChunk{Offset: offset, Data: data, Final: final}
			if final {
				chunk.Sha256Digest = hex.EncodeToString(hash.Sum(nil))
			}
			if err := stream.Send(&agentpb.DownloadResponse{SessionId: snap.ID, Response: &agentpb.DownloadResponse_Chunk{Chunk: chunk}}); err != nil {
				return mapErr(err)
			}
			offset += uint64(n)
			if final {
				return nil
			}
		}
		// Empty file.
		if errors.Is(readErr, io.EOF) && offset == 0 {
			chunk := &agentpb.DownloadFileChunk{Final: true, Sha256Digest: hex.EncodeToString(hash.Sum(nil))}
			return stream.Send(&agentpb.DownloadResponse{SessionId: snap.ID, Response: &agentpb.DownloadResponse_Chunk{Chunk: chunk}})
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			log.Printf("Error: Session %v: Download for %v, unable to read file: %v", req.GetSessionId(), req.GetFilename(), err)
			return status.Error(codes.Internal, "read download file")
		}
	}
}

// GetPathMetadata returns metadata for a file or immediate directory children.
//
// req.SessionId must be valid and active. req.Path may be "." or empty for the
// workspace root, otherwise it must be a safe existing relative path. The
// returned response contains file metadata or sorted directory entries. Session,
// path, and filesystem errors are mapped to gRPC status errors.
func (s *Server) GetPathMetadata(_ context.Context, req *agentpb.GetPathMetadataRequest) (*agentpb.GetPathMetadataResponse, error) {
	lease, err := s.Session.Acquire(req.GetSessionId())
	if err != nil {
		return nil, mapErr(err)
	}
	defer lease.Done()
	snap, err := lease.Session()
	if err != nil {
		return nil, mapErr(err)
	}
	resp, err := transfer.Metadata(snap.Workspace, req.GetPath())
	if err != nil {
		return nil, mapErr(err)
	}
	return resp, nil
}

// Execute runs one validated command in the session workspace.
//
// req.SessionId must be valid and active, req.Cmd must contain a non-empty
// executable, and environment variables must meet service limits. The command is
// passed directly to Runner without shell interpretation. A normal process exit,
// including non-zero exit code, returns ExecuteResponse with stdout and stderr.
// Setup, timeout, cancellation, and output-limit failures are returned as gRPC
// status errors.
func (s *Server) Execute(ctx context.Context, req *agentpb.ExecuteRequest) (*agentpb.ExecuteResponse, error) {
	lease, err := s.Session.Acquire(req.GetSessionId())
	if err != nil {
		return nil, mapErr(err)
	}
	defer lease.Done()
	snap, err := lease.Session()
	if err != nil {
		return nil, mapErr(err)
	}
	if err := validateExec(req); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	env := make([]runner.Env, 0, len(req.GetExecEnv()))
	for _, item := range req.GetExecEnv() {
		env = append(env, runner.Env{Key: item.GetKey(), Value: item.GetValue()})
	}
	res, err := s.Runner.Run(ctx, runner.Request{
		SessionID:      snap.ID,
		Workspace:      snap.Workspace,
		Cmd:            req.GetCmd(),
		Env:            env,
		Timeout:        s.Config.CommandTimeout,
		StdoutCap:      s.Config.StdoutCap,
		StderrCap:      s.Config.StderrCap,
		ContainerImage: s.Config.DockerImage,
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return &agentpb.ExecuteResponse{ExitCode: int32(res.ExitCode), Stdout: res.Stdout, Stderr: res.Stderr}, nil
}

func validateExec(req *agentpb.ExecuteRequest) error {
	if len(req.GetCmd()) == 0 || req.GetCmd()[0] == "" {
		return errors.New("command must be non-empty")
	}
	var argBytes int
	for _, arg := range req.GetCmd() {
		argBytes += len(arg)
	}
	if len(req.GetCmd()) > 256 || argBytes > 128<<10 {
		return errors.New("command arguments exceed limit")
	}
	var envBytes int
	for _, env := range req.GetExecEnv() {
		if env.GetKey() == "" || strings.Contains(env.GetKey(), "=") {
			return errors.New("invalid environment key")
		}
		envBytes += len(env.GetKey()) + len(env.GetValue())
	}
	if len(req.GetExecEnv()) > 256 || envBytes > 128<<10 {
		return errors.New("environment exceeds limit")
	}
	return nil
}

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	switch {
	case errors.Is(err, session.ErrInvalidID), errors.Is(err, pathutil.ErrInvalidPath), errors.Is(err, transfer.ErrDigestMismatch):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, session.ErrAlreadyExists):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, session.ErrNotFound), os.IsNotExist(err):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, session.ErrCompleting), errors.Is(err, transfer.ErrNotRegular):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, transfer.ErrTooLarge), errors.Is(err, runner.ErrOutputLimit):
		return status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, runner.ErrTimeout):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, runner.ErrCancellation):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, runner.ErrDocker):
		return status.Error(codes.Unavailable, err.Error())
	default:
		return status.Error(codes.Internal, fmt.Sprintf("%v", err))
	}
}
