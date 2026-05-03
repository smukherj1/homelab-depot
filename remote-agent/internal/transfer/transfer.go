package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/smukherj/homelab-depot/remote-agent/gen/go/proto"
	"github.com/smukherj/homelab-depot/remote-agent/internal/pathutil"
)

var (
	// ErrTooLarge reports that a transfer target exceeds the configured byte
	// limit for upload or download.
	ErrTooLarge = errors.New("file exceeds configured size limit")
	// ErrDigestMismatch reports that uploaded or downloaded bytes do not match
	// the caller-supplied SHA-256 digest.
	ErrDigestMismatch = errors.New("sha256 digest mismatch")
	// ErrNotRegular reports that a download target exists but is not a regular
	// file.
	ErrNotRegular = errors.New("path is not a regular file")
)

// UploadOptions describes a single file upload commit.
//
// Callers must provide a session workspace, caller-supplied relative filename,
// safe permission mode, and positive maximum size. Functions using
// UploadOptions validate Filename and Mode before mutating the filesystem.
type UploadOptions struct {
	// Workspace is the session root directory where the file will be created.
	Workspace string
	// Filename is the API-supplied relative destination path inside Workspace.
	Filename string
	// Mode is the Unix permission mode to apply after content and digest checks
	// pass.
	Mode uint32
	// MaxSize is the maximum accepted upload size in bytes.
	MaxSize int64
}

// DownloadOptions describes a single file download.
//
// Callers must provide a session workspace, caller-supplied relative filename,
// and positive size/chunk limits. OpenDownload validates Filename and enforces
// MaxSize before returning a readable file.
type DownloadOptions struct {
	// Workspace is the session root directory containing the file.
	Workspace string
	// Filename is the API-supplied relative source path inside Workspace.
	Filename string
	// MaxSize is the maximum regular file size in bytes that can be opened.
	MaxSize int64
	// ChunkSize is the intended response chunk size used by callers streaming
	// the file; OpenDownload stores it for option completeness but does not read
	// chunks itself.
	ChunkSize int
}

// ValidateUpload validates upload path and mode and returns the destination
// host path.
//
// opts.Workspace and opts.Filename must identify a path that can be created
// without following the destination file as a symlink. It returns the resolved
// destination path on success, or a mode, path, or filesystem-check error.
func ValidateUpload(opts UploadOptions) (string, error) {
	if err := pathutil.SafeMode(opts.Mode); err != nil {
		return "", err
	}
	return pathutil.ResolveForCreate(opts.Workspace, opts.Filename)
}

// CommitUpload atomically writes reader contents to the requested upload path.
//
// opts is validated before mutation. reader is consumed until EOF or
// opts.MaxSize+1 bytes, digest must be the expected hex SHA-256 of the content,
// and the final file is chmodded to opts.Mode before rename. The returned count
// is the number of bytes read from reader, including the byte that proves
// ErrTooLarge. On validation, I/O, size, or digest errors the destination is not
// replaced and the temporary file is removed.
func CommitUpload(opts UploadOptions, reader io.Reader, digest string) (uint64, error) {
	target, err := ValidateUpload(opts)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".upload-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpName)
		}
	}()

	hash := sha256.New()
	limited := io.LimitReader(reader, opts.MaxSize+1)
	n, err := io.Copy(io.MultiWriter(tmp, hash), limited)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return uint64(n), err
	}
	if n > opts.MaxSize {
		return uint64(n), ErrTooLarge
	}
	if got := hex.EncodeToString(hash.Sum(nil)); !strings.EqualFold(got, digest) {
		return uint64(n), ErrDigestMismatch
	}
	if err := os.Chmod(tmpName, os.FileMode(opts.Mode)); err != nil {
		return uint64(n), err
	}
	if err := os.Rename(tmpName, target); err != nil {
		return uint64(n), err
	}
	removeTmp = false
	return uint64(n), nil
}

// OpenDownload opens a validated regular file for streaming to a client.
//
// opts.Workspace and opts.Filename must identify an existing path inside the
// workspace. Symlinks are resolved and rejected if they escape. The returned
// file is open for reading and must be closed by the caller; the FileInfo
// describes the same target. Errors include path validation failures,
// ErrNotRegular, ErrTooLarge, and filesystem errors.
func OpenDownload(opts DownloadOptions) (*os.File, os.FileInfo, error) {
	target, err := pathutil.ResolveExisting(opts.Workspace, opts.Filename)
	if err != nil {
		return nil, nil, err
	}
	info, err := os.Stat(target)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, ErrNotRegular
	}
	if info.Size() > opts.MaxSize {
		return nil, nil, ErrTooLarge
	}
	f, err := os.Open(target)
	if err != nil {
		return nil, nil, err
	}
	return f, info, nil
}

// Metadata returns protobuf metadata for a file or one directory level.
//
// workspace is the session root. raw may be "." or empty to inspect the
// workspace itself; otherwise it must be a valid existing relative path. For a
// file it returns FileMetadata for the requested path. For a directory it
// returns immediate entries sorted by name. Errors come from path validation,
// symlink containment checks, lstat, or directory reads.
func Metadata(workspace, raw string) (*agentpb.GetPathMetadataResponse, error) {
	var target string
	var err error
	if raw == "." || raw == "" {
		target = workspace
	} else {
		target, err = pathutil.ResolveExisting(workspace, raw)
		if err != nil {
			return nil, err
		}
	}
	info, err := os.Lstat(target)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		entries, err := os.ReadDir(target)
		if err != nil {
			return nil, err
		}
		out := make([]*agentpb.PathMetadata, 0, len(entries))
		for _, entry := range entries {
			child := filepath.Join(target, entry.Name())
			childInfo, err := os.Lstat(child)
			if err != nil {
				return nil, err
			}
			out = append(out, toProto(entry.Name(), childInfo))
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return &agentpb.GetPathMetadataResponse{
			Response: &agentpb.GetPathMetadataResponse_DirectoryMetadata{
				DirectoryMetadata: &agentpb.DirectoryMetadata{Entries: out},
			},
		}, nil
	}
	name := filepath.Base(target)
	if target == workspace {
		name = "."
	}
	return &agentpb.GetPathMetadataResponse{
		Response: &agentpb.GetPathMetadataResponse_FileMetadata{FileMetadata: toProto(name, info)},
	}, nil
}

func toProto(name string, info os.FileInfo) *agentpb.PathMetadata {
	return &agentpb.PathMetadata{
		Name:             name,
		Type:             fileType(info),
		Size:             uint64(info.Size()),
		Mode:             uint32(info.Mode()),
		MtimeUnixSeconds: info.ModTime().Unix(),
		MtimeNanos:       int32(info.ModTime().Nanosecond()),
	}
}

func fileType(info os.FileInfo) agentpb.FileType {
	mode := info.Mode()
	switch {
	case mode.IsRegular():
		return agentpb.FileType_FILE_TYPE_REGULAR
	case mode.IsDir():
		return agentpb.FileType_FILE_TYPE_DIRECTORY
	case mode&os.ModeSymlink != 0:
		return agentpb.FileType_FILE_TYPE_SYMLINK
	default:
		return agentpb.FileType_FILE_TYPE_OTHER
	}
}
