# Runner v1 Product Requirements and Design

## Overview

Runner is a small supervisor process for one configured child process. Its
immediate use case is supervising the remote agent described in `docs/agent.md`,
but the runner itself should stay generic: it launches one configured command,
keeps it running, captures its stdout and stderr, records recent runner lifecycle
events, and exposes status plus log retrieval over gRPC.

The runner does not proxy the agent API. Remote systems interact with the agent
directly. Remote systems use the runner only to inspect process status and logs,
especially after the supervised process exits and is restarted.

Network reachability, firewall policy, authentication, authorization, TLS, SSH
tunnels, VPNs, and service exposure are out of scope for runner v1. Unlike the
original agent design document, the runner must not assume it is limited to a
loopback listener. It should be configurable to listen on the address supplied by
deployment.

## Goals

- Launch exactly one configured child process.
- Restart the child process after every exit, including exit code `0`.
- Use a fixed configurable restart delay.
- Capture stdout and stderr as separate structured log streams.
- Retain child logs on disk with separate configurable disk budgets for stdout
  and stderr.
- Rotate retained logs when a stream exceeds its disk budget.
- Expose current log begin and end offsets for every child log stream.
- Expose finite historical log reads and live-follow log streams over gRPC.
- Keep a configurable in-memory ring of recent runner events.
- Expose runner events through a dedicated non-streaming RPC.
- Record exits as ordinary process exit events with exit code or signal, then
  restart.
- Terminate the child process during intentional runner shutdown with a
  configurable graceful timeout before force-kill.
- Load configuration from YAML so command arrays and nested log policy are easy
  to express.

## Non-Goals

- Supervising more than one child process in one runner instance.
- Proxying, wrapping, or forwarding the agent API.
- Manual process controls such as `Start`, `Stop`, `Restart`, or `Reload`.
- Crash-loop detection, special startup grace periods, or restart backoff.
- Persisting runner events across runner restarts.
- Preserving child logs across runner restarts.
- Exposing child process environment variables in status.
- Configuring child process environment variables in v1.
- Configuring a child working directory in v1; the child inherits runner's
  working directory.
- Implementing network security inside runner.
- Core dump collection in v1.

## Process Lifecycle

Runner supervises one configured command. The command and arguments are stored in
YAML as an array and are executed directly, without shell interpretation unless
the configured command explicitly invokes a shell.

Runner starts the child after startup. When the child exits for any reason, the
runner records an exit event, waits the configured fixed restart delay, and
starts the child again. Exit code `0` is not treated as a clean terminal state;
it is still restarted.

Runner does not expose manual process controls in v1. While runner is alive and
not intentionally shutting down, it attempts to keep the child running.

During intentional runner shutdown:

- runner records a final shutdown event on a best-effort basis;
- runner sends a graceful termination signal to the child;
- runner waits for the configured graceful shutdown timeout;
- runner force-kills the child if it is still running after the timeout;
- runner does not restart the child.

The default graceful shutdown timeout is 30 seconds.

## Process Status

`GetStatus` returns process and log status. v1 does not need a process state
enum. Instead, status includes enough information for clients to determine the
current situation.

Process status includes:

- whether the child is currently running;
- current child start time, present only while the child is running;
- most recent child exit time, when available;
- most recent child exit code, when the process exited normally;
- most recent child signal, when the process was terminated by signal;
- restart count, counting restarts after exits only and not counting the initial
  launch;
- configured command exactly as supplied, without redaction.

If the child is waiting for the fixed restart delay, `running` is false and
`current_start_time` is unset.

Timestamps should use `google.protobuf.Timestamp` rather than integer epoch
milliseconds so unset timestamps are natural in generated clients.

## Runner Events

Runner keeps an in-memory ring buffer of recent structured events. Events are
not persisted to disk and do not survive runner restarts.

Each event includes:

- a monotonically increasing unsigned 64-bit sequence number;
- timestamp;
- severity or type;
- event code;
- human-readable message;
- optional structured details where useful.

Events should cover at least:

- runner startup;
- child process start;
- child process exit;
- restart scheduled;
- child process graceful termination during runner shutdown;
- child process force-kill during runner shutdown;
- runner shutdown;
- log rotation;
- invalid structured log line rejected;
- invalid UTF-8 log line rejected.

`GetEvents` is a dedicated non-streaming RPC. Event details are not included in
`GetStatus`.

`GetEvents` supports:

- requesting the last N retained events;
- requesting events after a supplied sequence number, so clients can de-duplicate
  and poll incrementally.

Runner events use the same per-entry message size limit as child logs. If an
event message or included raw line exceeds that limit, it is truncated to the
same retained byte limit.

## Child Log Streams

Runner captures stdout and stderr as two separate streams. v1 does not provide a
merged chronological stream.

Child logs are exposed as structured log entries. Each accepted log entry has:

- timestamp;
- level;
- message;
- source location, when present in structured logs;
- stream source: stdout or stderr.

The API offset model is logical rather than raw file based. Offsets count only
the retained UTF-8 bytes of accepted `LogEntry.message` values in a stream.
Metadata such as timestamp, level, source, source location, framing, and index
data do not contribute to the offset. Rejected log lines do not advance the
logical offset.

The offset of a log entry is the start offset of that entry's message. A batch
response can include a single `begin_offset` and `end_offset` for all entries in
the response. `begin_offset` is the offset of the first returned entry's message.
`end_offset` is `begin_offset` plus the total retained message-byte length of all
entries in that response.

When a message exceeds the configured maximum entry size, runner stores only the
first maximum allowed bytes and discards the rest until newline or EOF. Offsets
advance by the retained/truncated bytes actually available to clients, not by
the original pre-truncation length. v1 does not expose a `truncated` boolean on
log entries.

The maximum log entry size is measured in bytes after UTF-8 encoding.

## Plain Text Log Mode

In plain-text mode, runner treats each newline-delimited line as one log entry.
The stored message includes the trailing newline.

The timestamp for a plain-text log entry is the time at which runner first sees
bytes for that line, not the time the terminating newline or EOF arrives.

If a process writes a long line without a newline, runner buffers until newline
or EOF. If the line exceeds the configured maximum entry size, runner retains
only the first allowed bytes and discards additional bytes until newline or EOF.

Invalid UTF-8 lines are rejected, omitted from the child log stream, and
represented only by a runner event containing the reason, source stream, and
logical offset at which the invalid line would otherwise have appeared. The raw
invalid bytes are not included in the event.

## Structured Log Mode

Structured logging is configurable, but v1 supports only one structured format:
Go `log/slog` JSON output.

The format should be named precisely, for example `slog_json`, and should mean
newline-delimited JSON records emitted by Go's `log/slog.JSONHandler`. This is
JSON that common log pipelines can ingest, but it is not a single universal Loki
or Elastic standard.

For accepted `slog_json` lines, runner parses:

- timestamp;
- level;
- message;
- source file/function/line location when present.

The returned `LogEntry.message` contains only the slog message field, not the
entire original JSON line.

If a structured log line is invalid JSON, or valid JSON but missing required
timestamp, level, or message fields, runner rejects the line and emits a runner
event. Rejected structured log lines are omitted from the child log stream and do
not advance the logical log offset.

The rejection event includes the raw bad line, subject to the same per-entry size
limit as ordinary logs and events.

## Log Retention and Rotation

Child stdout and stderr logs are retained on disk, not in memory, because logs
can be large. Each stream has a separate configurable disk budget.

Disk budgets are configured with human-readable sizes such as bytes, KB, MB, or
GB. Invalid or too-small budgets fail runner startup.

The maximum log entry size is also configured with the same human-readable size
syntax. Invalid or too-small entry limits fail runner startup.

Logs do not need to survive runner restarts. On startup, runner deletes or
truncates old log files in its configured log directory before beginning the new
runner lifetime.

When retained logs exceed a stream's budget, runner rotates out old data. If a
client requests offset `0`, runner starts at the oldest retained entry for that
stream. If a client requests a nonzero offset that has already rotated out,
`GetLog` returns `OutOfRange` rather than silently starting at the oldest
retained entry.

Because API offsets count only retained message bytes while disk files may also
store metadata and framing, the implementation may need an index mapping logical
message offsets to file positions.

## Log Streaming API

`GetStatus` exposes `begin_offset` and `end_offset` for every child log stream:
stdout and stderr.

`GetLog` reads one source stream at a time: stdout or stderr.

`GetLog` supports both historical reads and live follow:

- request offset `0` to start at the oldest retained entry;
- request nonzero offset to resume from that logical message offset;
- request `max_entries > 0` to stream until that many log entries have been
  delivered, then end the RPC;
- request `max_entries = 0` to follow forever until client cancellation.

When `max_entries > 0`, the server streams entries already available at the
requested offset, then continues streaming entries as they materialize until the
requested count has been reached or the client cancels.

When `max_entries = 0`, the server streams entries as soon as they are available
and keeps the stream open until the client cancels.

Responses may batch multiple log entries for efficiency. Each response should
stay within the default 4 MB gRPC message size limit.

The existing proto field named `limit` should be renamed to `max_entries` to
make it explicit that the limit counts entries, not bytes.

## Configuration

Runner is configured with YAML. YAML is the preferred v1 format because it is
easy to express a command and arguments without shell quoting and can represent
nested log policy cleanly.

Configuration includes at least:

- gRPC listen address;
- command and arguments;
- fixed restart delay;
- graceful shutdown timeout, default 30 seconds;
- log directory;
- stdout disk budget;
- stderr disk budget;
- maximum log entry size;
- log format: plain text or structured;
- structured log format, currently only `slog_json`;
- retained runner event count.

The child process inherits runner's working directory. v1 does not configure
child environment variables.

All sizes, durations, listen addresses, command arrays, log formats, disk
budgets, and retention counts are validated on startup. Invalid configuration
fails before the runner starts listening or launches the child process.

## gRPC API Implications

The current `proto/runner.proto` sketch should evolve to reflect the decisions
above.

Likely changes:

- add `GetEvents`;
- rename `GetLogRequest.limit` to `max_entries`;
- use `google.protobuf.Timestamp` for process and log timestamps;
- include per-stream log status for stdout and stderr in `GetStatus`;
- include `running`, current start time, last exit time, last exit code, and last
  exit signal in process status;
- add source location fields to `LogEntry`;
- include batch `begin_offset` and `end_offset` in `GetLogResponse`;
- keep stdout and stderr as the only child log sources;
- model runner events separately from child log entries.

`GetLog` should use gRPC status codes consistently:

- `InvalidArgument` for unknown log source or malformed request values;
- `OutOfRange` for a nonzero requested offset older than the retained begin
  offset;
- `Canceled` when the client cancels a streaming request;
- `Internal` for unexpected storage or indexing failures.

## Open Questions

- For oversized plain-text lines, should the timestamp always remain the time
  runner first saw the line's first bytes, even if newline or EOF arrives much
  later?
- What minimum disk budget should be considered too small for startup
  validation?
- What minimum maximum-entry-size should be considered too small for startup
  validation?
- What exact YAML schema and field names should v1 use?
- What exact `slog_json` field names should be required for timestamp, level,
  message, and source location? Should runner support slog's default keys only,
  or configurable key names later?
- What exact shape should source location have in `LogEntry`: file, function,
  line, or a single string?
- Should `LogEntry.level` include only debug/info/warn/error, or also unknown
  and arbitrary string levels from structured logs?
- Should `OutOfRange` include current begin and end offsets in structured error
  details, or should clients recover by calling `GetStatus`?
- What batching policy should runner use beyond staying under 4 MB: max entries
  per batch, max bytes per batch, max latency, or a combination?
- Should disk log rotation delete whole segment files only, or is trimming within
  a segment acceptable?
- What file/index format should be used for retained log entries on disk?
- Should runner expose build/version information in `GetStatus`?
