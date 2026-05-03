package config

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

// Config contains all process-level settings used by the agent.
//
// Callers pass Config by value after loading it from defaults, environment, and
// flags. Validate must succeed before the server starts. Duration fields must be
// positive, size fields are byte counts unless otherwise noted, and no method on
// Config is concurrency-sensitive because the service treats it as immutable.
type Config struct {
	// ListenAddr is the host:port address the gRPC server listens on. It must
	// include both host and port and is validated by ValidateListenAddr.
	ListenAddr string
	// WorkspaceRoot is the host directory under which session workspaces are
	// created. An empty value tells the session manager to create a private
	// temporary root.
	WorkspaceRoot string
	// SessionIdle is the maximum duration since last session activity before the
	// janitor may expire an inactive session.
	SessionIdle time.Duration
	// MaxUploadSize is the maximum complete file size accepted by upload RPCs.
	MaxUploadSize int64
	// MaxUploadChunk is the maximum data payload accepted in a single upload
	// chunk. It must be less than or equal to MaxUploadSize.
	MaxUploadChunk int64
	// MaxDownloadSize is the maximum regular file size that can be downloaded.
	MaxDownloadSize int64
	// DownloadChunk is the maximum data payload sent in each download response.
	DownloadChunk int
	// CommandTimeout is the maximum wall-clock duration for a command execution.
	CommandTimeout time.Duration
	// StdoutCap is the maximum number of stdout bytes captured from a command.
	StdoutCap int64
	// StderrCap is the maximum number of stderr bytes captured from a command.
	StderrCap int64
	// DockerImage is the container image used by Docker-backed executions.
	DockerImage string
	// JanitorInterval is the interval between idle-session cleanup scans.
	JanitorInterval time.Duration
}

// Defaults returns the built-in configuration used when no environment
// variable or flag overrides a setting.
//
// The returned Config is independent and may be mutated by the caller before
// validation. It performs no I/O and returns values that should pass Validate.
func Defaults() Config {
	return Config{
		ListenAddr:      "127.0.0.1:50051",
		WorkspaceRoot:   "",
		SessionIdle:     5 * time.Minute,
		MaxUploadSize:   256 << 20,
		MaxUploadChunk:  3 << 20,
		MaxDownloadSize: 256 << 20,
		DownloadChunk:   3 << 20,
		CommandTimeout:  5 * time.Minute,
		StdoutCap:       10 << 20,
		StderrCap:       10 << 20,
		DockerImage:     "ubuntu:26.04",
		JanitorInterval: 30 * time.Second,
	}
}

// FromEnv returns a Config populated from Defaults and supported
// REMOTE_AGENT_* environment variables.
//
// Invalid duration or integer environment values are ignored in favor of their
// fallback defaults, so callers must still call Validate to catch semantic
// errors such as negative limits. The function reads process environment only
// and has no other side effects.
func FromEnv() Config {
	cfg := Defaults()
	cfg.ListenAddr = envString("REMOTE_AGENT_LISTEN_ADDR", cfg.ListenAddr)
	cfg.WorkspaceRoot = envString("REMOTE_AGENT_WORKSPACE_ROOT", cfg.WorkspaceRoot)
	cfg.SessionIdle = envDuration("REMOTE_AGENT_SESSION_IDLE", cfg.SessionIdle)
	cfg.MaxUploadSize = envInt64("REMOTE_AGENT_MAX_UPLOAD_SIZE", cfg.MaxUploadSize)
	cfg.MaxUploadChunk = envInt64("REMOTE_AGENT_MAX_UPLOAD_CHUNK", cfg.MaxUploadChunk)
	cfg.MaxDownloadSize = envInt64("REMOTE_AGENT_MAX_DOWNLOAD_SIZE", cfg.MaxDownloadSize)
	cfg.DownloadChunk = int(envInt64("REMOTE_AGENT_DOWNLOAD_CHUNK", int64(cfg.DownloadChunk)))
	cfg.CommandTimeout = envDuration("REMOTE_AGENT_COMMAND_TIMEOUT", cfg.CommandTimeout)
	cfg.StdoutCap = envInt64("REMOTE_AGENT_STDOUT_CAP", cfg.StdoutCap)
	cfg.StderrCap = envInt64("REMOTE_AGENT_STDERR_CAP", cfg.StderrCap)
	cfg.DockerImage = envString("REMOTE_AGENT_DOCKER_IMAGE", cfg.DockerImage)
	cfg.JanitorInterval = envDuration("REMOTE_AGENT_JANITOR_INTERVAL", cfg.JanitorInterval)
	return cfg
}

// RegisterFlags binds agent configuration flags to cfg.
//
// fs and cfg must be non-nil. The flag defaults come from the values already in
// cfg, so callers should apply Defaults and environment values before calling
// RegisterFlags. Parsed flags mutate cfg through the registered flag variables;
// this function itself returns no value and reports duplicate-flag panics using
// the standard flag package behavior.
func RegisterFlags(fs *flag.FlagSet, cfg *Config) {
	fs.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "loopback listen address")
	fs.StringVar(&cfg.WorkspaceRoot, "workspace-root", cfg.WorkspaceRoot, "workspace root")
	fs.DurationVar(&cfg.SessionIdle, "session-idle", cfg.SessionIdle, "session idle timeout")
	fs.Int64Var(&cfg.MaxUploadSize, "max-upload-size", cfg.MaxUploadSize, "max upload file size")
	fs.Int64Var(&cfg.MaxUploadChunk, "max-upload-chunk", cfg.MaxUploadChunk, "max upload chunk size")
	fs.Int64Var(&cfg.MaxDownloadSize, "max-download-size", cfg.MaxDownloadSize, "max download file size")
	fs.IntVar(&cfg.DownloadChunk, "download-chunk", cfg.DownloadChunk, "download chunk size")
	fs.DurationVar(&cfg.CommandTimeout, "command-timeout", cfg.CommandTimeout, "command timeout")
	fs.Int64Var(&cfg.StdoutCap, "stdout-cap", cfg.StdoutCap, "stdout cap")
	fs.Int64Var(&cfg.StderrCap, "stderr-cap", cfg.StderrCap, "stderr cap")
	fs.StringVar(&cfg.DockerImage, "docker-image", cfg.DockerImage, "Docker image")
	fs.DurationVar(&cfg.JanitorInterval, "janitor-interval", cfg.JanitorInterval, "janitor interval")
}

// Validate checks that cfg is internally consistent and usable by the server.
//
// The caller does not need to hold any locks because Config is value data. It
// returns nil when all settings are acceptable, or an error describing the first
// invalid address, timeout, size limit, image, or janitor interval found.
func Validate(cfg Config) error {
	if err := ValidateListenAddr(cfg.ListenAddr); err != nil {
		return err
	}
	if cfg.SessionIdle <= 0 {
		return errors.New("session idle timeout must be positive")
	}
	if cfg.MaxUploadSize <= 0 || cfg.MaxUploadChunk <= 0 || cfg.MaxUploadChunk > cfg.MaxUploadSize {
		return errors.New("invalid upload size limits")
	}
	if cfg.MaxDownloadSize <= 0 || cfg.DownloadChunk <= 0 || int64(cfg.DownloadChunk) > cfg.MaxDownloadSize {
		return errors.New("invalid download size limits")
	}
	if cfg.CommandTimeout <= 0 || cfg.StdoutCap <= 0 || cfg.StderrCap <= 0 {
		return errors.New("command limits must be positive")
	}
	if cfg.DockerImage == "" {
		return errors.New("docker image must be set")
	}
	if cfg.JanitorInterval <= 0 {
		return errors.New("janitor interval must be positive")
	}
	return nil
}

// ValidateListenAddr checks that addr is a syntactically valid host:port pair.
//
// addr must include a non-empty host and port. The returned error describes
// malformed input; nil means net.SplitHostPort accepted the address and both
// components were present. This function does not resolve hostnames or open a
// listener.
func ValidateListenAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("listen address must be host:port: %w", err)
	}
	if port == "" {
		return errors.New("listen port must be set")
	}
	if host == "" {
		return errors.New("hostname must be set")
	}
	return nil
}

func envString(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil {
			return parsed
		}
	}
	return fallback
}

func envInt64(key string, fallback int64) int64 {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
			return parsed
		}
	}
	return fallback
}
