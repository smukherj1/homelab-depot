package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLoadFileAppliesDefaultsWhenOptionalSectionsOmitted(t *testing.T) {
	dir := t.TempDir()
	cfg := loadFromString(t, dir, `
server:
  listen_address: "127.0.0.1:9090"
process:
  command: ["echo", "ok"]
`)

	if cfg.Process.RestartDelay != 5*time.Second {
		t.Errorf("RestartDelay = %s, want 5s", cfg.Process.RestartDelay)
	}
	if cfg.Process.GracefulShutdownTimeout != 30*time.Second {
		t.Errorf("GracefulShutdownTimeout = %s, want 30s", cfg.Process.GracefulShutdownTimeout)
	}
	if cfg.Logs.MaxEntrySize != 16*1024 {
		t.Errorf("MaxEntrySize = %d, want 16384", cfg.Logs.MaxEntrySize)
	}
	if cfg.Logs.Stdout.DiskBudget != 128*1024*1024 || cfg.Logs.Stderr.DiskBudget != 128*1024*1024 {
		t.Errorf("DiskBudget = stdout %d stderr %d, want both 128MiB", cfg.Logs.Stdout.DiskBudget, cfg.Logs.Stderr.DiskBudget)
	}
	if cfg.Logs.Encoding.Mode != EncodingPlainText {
		t.Errorf("Encoding.Mode = %q, want %q", cfg.Logs.Encoding.Mode, EncodingPlainText)
	}
	if cfg.Events.RetainedCount != 1024 {
		t.Errorf("RetainedCount = %d, want 1024", cfg.Events.RetainedCount)
	}
	if cfg.Process.WorkingDirectory != filepath.Join(dir, "process") {
		t.Errorf("WorkingDirectory = %q, want resolved ./process", cfg.Process.WorkingDirectory)
	}
	if cfg.Logs.Directory != filepath.Join(dir, "logs") {
		t.Errorf("Logs.Directory = %q, want resolved ./logs", cfg.Logs.Directory)
	}
}

func TestLoadFileRequiredFieldsNameMissingField(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "listen address",
			yaml: `
process:
  command: ["echo"]
`,
			wantErr: "server.listen_address",
		},
		{
			name: "command",
			yaml: `
server:
  listen_address: "127.0.0.1:9090"
`,
			wantErr: "process.command",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadFromStringErr(t, t.TempDir(), tt.yaml)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("LoadFile() error = %v, want field name %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadFileRejectsInvalidCommandArrays(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{
			name: "empty array",
			yaml: `
server:
  listen_address: "127.0.0.1:9090"
process:
  command: []
`,
		},
		{
			name: "empty element",
			yaml: `
server:
  listen_address: "127.0.0.1:9090"
process:
  command: ["echo", ""]
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadFromStringErr(t, t.TempDir(), tt.yaml)
			if err == nil || !strings.Contains(err.Error(), "process.command") {
				t.Fatalf("LoadFile() error = %v, want process.command validation error", err)
			}
		})
	}
}

func TestParseDurationRejectsNonGoSyntax(t *testing.T) {
	valid, err := ParseDuration("1m30s")
	if err != nil {
		t.Fatalf("ParseDuration(1m30s) error = %v, want nil", err)
	}
	if valid != 90*time.Second {
		t.Errorf("ParseDuration(1m30s) = %s, want 1m30s", valid)
	}

	for _, value := range []string{"5", "5 seconds"} {
		if _, err := ParseDuration(value); err == nil {
			t.Errorf("ParseDuration(%q) error = nil, want error", value)
		}
	}
}

func TestParseSizeValidExamples(t *testing.T) {
	tests := map[string]uint64{
		"0B":     0,
		"1024B":  1024,
		"16KiB":  16 * 1024,
		"128MiB": 128 * 1024 * 1024,
		"1GiB":   1024 * 1024 * 1024,
	}

	for input, want := range tests {
		got, err := ParseSize(input)
		if err != nil {
			t.Errorf("ParseSize(%q) error = %v, want nil", input, err)
			continue
		}
		if got != want {
			t.Errorf("ParseSize(%q) = %d, want %d", input, got, want)
		}
	}
}

func TestParseSizeRejectsInvalidSyntax(t *testing.T) {
	for _, input := range []string{"1kib", "1MB", "128 MiB", "1.5MiB", "1024", "-1B", "B"} {
		if _, err := ParseSize(input); err == nil {
			t.Errorf("ParseSize(%q) error = nil, want error", input)
		}
	}
}

func TestLoadFileRangeBoundaries(t *testing.T) {
	dir := t.TempDir()
	cfg := loadFromString(t, dir, `
server:
  listen_address: "127.0.0.1:9090"
process:
  command: ["echo"]
  restart_delay: "100ms"
  graceful_shutdown_timeout: "0s"
  working_directory: "./process"
logs:
  directory: "./logs"
  max_entry_size: "1KiB"
  stdout:
    disk_budget: "16MiB"
  stderr:
    disk_budget: "16MiB"
events:
  retained_count: 1
`)
	if cfg.Process.RestartDelay != 100*time.Millisecond || cfg.Process.GracefulShutdownTimeout != 0 {
		t.Errorf("minimum duration config = restart %s graceful %s, want 100ms and 0s", cfg.Process.RestartDelay, cfg.Process.GracefulShutdownTimeout)
	}

	cfg = loadFromString(t, dir, `
server:
  listen_address: "127.0.0.1:9090"
process:
  command: ["echo"]
  restart_delay: "1h"
  graceful_shutdown_timeout: "10m"
  working_directory: "./process2"
logs:
  directory: "./logs2"
  max_entry_size: "1MiB"
events:
  retained_count: 65536
`)
	if cfg.Process.RestartDelay != time.Hour || cfg.Process.GracefulShutdownTimeout != 10*time.Minute {
		t.Errorf("maximum duration config = restart %s graceful %s, want 1h and 10m", cfg.Process.RestartDelay, cfg.Process.GracefulShutdownTimeout)
	}
	if cfg.Logs.MaxEntrySize != 1024*1024 || cfg.Events.RetainedCount != 65536 {
		t.Errorf("maximum size/count config = max_entry_size %d retained %d, want 1MiB and 65536", cfg.Logs.MaxEntrySize, cfg.Events.RetainedCount)
	}
}

func TestLoadFileRejectsRangeViolations(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{name: "restart too small", wantErr: "process.restart_delay", yaml: validConfigWith(`
  restart_delay: "99ms"`, "", "")},
		{name: "restart too large", wantErr: "process.restart_delay", yaml: validConfigWith(`
  restart_delay: "1h1ns"`, "", "")},
		{name: "graceful too small", wantErr: "process.graceful_shutdown_timeout", yaml: validConfigWith(`
  graceful_shutdown_timeout: "-1ns"`, "", "")},
		{name: "graceful too large", wantErr: "process.graceful_shutdown_timeout", yaml: validConfigWith(`
  graceful_shutdown_timeout: "10m1ns"`, "", "")},
		{name: "max entry too small", wantErr: "logs.max_entry_size", yaml: validConfigWith("", `
  max_entry_size: "1023B"`, "")},
		{name: "max entry too large", wantErr: "logs.max_entry_size", yaml: validConfigWith("", `
  max_entry_size: "1048577B"`, "")},
		{name: "stdout budget too small", wantErr: "logs.stdout.disk_budget", yaml: validConfigWith("", `
  stdout:
    disk_budget: "16777215B"`, "")},
		{name: "stderr budget too small", wantErr: "logs.stderr.disk_budget", yaml: validConfigWith("", `
  stderr:
    disk_budget: "16777215B"`, "")},
		{name: "events too small", wantErr: "events.retained_count", yaml: validConfigWith("", "", `
  retained_count: 0`)},
		{name: "events too large", wantErr: "events.retained_count", yaml: validConfigWith("", "", `
  retained_count: 65537`)},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadFromStringErr(t, t.TempDir(), tt.yaml)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("LoadFile() error = %v, want field name %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadFileEncodingValidation(t *testing.T) {
	cfg := loadFromString(t, t.TempDir(), `
server:
  listen_address: "127.0.0.1:9090"
process:
  command: ["echo"]
logs:
  encoding:
    structured:
      format: "slog_json"
`)
	if cfg.Logs.Encoding.Mode != EncodingStructured || cfg.Logs.Encoding.StructuredFormat != StructuredFormatSlogJSON {
		t.Errorf("Encoding = %+v, want structured slog_json", cfg.Logs.Encoding)
	}

	_, err := loadFromStringErr(t, t.TempDir(), `
server:
  listen_address: "127.0.0.1:9090"
process:
  command: ["echo"]
logs:
  encoding:
    structured:
      format: "json"
`)
	if err == nil || !strings.Contains(err.Error(), "logs.encoding.structured.format") {
		t.Fatalf("LoadFile() error = %v, want structured format validation error", err)
	}
}

func TestLoadFileDirectoryValidation(t *testing.T) {
	t.Run("creates missing directories", func(t *testing.T) {
		dir := t.TempDir()
		cfg := loadFromString(t, dir, `
server:
  listen_address: "127.0.0.1:9090"
process:
  command: ["echo"]
  working_directory: "./new-process"
logs:
  directory: "./new-logs"
`)
		for _, path := range []string{cfg.Process.WorkingDirectory, cfg.Logs.Directory} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("os.Stat(%q) error = %v, want nil", path, err)
			}
			if !info.IsDir() {
				t.Fatalf("os.Stat(%q).IsDir = false, want true", path)
			}
		}
	})

	t.Run("rejects existing file", func(t *testing.T) {
		dir := t.TempDir()
		filePath := filepath.Join(dir, "logs-file")
		if err := os.WriteFile(filePath, []byte("not a directory"), 0o644); err != nil {
			t.Fatalf("os.WriteFile() error = %v, want nil", err)
		}
		_, err := loadFromStringErr(t, dir, `
server:
  listen_address: "127.0.0.1:9090"
process:
  command: ["echo"]
  working_directory: "./process"
logs:
  directory: "./logs-file"
`)
		if err == nil || !strings.Contains(err.Error(), "logs.directory") {
			t.Fatalf("LoadFile() error = %v, want logs.directory file rejection", err)
		}
	})

	t.Run("rejects nested process directory", func(t *testing.T) {
		_, err := loadFromStringErr(t, t.TempDir(), `
server:
  listen_address: "127.0.0.1:9090"
process:
  command: ["echo"]
  working_directory: "./logs/process"
logs:
  directory: "./logs"
`)
		if err == nil || !strings.Contains(err.Error(), "process.working_directory") {
			t.Fatalf("LoadFile() error = %v, want nested process directory rejection", err)
		}
	})

	t.Run("rejects nested logs directory", func(t *testing.T) {
		_, err := loadFromStringErr(t, t.TempDir(), `
server:
  listen_address: "127.0.0.1:9090"
process:
  command: ["echo"]
  working_directory: "./process"
logs:
  directory: "./process/logs"
`)
		if err == nil || !strings.Contains(err.Error(), "logs.directory") {
			t.Fatalf("LoadFile() error = %v, want nested logs directory rejection", err)
		}
	})

	t.Run("rejects non writable directory", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("chmod-based non-writable directory setup is not portable on windows")
		}
		dir := t.TempDir()
		logsDir := filepath.Join(dir, "logs")
		if err := os.Mkdir(logsDir, 0o555); err != nil {
			t.Fatalf("os.Mkdir() error = %v, want nil", err)
		}
		defer func() {
			if err := os.Chmod(logsDir, 0o755); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("restore chmod error = %v, want nil", err)
			}
		}()
		_, err := loadFromStringErr(t, dir, `
server:
  listen_address: "127.0.0.1:9090"
process:
  command: ["echo"]
  working_directory: "./process"
logs:
  directory: "./logs"
`)
		if err == nil || !strings.Contains(err.Error(), "logs.directory") {
			t.Fatalf("LoadFile() error = %v, want logs.directory writability rejection", err)
		}
	})
}

func TestLoadFileTestdataExamples(t *testing.T) {
	for _, name := range []string{"plain.yaml", "structured.yaml"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			oldwd, err := os.Getwd()
			if err != nil {
				t.Fatalf("os.Getwd() error = %v, want nil", err)
			}
			defer func() {
				if err := os.Chdir(oldwd); err != nil {
					t.Fatalf("restore working directory error = %v, want nil", err)
				}
			}()
			if err := os.Chdir(dir); err != nil {
				t.Fatalf("os.Chdir(%q) error = %v, want nil", dir, err)
			}
			cfg, err := LoadFile(filepath.Join(oldwd, "..", "..", "testdata", name))
			if err != nil {
				t.Fatalf("LoadFile(%s) error = %v, want nil", name, err)
			}
			if !filepath.IsAbs(cfg.Logs.Directory) || cfg.Logs.Directory != filepath.Join(dir, "runner-logs") {
				t.Errorf("Logs.Directory = %q, want relative path resolved from working directory %q", cfg.Logs.Directory, dir)
			}
		})
	}
}

func loadFromString(t *testing.T, dir, content string) Config {
	t.Helper()
	cfg, err := loadFromStringErr(t, dir, content)
	if err != nil {
		t.Fatalf("LoadFile() error = %v, want nil", err)
	}
	return cfg
}

func loadFromStringErr(t *testing.T, dir, content string) (Config, error) {
	t.Helper()
	path := filepath.Join(dir, "runner.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v, want nil", path, err)
	}
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd() error = %v, want nil", err)
	}
	defer func() {
		if err := os.Chdir(oldwd); err != nil {
			t.Fatalf("restore working directory error = %v, want nil", err)
		}
	}()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("os.Chdir(%q) error = %v, want nil", dir, err)
	}
	return LoadFile(path)
}

func validConfigWith(processExtra, logsExtra, eventsExtra string) string {
	return `
server:
  listen_address: "127.0.0.1:9090"
process:
  command: ["echo"]
  working_directory: "./process"` + processExtra + `
logs:
  directory: "./logs"` + logsExtra + `
events:` + eventsExtra + `
`
}
