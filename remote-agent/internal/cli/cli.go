package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/smukherj/homelab-depot/remote-agent/gen/go/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Run executes the agentctl command-line client.
//
// ctx controls dialing and RPC lifetime. args must not include argv[0]. stdout
// and stderr must be non-nil writers used for command output and diagnostics.
// Run connects to the configured gRPC address, performs the requested command,
// streams upload/download data when needed, and returns a process-style exit
// code: 0 for success, 1 for runtime/RPC failures, 2 for usage errors, or the
// remote command exit code for "exec".
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agentctl", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "127.0.0.1:50051", "agent address")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(stderr, "command required")
		return 2
	}
	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer conn.Close()
	client := agentpb.NewAgentClient(conn)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	switch rest[0] {
	case "status":
		resp, err := client.GetStatus(ctx, &agentpb.GetStatusRequest{})
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "active_sessions=%d\n", resp.GetActiveSessions())
	case "create-session":
		if len(rest) != 2 {
			return usage(stderr, "create-session SESSION_ID")
		}
		_, err := client.CreateSession(ctx, &agentpb.CreateSessionRequest{SessionId: rest[1]})
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	case "complete-session":
		if len(rest) != 2 {
			return usage(stderr, "complete-session SESSION_ID")
		}
		_, err := client.CompleteSession(ctx, &agentpb.CompleteSessionRequest{SessionId: rest[1]})
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	case "upload":
		if len(rest) != 4 {
			return usage(stderr, "upload SESSION_ID LOCAL_PATH REMOTE_PATH")
		}
		if err := upload(ctx, client, rest[1], rest[2], rest[3]); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	case "download":
		if len(rest) != 4 {
			return usage(stderr, "download SESSION_ID REMOTE_PATH LOCAL_PATH")
		}
		if err := download(ctx, client, rest[1], rest[2], rest[3]); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	case "metadata":
		if len(rest) != 3 {
			return usage(stderr, "metadata SESSION_ID REMOTE_PATH")
		}
		resp, err := client.GetPathMetadata(ctx, &agentpb.GetPathMetadataRequest{SessionId: rest[1], Path: rest[2]})
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		printMetadata(stdout, resp)
	case "exec":
		if len(rest) < 4 || rest[2] != "--" {
			return usage(stderr, "exec SESSION_ID -- CMD [ARG...]")
		}
		resp, err := client.Execute(ctx, &agentpb.ExecuteRequest{SessionId: rest[1], Cmd: rest[3:]})
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprint(stdout, resp.GetStdout())
		fmt.Fprint(stderr, resp.GetStderr())
		return int(resp.GetExitCode())
	default:
		fmt.Fprintln(stderr, "unknown command:", rest[0])
		return 2
	}
	return 0
}

func upload(ctx context.Context, client agentpb.AgentClient, sessionID, localPath, remotePath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	stream, err := client.Upload(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&agentpb.UploadRequest{SessionId: sessionID, Request: &agentpb.UploadRequest_Header{Header: &agentpb.UploadFileHeader{Filename: remotePath, Permissions: uint32(info.Mode().Perm())}}}); err != nil {
		return err
	}
	hash := sha256.New()
	buf := make([]byte, 1024*1024)
	var offset uint64
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			_, _ = hash.Write(data)
			chunk := &agentpb.UploadFileChunk{Offset: offset, Data: data}
			if err := stream.Send(&agentpb.UploadRequest{SessionId: sessionID, Request: &agentpb.UploadRequest_Chunk{Chunk: chunk}}); err != nil {
				return err
			}
			offset += uint64(n)
		}
		if errors.Is(readErr, io.EOF) {
			chunk := &agentpb.UploadFileChunk{Offset: offset, Final: true, Sha256Digest: hex.EncodeToString(hash.Sum(nil))}
			if err := stream.Send(&agentpb.UploadRequest{SessionId: sessionID, Request: &agentpb.UploadRequest_Chunk{Chunk: chunk}}); err != nil {
				return err
			}
			_, err := stream.CloseAndRecv()
			return err
		}
		if readErr != nil {
			return readErr
		}
	}
}

func download(ctx context.Context, client agentpb.AgentClient, sessionID, remotePath, localPath string) error {
	stream, err := client.Download(ctx, &agentpb.DownloadRequest{SessionId: sessionID, Filename: remotePath})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil && filepath.Dir(localPath) != "." {
		return err
	}
	f, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	hash := sha256.New()
	var offset uint64
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		chunk := resp.GetChunk()
		if chunk == nil {
			return errors.New("missing download chunk")
		}
		if chunk.GetOffset() != offset {
			return errors.New("unexpected download offset")
		}
		if len(chunk.GetData()) > 0 {
			if _, err := f.Write(chunk.GetData()); err != nil {
				return err
			}
			_, _ = hash.Write(chunk.GetData())
			offset += uint64(len(chunk.GetData()))
		}
		if chunk.GetFinal() {
			got := hex.EncodeToString(hash.Sum(nil))
			if !strings.EqualFold(got, chunk.GetSha256Digest()) {
				return errors.New("download sha256 digest mismatch")
			}
			return nil
		}
	}
}

func printMetadata(w io.Writer, resp *agentpb.GetPathMetadataResponse) {
	if file := resp.GetFileMetadata(); file != nil {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", file.GetName(), file.GetType(), file.GetSize(), strconv.FormatUint(uint64(file.GetMode()), 8))
		return
	}
	for _, entry := range resp.GetDirectoryMetadata().GetEntries() {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", entry.GetName(), entry.GetType(), entry.GetSize(), strconv.FormatUint(uint64(entry.GetMode()), 8))
	}
}

func usage(w io.Writer, msg string) int {
	fmt.Fprintln(w, "usage:", msg)
	return 2
}
