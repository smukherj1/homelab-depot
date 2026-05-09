package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultRestartDelay                   = 5 * time.Second
	defaultGracefulShutdownTimeout        = 30 * time.Second
	defaultProcessWorkingDirectory        = "./process"
	defaultLogsDirectory                  = "./logs"
	defaultMaxEntrySize                   = 16 * 1024
	defaultStreamDiskBudget               = 128 * 1024 * 1024
	defaultEventsRetainedCount            = 1024
	minRestartDelay                       = 100 * time.Millisecond
	maxRestartDelay                       = time.Hour
	minGracefulShutdownTimeout            = 0 * time.Second
	maxGracefulShutdownTimeout            = 10 * time.Minute
	minMaxEntrySize                uint64 = 1024
	maxMaxEntrySize                uint64 = 1024 * 1024
	minStreamDiskBudget            uint64 = 16 * 1024 * 1024
	minEventsRetainedCount         uint64 = 1
	maxEventsRetainedCount         uint64 = 65536
)

var sizePattern = regexp.MustCompile(`^([0-9]+)(B|KiB|MiB|GiB)$`)

// EncodingMode identifies the accepted child log encoding mode.
type EncodingMode string

const (
	// EncodingPlainText selects newline-delimited plain text child logs.
	EncodingPlainText EncodingMode = "plain_text"
	// EncodingStructured selects newline-delimited structured child logs.
	EncodingStructured EncodingMode = "structured"
)

// StructuredFormat identifies the supported structured child log format.
type StructuredFormat string

const (
	// StructuredFormatSlogJSON selects Go slog JSONHandler-compatible records.
	StructuredFormatSlogJSON StructuredFormat = "slog_json"
)

// Config is the fully defaulted and validated runner configuration.
type Config struct {
	Server  ServerConfig
	Process ProcessConfig
	Logs    LogsConfig
	Events  EventsConfig
}

// ServerConfig contains gRPC listener configuration.
type ServerConfig struct {
	ListenAddress string
}

// ProcessConfig contains the supervised child process configuration.
type ProcessConfig struct {
	Command                 []string
	RestartDelay            time.Duration
	GracefulShutdownTimeout time.Duration
	WorkingDirectory        string
}

// LogsConfig contains child log retention, parsing, and storage configuration.
type LogsConfig struct {
	Directory    string
	MaxEntrySize uint64
	Stdout       LogStreamConfig
	Stderr       LogStreamConfig
	Encoding     LogEncodingConfig
}

// LogStreamConfig contains retention settings for one child output stream.
type LogStreamConfig struct {
	DiskBudget uint64
}

// LogEncodingConfig contains the selected child log encoding.
type LogEncodingConfig struct {
	Mode             EncodingMode
	StructuredFormat StructuredFormat
}

// EventsConfig contains in-memory runner event retention configuration.
type EventsConfig struct {
	RetainedCount uint64
}

type rawConfig struct {
	Server  rawServerConfig  `yaml:"server"`
	Process rawProcessConfig `yaml:"process"`
	Logs    rawLogsConfig    `yaml:"logs"`
	Events  rawEventsConfig  `yaml:"events"`
}

type rawServerConfig struct {
	ListenAddress *string `yaml:"listen_address"`
}

type rawProcessConfig struct {
	Command                 []string `yaml:"command"`
	RestartDelay            *string  `yaml:"restart_delay"`
	GracefulShutdownTimeout *string  `yaml:"graceful_shutdown_timeout"`
	WorkingDirectory        *string  `yaml:"working_directory"`
}

type rawLogsConfig struct {
	Directory    *string            `yaml:"directory"`
	MaxEntrySize *string            `yaml:"max_entry_size"`
	Stdout       rawLogStreamConfig `yaml:"stdout"`
	Stderr       rawLogStreamConfig `yaml:"stderr"`
	Encoding     rawLogEncoding     `yaml:"encoding"`
}

type rawLogStreamConfig struct {
	DiskBudget *string `yaml:"disk_budget"`
}

type rawLogEncoding struct {
	PlainText  *struct{}          `yaml:"plain_text"`
	Structured *rawStructuredLogs `yaml:"structured"`
}

type rawStructuredLogs struct {
	Format *string `yaml:"format"`
}

type rawEventsConfig struct {
	RetainedCount *uint64 `yaml:"retained_count"`
}

// LoadFile reads a YAML configuration file, applies defaults, resolves relative
// process and log directories against the current working directory, creates
// those directories when needed, checks writability, and validates all fields
// before returning the resulting Config.
func LoadFile(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config file %q: %w", path, err)
	}

	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return Config{}, fmt.Errorf("parse config file %q: %w", path, err)
	}

	wd, err := os.Getwd()
	if err != nil {
		return Config{}, fmt.Errorf("get working directory: %w", err)
	}

	cfg, err := buildConfig(raw, wd)
	if err != nil {
		return Config{}, err
	}
	if err := ensureDirectoryWritable(cfg.Process.WorkingDirectory, "process.working_directory"); err != nil {
		return Config{}, err
	}
	if err := ensureDirectoryWritable(cfg.Logs.Directory, "logs.directory"); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// ParseSize parses a binary size string using the accepted B, KiB, MiB, and GiB
// units. Amounts must be non-negative base-10 integers with no spaces,
// fractions, decimal units, or lowercase units.
func ParseSize(value string) (uint64, error) {
	matches := sizePattern.FindStringSubmatch(value)
	if matches == nil {
		return 0, fmt.Errorf("invalid size %q: use a non-negative integer followed by B, KiB, MiB, or GiB", value)
	}
	amount, err := strconv.ParseUint(matches[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse size amount %q: %w", matches[1], err)
	}

	var multiplier uint64
	switch matches[2] {
	case "B":
		multiplier = 1
	case "KiB":
		multiplier = 1024
	case "MiB":
		multiplier = 1024 * 1024
	case "GiB":
		multiplier = 1024 * 1024 * 1024
	default:
		return 0, fmt.Errorf("unsupported size unit %q", matches[2])
	}
	if amount > ^uint64(0)/multiplier {
		return 0, fmt.Errorf("size %q overflows uint64", value)
	}
	return amount * multiplier, nil
}

// ParseDuration parses a duration string using Go's time.ParseDuration syntax.
// Bare integers and human-word durations are rejected by the underlying parser.
func ParseDuration(value string) (time.Duration, error) {
	return time.ParseDuration(value)
}

func buildConfig(raw rawConfig, wd string) (Config, error) {
	cfg := Config{
		Process: ProcessConfig{
			RestartDelay:            defaultRestartDelay,
			GracefulShutdownTimeout: defaultGracefulShutdownTimeout,
			WorkingDirectory:        defaultProcessWorkingDirectory,
		},
		Logs: LogsConfig{
			Directory:    defaultLogsDirectory,
			MaxEntrySize: defaultMaxEntrySize,
			Stdout:       LogStreamConfig{DiskBudget: defaultStreamDiskBudget},
			Stderr:       LogStreamConfig{DiskBudget: defaultStreamDiskBudget},
			Encoding:     LogEncodingConfig{Mode: EncodingPlainText},
		},
		Events: EventsConfig{RetainedCount: defaultEventsRetainedCount},
	}

	if raw.Server.ListenAddress == nil || *raw.Server.ListenAddress == "" {
		return Config{}, errors.New("server.listen_address is required")
	}
	cfg.Server.ListenAddress = *raw.Server.ListenAddress

	if len(raw.Process.Command) == 0 {
		return Config{}, errors.New("process.command is required")
	}
	cfg.Process.Command = append([]string(nil), raw.Process.Command...)
	for i, arg := range cfg.Process.Command {
		if arg == "" {
			return Config{}, fmt.Errorf("process.command[%d] must not be empty", i)
		}
	}

	if raw.Process.RestartDelay != nil {
		d, err := parseNamedDuration("process.restart_delay", *raw.Process.RestartDelay)
		if err != nil {
			return Config{}, err
		}
		cfg.Process.RestartDelay = d
	}
	if raw.Process.GracefulShutdownTimeout != nil {
		d, err := parseNamedDuration("process.graceful_shutdown_timeout", *raw.Process.GracefulShutdownTimeout)
		if err != nil {
			return Config{}, err
		}
		cfg.Process.GracefulShutdownTimeout = d
	}
	if raw.Process.WorkingDirectory != nil {
		if *raw.Process.WorkingDirectory == "" {
			return Config{}, errors.New("process.working_directory must not be empty")
		}
		cfg.Process.WorkingDirectory = *raw.Process.WorkingDirectory
	}
	if raw.Logs.Directory != nil {
		if *raw.Logs.Directory == "" {
			return Config{}, errors.New("logs.directory must not be empty")
		}
		cfg.Logs.Directory = *raw.Logs.Directory
	}
	if raw.Logs.MaxEntrySize != nil {
		size, err := parseNamedSize("logs.max_entry_size", *raw.Logs.MaxEntrySize)
		if err != nil {
			return Config{}, err
		}
		cfg.Logs.MaxEntrySize = size
	}
	if raw.Logs.Stdout.DiskBudget != nil {
		size, err := parseNamedSize("logs.stdout.disk_budget", *raw.Logs.Stdout.DiskBudget)
		if err != nil {
			return Config{}, err
		}
		cfg.Logs.Stdout.DiskBudget = size
	}
	if raw.Logs.Stderr.DiskBudget != nil {
		size, err := parseNamedSize("logs.stderr.disk_budget", *raw.Logs.Stderr.DiskBudget)
		if err != nil {
			return Config{}, err
		}
		cfg.Logs.Stderr.DiskBudget = size
	}
	if raw.Events.RetainedCount != nil {
		cfg.Events.RetainedCount = *raw.Events.RetainedCount
	}
	encoding, err := parseEncoding(raw.Logs.Encoding)
	if err != nil {
		return Config{}, err
	}
	if encoding.Mode != "" {
		cfg.Logs.Encoding = encoding
	}

	cfg.Process.WorkingDirectory = resolvePath(wd, cfg.Process.WorkingDirectory)
	cfg.Logs.Directory = resolvePath(wd, cfg.Logs.Directory)
	if err := validateConfig(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func parseNamedDuration(field, value string) (time.Duration, error) {
	d, err := ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s invalid duration %q: %w", field, value, err)
	}
	return d, nil
}

func parseNamedSize(field, value string) (uint64, error) {
	size, err := ParseSize(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}
	return size, nil
}

func parseEncoding(raw rawLogEncoding) (LogEncodingConfig, error) {
	if raw.PlainText == nil && raw.Structured == nil {
		return LogEncodingConfig{}, nil
	}
	if raw.PlainText != nil && raw.Structured != nil {
		return LogEncodingConfig{}, errors.New("logs.encoding must select only one of plain_text or structured")
	}
	if raw.PlainText != nil {
		return LogEncodingConfig{Mode: EncodingPlainText}, nil
	}
	if raw.Structured.Format == nil || *raw.Structured.Format == "" {
		return LogEncodingConfig{}, errors.New("logs.encoding.structured.format is required")
	}
	if *raw.Structured.Format != string(StructuredFormatSlogJSON) {
		return LogEncodingConfig{}, fmt.Errorf("logs.encoding.structured.format %q is not supported", *raw.Structured.Format)
	}
	return LogEncodingConfig{Mode: EncodingStructured, StructuredFormat: StructuredFormatSlogJSON}, nil
}

func validateConfig(cfg Config) error {
	if cfg.Process.RestartDelay < minRestartDelay || cfg.Process.RestartDelay > maxRestartDelay {
		return fmt.Errorf("process.restart_delay must be between %s and %s inclusive", minRestartDelay, maxRestartDelay)
	}
	if cfg.Process.GracefulShutdownTimeout < minGracefulShutdownTimeout || cfg.Process.GracefulShutdownTimeout > maxGracefulShutdownTimeout {
		return fmt.Errorf("process.graceful_shutdown_timeout must be between %s and %s inclusive", minGracefulShutdownTimeout, maxGracefulShutdownTimeout)
	}
	if cfg.Logs.MaxEntrySize < minMaxEntrySize || cfg.Logs.MaxEntrySize > maxMaxEntrySize {
		return fmt.Errorf("logs.max_entry_size must be between %dB and %dB inclusive", minMaxEntrySize, maxMaxEntrySize)
	}
	if cfg.Logs.Stdout.DiskBudget < minStreamDiskBudget {
		return fmt.Errorf("logs.stdout.disk_budget must be at least %dB", minStreamDiskBudget)
	}
	if cfg.Logs.Stderr.DiskBudget < minStreamDiskBudget {
		return fmt.Errorf("logs.stderr.disk_budget must be at least %dB", minStreamDiskBudget)
	}
	if cfg.Events.RetainedCount < minEventsRetainedCount || cfg.Events.RetainedCount > maxEventsRetainedCount {
		return fmt.Errorf("events.retained_count must be between %d and %d inclusive", minEventsRetainedCount, maxEventsRetainedCount)
	}
	if err := validateSeparateDirectories(cfg.Process.WorkingDirectory, cfg.Logs.Directory); err != nil {
		return err
	}
	return nil
}

func validateSeparateDirectories(processDir, logsDir string) error {
	processClean := filepath.Clean(processDir)
	logsClean := filepath.Clean(logsDir)
	if processClean == logsClean {
		return errors.New("process.working_directory and logs.directory must be separate directories")
	}
	processRelToLogs, err := filepath.Rel(logsClean, processClean)
	if err != nil {
		return fmt.Errorf("compare process.working_directory and logs.directory: %w", err)
	}
	if isSubPath(processRelToLogs) {
		return errors.New("process.working_directory must not be inside logs.directory")
	}
	logsRelToProcess, err := filepath.Rel(processClean, logsClean)
	if err != nil {
		return fmt.Errorf("compare logs.directory and process.working_directory: %w", err)
	}
	if isSubPath(logsRelToProcess) {
		return errors.New("logs.directory must not be inside process.working_directory")
	}
	return nil
}

func isSubPath(rel string) bool {
	return rel != "." && rel != ".." && !filepath.IsAbs(rel) && rel != "" && !startsWithParent(rel)
}

func startsWithParent(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func resolvePath(wd, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(wd, path))
}

func ensureDirectoryWritable(path, field string) error {
	info, err := os.Stat(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s stat %q: %w", field, path, err)
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			return fmt.Errorf("%s create %q: %w", field, path, err)
		}
		info, err = os.Stat(path)
		if err != nil {
			return fmt.Errorf("%s stat created directory %q: %w", field, path, err)
		}
	}
	if !info.IsDir() {
		return fmt.Errorf("%s %q is not a directory", field, path)
	}
	probe, err := os.CreateTemp(path, ".runner-config-write-test-*")
	if err != nil {
		return fmt.Errorf("%s %q is not writable: %w", field, path, err)
	}
	name := probe.Name()
	if err := probe.Close(); err != nil {
		return fmt.Errorf("%s close writable probe %q: %w", field, name, err)
	}
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("%s remove writable probe %q: %w", field, name, err)
	}
	return nil
}
