# Runner Implementation Plan

This plan implements `docs/runner.md` in small, encapsulated Go packages. The
repository uses conventional Go layout:

- `cmd/runner`: the runner executable entrypoint.
- `internal/config`: YAML loading, defaults, size parsing, and validation.
- `internal/events`: in-memory runner event ring.
- `internal/logparse`: plain-text and `slog_json` child log parsing.
- `internal/logstore`: SQLite-backed log storage, retention, and readers.
- `internal/process`: one-child supervisor and shutdown behavior.
- `internal/server`: gRPC service implementation.
- `proto`: protobuf API definitions.
- `gen/go`: generated Go protobuf code, produced by `buf generate`.
- `bin`: local build outputs, produced by `make build`.

## Coding Guidelines

Keep each package responsible for one component and expose only the interfaces
needed by neighboring packages. Prefer small structs with explicit dependencies
over package-level mutable state.

All exported types, constants, variables, functions, and methods must have
GoDoc comments. Comments must document expected inputs, assumptions, return
values, error conditions, and side effects. For constructors, include ownership
rules for passed dependencies such as directories, clocks, processes, channels,
or contexts.

Keep functions reasonably small. When a function has separate parsing,
validation, state mutation, I/O, or error mapping steps, split those steps into
private helpers with names that describe the invariant they enforce.

Handle errors at every layer and wrap propagated errors with context using
`fmt.Errorf("...: %w", err)` or a package-specific error type when callers need
structured decisions. Do not log and return the same error unless the log adds
state that the caller cannot add.

Tests must make failures self-explanatory. Use assertions such as
`t.Errorf("Validate() error = %v, want nil for default config", err)` or
`t.Errorf("entry.LogID = %d, want %d after rejected invalid UTF-8 line", got,
want)`. If the condition is long, put a short comment above the assertion that
states the condition being tested.

## Phase 1: Build System and Proto Generation

Implement and maintain:

- `go.mod` with module path `github.com/smukherj/homelab-depot/runner`.
- `Makefile` targets:
  - `make generate`: run `buf generate`.
  - `make proto-lint`: run `buf lint`.
  - `make proto-check`: fail when generated code is stale.
  - `make test`: run unit tests.
  - `make test-integration`: run integration-tagged or named tests.
  - `make test-e2e`: build the runner and run end-to-end tests.
  - `make build`: write `bin/runner`.
  - `make clean`: remove generated outputs and binaries.
- `buf.yaml` and `buf.gen.yaml` using local `protoc-gen-go` and
  `protoc-gen-go-grpc`.
- `.gitignore` entries for `bin/` and `gen/go/`.

Unit tests:

- Add a lightweight build test once packages exist by relying on `go test ./...`.
- Keep `proto-check` in CI or the local verification list so generated code
  drift is caught.

## Phase 2: Protobuf API

Update `proto/runner.proto` to match `docs/runner.md` before implementing the
server:

- Use package `homelabdepot.runner` and Go package `runnerpb`.
- Add `GetEvents`.
- Rename `GetLogRequest.limit` to `max_entries`.
- Use `google.protobuf.Timestamp` for process, log, and event timestamps.
- Model per-stream `LogStatus` for stdout and stderr.
- Add process fields for `running`, `current_start_time`, `last_exit_time`,
  `last_exit_code`, `last_exit_signal`, `restart_count`, and configured command.
- Add `LogEntry.log_id`, `LogEntry.source`, `LogEntry.level` as a string,
  `LogEntry.message`, `LogEntry.truncated`, and optional source location.
- Add `RunnerEvent`, `GetEventsRequest` with a request-mode `oneof`, and
  `GetEventsResponse.next_sequence_number`.

Unit tests:

- Compile generated code with `go test ./...`.
- Add server mapping tests that verify internal models convert to proto fields,
  including unset timestamp behavior and optional source locations.

Integration tests:

- Start a real gRPC server on `127.0.0.1:0`, call each RPC with generated
  clients, and verify status code mappings for invalid source, out-of-range log
  IDs, and malformed event requests.

## Phase 3: Configuration

Implement `internal/config`:

- `LoadFile(path string) (Config, error)` reads YAML, applies defaults, resolves
  relative log directories against the current working directory, and validates
  all fields before returning.
- `ParseSize(string) (uint64, error)` accepts only `B`, `KiB`, `MiB`, and `GiB`
  binary units with non-negative integer amounts.
- `ParseDuration` uses Go duration syntax only.
- Validate required fields: `server.listen_address`, non-empty
  `process.command`, and `logs.directory`.
- Validate ranges from `docs/runner.md`.
- Validate `logs.encoding` as either default plain text or structured
  `slog_json`.
- Check that the log directory can be created and is writable before the child
  process starts.

Unit tests:

- Defaults are applied when optional sections are omitted.
- Required fields fail with errors naming the missing field.
- Command arrays reject empty arrays and empty elements.
- Durations reject bare integers and human-word durations.
- Size strings accept exact valid examples and reject lowercase units, decimal
  units, spaces, fractions, and bare integers.
- Boundary tests cover minimum and maximum restart delay, graceful shutdown
  timeout, max entry size, disk budgets, and event retention count.
- Directory validation covers missing directory creation, existing file path,
  and non-writable directory where the platform allows the test to set that up.

Integration tests:

- Load example plain-text and structured YAML files from `testdata`.
- Verify relative log directories resolve from the runner working directory.

## Phase 4: Runner Events

Implement `internal/events`:

- Fixed-capacity in-memory ring with monotonically increasing `uint64` sequence
  numbers.
- Event fields: sequence number, timestamp, severity/type, code, message, and
  optional structured details.
- Query modes: all retained events, last N, or from sequence number
  inclusively.
- Always return `next_sequence_number`.
- Truncate event messages and raw-line details to the configured entry limit.
- Keep the package concurrency-safe because process, log parsing, storage, and
  server code may record events concurrently.

Unit tests:

- Sequence numbers start at zero and increase by one.
- Ring overflow drops oldest events and keeps `next_sequence_number`.
- `last_count` zero returns no events.
- `last_count` above capacity returns all retained events.
- `from_sequence_number` before the retained range starts at the oldest retained
  event.
- `from_sequence_number` after the next sequence returns an empty list.
- Concurrent writers and readers pass `go test -race`.
- Truncation preserves the configured byte limit.

Integration tests:

- Through the gRPC server, record events and verify `GetEvents` polling can
  de-duplicate with `next_sequence_number`.

## Phase 5: Log Parsing

Implement `internal/logparse`:

- A stream parser that accepts byte chunks and emits complete validated entries.
- Plain-text mode:
  - Timestamp is assigned when the first byte of a line is observed.
  - Stored message includes the trailing newline.
  - stdout level is `INFO`; stderr level is `ERROR`.
  - Invalid UTF-8 rejects the line, emits a runner event, and does not consume a
    log ID.
  - Oversized lines keep the allowed prefix, discard through newline or EOF, and
    set `truncated = true`.
- Structured `slog_json` mode:
  - Parse newline-delimited JSON compatible with Go `slog.JSONHandler`.
  - Require string `time`, non-empty string `level`, and string `msg`.
  - Parse time as RFC3339 or RFC3339Nano and preserve producer timestamp.
  - Preserve arbitrary non-empty level strings.
  - Parse optional `source` object opportunistically.
  - Reject malformed JSON or missing required fields and emit runner events with
    retained raw prefix details.
  - Attempt to parse truncated prefixes; accepted entries are marked truncated,
    rejected prefixes produce structured rejection events.

Unit tests:

- Plain-text stdout and stderr levels are assigned correctly.
- Timestamps are taken from first byte observation, including delayed newline.
- EOF flushes an unterminated line.
- Invalid UTF-8 emits an event and does not advance the log ID allocator in the
  caller-level test.
- Oversized plain-text entries retain only the allowed prefix and discard the
  suffix.
- Structured logs parse valid `slog_json` lines with and without source.
- Structured logs preserve custom levels.
- Structured logs reject invalid JSON, missing `time`, malformed `time`, missing
  `level`, empty `level`, and missing or non-string `msg`.
- Invalid optional source fields do not reject the entry.
- Truncated structured prefixes cover both accepted and rejected cases.

Integration tests:

- Pipe stdout and stderr from a helper process through the parser and store,
  then verify accepted entries and rejection events across both streams.

## Phase 6: Log Storage and Retention

Implement `internal/logstore`:

- Startup cleanup removes only runner-owned SQLite files from the configured log
  directory: `logs.sqlite`, `logs.sqlite-wal`, and `logs.sqlite-shm`.
- Create one SQLite database at `logs.sqlite` for both child log streams.
- Enable WAL journal mode and `synchronous=NORMAL` during initialization so live
  readers can query retained rows while the capture path inserts new entries.
- Store parsed API fields only: `source`, `log_id`, timestamp, level, message,
  truncation flag, optional source location fields, and `stored_size`.
- Use `(source, log_id)` as the primary key and read one selected stream ordered
  by `log_id`.
- Maintain independent per-stream state for stdout and stderr: next log ID,
  retained bytes, logical budget, and current retained range.
- Assign log IDs only after parsing accepts an entry. Rejected lines do not
  consume a log ID.
- Enforce retention after each accepted row by deleting oldest rows for the
  affected source until that stream's logical `stored_size` total is within its
  budget.
- Ensure cleanup advances only the affected stream's `begin_log_id` and leaves
  at least the newest row for that stream.
- Checkpoint the WAL after retention cleanup and support an optional periodic
  checkpoint so WAL growth does not hide unbounded disk use.
- Treat unexpected SQLite errors as storage failures: record runner events with
  operation, stream when relevant, database path, and reason, and fail affected
  reads with internal errors.
- Provide blocking readers that can start at any log ID in
  `[begin_log_id, end_log_id]`, deliver batches, and fail with out-of-range if
  retention removes needed entries.

Unit tests:

- Startup cleanup deletes only `logs.sqlite`, `logs.sqlite-wal`, and
  `logs.sqlite-shm`, and preserves unrelated files.
- Schema creation is idempotent and configures WAL plus `synchronous=NORMAL`.
- Inserted rows round-trip all API fields, including optional source locations
  and truncation flags.
- Appending entries advances `end_log_id`; retention advances `begin_log_id`.
- stdout and stderr log IDs and retained byte counts are independent.
- `stored_size` accounting drives logical retention for each stream.
- Requests below `begin_log_id` or above `end_log_id` return out-of-range.
- Retention deletes oldest rows only for the over-budget source and leaves the
  other stream untouched.
- Retention leaves at least the newest accepted row when a single row fits the
  validated budget rules.
- WAL checkpointing after retention can be observed in tests without relying on
  exact physical file sizes.
- SQLite execution or scan failures return internal storage errors and emit
  runner events.
- Batch reads obey 1024-entry and 4 MB response-planning limits.
- Concurrent appends, readers, and retention pass `go test -race`.

Integration tests:

- Write enough entries to exceed a small test budget, then verify retained
  ranges, deleted rows, logical byte accounting, and historical reads.
- Hold a blocking reader at `end_log_id`, append entries, and verify it wakes
  without intentional delay.
- Hold a slow reader while retention removes its needed log ID and verify it
  fails with out-of-range.

## Phase 7: Process Supervisor

Implement `internal/process`:

- Start exactly one configured command without shell interpretation.
- Capture stdout and stderr separately and feed each stream into log parsing and
  storage.
- Restart after every child exit, including exit code `0`, using the fixed
  configured restart delay.
- Track status: running, current start time, last exit time, last exit code,
  last exit signal, restart count, and configured command.
- Record lifecycle events for runner startup, child start, child exit, restart
  scheduled, graceful termination, force-kill, and runner shutdown.
- On intentional shutdown, send graceful termination, wait for configured
  timeout, force-kill if needed, and do not restart.
- Keep process state concurrency-safe for `GetStatus`.

Unit tests:

- Command configuration is passed to `exec.CommandContext` without shell
  expansion.
- Exit code `0` still schedules restart.
- Non-zero exit and signal termination populate the correct status fields.
- Restart count excludes initial launch and increments only after exits.
- Shutdown with a cooperative child records graceful termination and avoids
  restart.
- Shutdown timeout force-kills an uncooperative child and records the force-kill
  event.
- stdout and stderr capture are routed to the correct parser/store.
- Errors from starting the child are wrapped with command context and recorded.

Integration tests:

- Use small helper binaries under `internal/process/testdata` to emit logs,
  exit with specific codes, ignore termination, and write invalid UTF-8.
- Verify restarts happen after the configured delay with a fake or short real
  clock strategy.
- Run with `go test -race` to validate concurrent status reads during restarts.

## Phase 8: gRPC Server

Implement `internal/server`:

- Map internal status, log entries, and events to generated protobuf messages.
- `GetStatus` returns process status plus per-stream stdout and stderr retained
  ranges.
- `GetLog` validates source and log ID, streams historical and live entries,
  batches adjacent available entries, and respects `max_entries`.
- `GetLog` uses gRPC status codes:
  - `InvalidArgument` for unknown log source or malformed request values.
  - `OutOfRange` for requested or needed log IDs outside retained ranges.
  - `Canceled` when client cancellation ends the stream.
  - `Internal` for unexpected storage failures.
- `GetEvents` implements all request modes and rejects impossible malformed
  oneof states defensively.
- Include structured out-of-range details when possible.

Unit tests:

- Internal-to-proto mapping covers unset timestamps, exit code vs signal,
  source locations, truncation flags, and independent stream ranges.
- `GetLog` rejects unspecified source.
- `GetLog` with `max_entries > 0` stops after exactly the requested count.
- `GetLog` with `max_entries = 0` follows until client cancellation.
- Batching never exceeds entry count or message-size limits.
- Storage errors map to the documented gRPC codes.
- `GetEvents` returns expected events and `next_sequence_number` for each mode.

Integration tests:

- Start the server with in-memory or temporary-directory dependencies and use a
  real generated gRPC client.
- Verify finite historical log reads, live-follow reads, cancellation behavior,
  event polling, and out-of-range error details.

## Phase 9: Command Entrypoint

Implement `cmd/runner`:

- Parse CLI flags, starting with `--config path`.
- Load and validate config before opening the listener or launching the child.
- Initialize event ring, log stores, process supervisor, and gRPC server.
- Start listening on the configured address.
- Launch the child only after all startup validation succeeds.
- Handle SIGINT and SIGTERM by gracefully shutting down the gRPC server and
  supervisor.
- Return non-zero on startup failures.

Unit tests:

- Flag parsing rejects missing config path.
- Startup assembly returns contextual errors for config, log directory, listener,
  store, and supervisor failures.
- Signal handling calls shutdown once.

Integration tests:

- Run `bin/runner --config testdata/plain.yaml` with a helper process.
- Verify the configured listener is reachable and the child starts only after
  validation succeeds.
- Verify startup failure for invalid config does not launch the child.

## Phase 10: End-to-End Tests

Add e2e tests that build and run the actual `bin/runner` binary:

- Plain-text mode:
  - Helper process writes stdout and stderr lines.
  - `GetStatus` reports running process and independent stream ranges.
  - `GetLog` retrieves historical entries and follows new entries.
- Structured mode:
  - Helper process writes valid `slog_json`, custom levels, source locations,
    and invalid structured lines.
  - Accepted entries appear in logs; rejected lines appear only in events.
- Restart behavior:
  - Helper process exits with code `0` and later non-zero.
  - Runner records exit and restart events and increments restart count.
- Retention behavior:
  - Configure small valid disk budgets and write enough logs to exceed them.
  - Verify old log IDs return `OutOfRange` and current ranges remain readable.
- Shutdown behavior:
  - Send SIGTERM to the runner.
  - Verify graceful child shutdown when cooperative and force-kill behavior with
    an uncooperative helper in a separate test.

## Verification Checklist

Run before considering the implementation complete:

```sh
make generate
make proto-lint
make proto-check
make test
make test-integration
make test-e2e
make build
```
