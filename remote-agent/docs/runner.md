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

`GetStatus` does not expose runner build or version information in v1. Runner
does not provide a separate version RPC in v1.

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
- requesting events from a supplied sequence number, inclusively, so clients can
  de-duplicate and poll incrementally.

`GetEvents` uses event-specific sequence numbers, not log offset terminology.
Runner event sequence numbers are global to the runner event stream and are
independent from child stdout and stderr log offsets.

`GetEvents` should use a request-mode `oneof`:

- `from_sequence_number`, a `uint64`, returns retained events at or after that
  sequence number;
- `last_count`, a `uint64`, returns up to that many newest retained events.

If neither request mode is set, `GetEvents` returns all currently retained
events, bounded by the configured event ring size. If both modes are somehow
provided, the request is invalid. If `last_count` is `0`, the response contains
no events. If `last_count` is larger than the retained event capacity, runner
returns all retained events.

If `from_sequence_number` is older than the retained event ring, runner starts
from the oldest retained event rather than returning an error. If it is newer
than the next event sequence number, runner returns an empty event list.

Every `GetEvents` response includes `next_sequence_number`, which clients can
use as the next `from_sequence_number` when polling. Runner does not expose the
oldest retained event sequence number in v1.

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
- stream source: stdout or stderr;
- per-stream offset;
- whether the stored entry was truncated.

The API offset model is entry based rather than raw file or byte based. Each
accepted child log entry receives an absolute monotonically increasing `uint64`
offset. Offsets are independent per child log stream: stdout and stderr each
have their own offset sequence.

The first accepted entry in a stream has offset `0`; the next accepted entry in
that same stream has offset `1`, and so on until runner restarts. Rotation never
renumbers retained entries. `stdout` offset `42` and `stderr` offset `42` may
refer to unrelated entries.

Offsets are assigned after validation and parsing. Rejected log lines do not
consume child log offsets. Oversized but otherwise accepted log lines consume
one offset and set `LogEntry.truncated = true`.

`GetStatus` exposes the currently retained offset range for every child log
stream. `begin_offset` is the oldest retained entry offset. `end_offset` is one
past the newest assigned entry offset. Retained entries are in the half-open
range `[begin_offset, end_offset)`.

If no entries have ever been accepted for a stream, both offsets are `0`. If
entries existed but all have rotated out, `begin_offset == end_offset`, and both
equal the next offset that will be assigned.

The maximum log entry size applies to the raw newline-delimited child output
entry before format-specific parsing. It is measured in bytes after UTF-8
encoding for valid UTF-8 text.

## Plain Text Log Mode

In plain-text mode, runner treats each newline-delimited line as one log entry.
The stored message includes the trailing newline.

The timestamp for a plain-text log entry is the time at which runner first sees
bytes for that line, not the time the terminating newline or EOF arrives. This
also applies to oversized lines: truncation does not change timestamp
assignment.

If a process writes a long line without a newline, runner buffers until newline
or EOF. If the line exceeds the configured maximum entry size, runner retains
only the first allowed bytes, discards additional bytes until newline or EOF, and
stores the entry with `truncated = true`.

Invalid UTF-8 lines are rejected, omitted from the child log stream, and
represented only by a runner event containing the reason, source stream, and the
stream's current `end_offset` as context. The raw invalid bytes are not included
in the event.

Plain-text entries use fixed levels by stream: stdout entries use `INFO`, and
stderr entries use `ERROR`. The stream source is still returned separately.

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
- source location when present.

The returned `LogEntry.message` contains only the slog message field, not the
entire original JSON line.

The `slog_json` format supports Go `log/slog.JSONHandler`'s default field names
only. Required fields are `time`, `level`, and `msg`. The optional source field
is `source`. Field names are not configurable in v1.

The `time` field must be a JSON string that parses as RFC 3339 or RFC 3339 Nano.
Structured log entries use this producer timestamp and do not fall back to
runner observation time. Missing, non-string, or malformed `time` rejects the
line.

The `msg` field must be a JSON string. Missing or non-string `msg` rejects the
line. The returned `LogEntry.message` is exactly the parsed `msg` value.

The `level` field must be a non-empty JSON string. Missing, non-string, or empty
`level` rejects the line.

`LogEntry.level` is a string. Structured log entries accept arbitrary non-empty
level values and preserve the original level string. Unknown or custom levels do
not cause rejection.

Extra structured attributes are accepted but ignored in v1.

`LogEntry.source_location` is an optional nested message with `file`,
`function`, and `line` fields. If `source` is absent, runner omits the source
location. If `source` is present and not an object, runner omits the source
location.

If `source` is an object, runner parses recognized fields opportunistically:
`function`, if present, must be a string; `file`, if present, must be a string;
and `line`, if present, must be an integer. Runner keeps valid recognized fields,
ignores invalid recognized fields, and ignores unrecognized source fields. If no
recognized valid fields remain, runner omits the source location.

Malformed optional `source` does not reject the log entry and does not emit a
runner event.

If a structured log line is invalid JSON, or valid JSON but missing required
timestamp, level, or message fields, runner rejects the line and emits a runner
event. Rejected structured log lines are omitted from the child log stream and do
not consume a child log offset.

The rejection event includes the raw bad line, subject to the same per-entry size
limit as ordinary logs and events.

If a structured log line exceeds `max_entry_size`, runner retains only the raw
prefix up to the limit, discards the rest until newline or EOF, and then attempts
to parse the retained prefix. If the prefix parses as a valid structured log
entry, the entry is accepted with `truncated = true`. If the prefix cannot be
parsed as a valid structured log entry, runner rejects the line and emits a
runner event that includes the retained raw prefix with `truncated = true`, the
source stream, the rejection reason, and the stream's current `end_offset` as
context.

Accepted truncation does not emit a runner event. The accepted child log entry's
`truncated` field is the client-visible signal.

## Log Retention and Rotation

Child stdout and stderr logs are retained on disk, not in memory, because logs
can be large. Each stream has a separate configurable disk budget.

Disk budgets are configured with explicit binary size strings such as `16MiB` or
`1GiB`. Invalid or too-small budgets fail runner startup. The default disk
budget is 128MiB per stream. Each stream budget must be at least 16MiB. Runner does not
enforce an explicit maximum budget and does not validate the budget against
currently available disk in v1.

The maximum log entry size is also configured with the same binary size string
syntax. If unset, it defaults to 16KiB. Values below 1KiB or above 1MiB fail
runner startup validation.

Logs do not need to survive runner restarts. On startup, runner deletes all
runner-owned segment and index files in its configured log directory before
beginning the new runner lifetime. Startup does not attempt to recover old data
segments or rebuild missing indexes.

Each stream stores logs in segment files. Segment size is an internal derived
value, not YAML configuration. Runner derives each stream's target segment size
from that stream's disk budget:

```text
clamp(stream_disk_budget / 16, 1MiB, 64MiB)
```

Runner rolls to a new segment when the current segment reaches the target segment
size. The default 128MiB stream budget therefore uses an 8MiB target segment
size. The minimum 16MiB stream budget uses a 1MiB target segment size.

Each segment consists of a JSON Lines data file and a binary index file. The data
file stores a header line followed by one JSON object per accepted `LogEntry`.
The index file stores a fixed binary header followed by fixed-width index
records. Rotation deletes whole data/index segment pairs.

Segment filenames include the stream and the segment base offset:

```text
stdout-00000000000000000000.logseg
stdout-00000000000000000000.idx
stderr-00000000000000000000.logseg
stderr-00000000000000000000.idx
```

The base offset is exactly 20 zero-padded decimal digits, covering the full
`uint64` range and preserving segment order under lexicographic sorting. Segment
identity is stream plus base offset; v1 does not include a separate segment
sequence number. Filename base offset, data header base offset, and index header
base offset must match.

Startup cleanup deletes only runner-owned files matching the fixed stream
patterns, such as `stdout-*.logseg`, `stdout-*.idx`, `stderr-*.logseg`, and
`stderr-*.idx`. Runner does not delete unrelated files in `logs.directory`.

The data file header is a JSON object line containing at least magic/type,
format version, stream source, and base offset. Subsequent JSONL records use a
storage-specific shape, not generated proto JSON. Each record stores enough to
reconstruct the API `LogEntry`:

```json
{
  "offset": 123,
  "ts": "2026-05-07T12:34:56.789Z",
  "level": "INFO",
  "message": "hello\n",
  "source": "stdout",
  "truncated": false,
  "loc": {
    "file": "/app/main.go",
    "function": "main.run",
    "line": 42
  }
}
```

The `loc` object is omitted when no source location is present. Data records do
not store the original raw log line or ignored structured attributes. Although
each stream has its own files, records still include `source` so files remain
self-describing and readers can detect stream/file mismatches.

The binary index header contains at least magic/type, format version, stream
source, and base offset. Index records are fixed-width 20-byte little-endian
records:

```text
uint64 offset
uint64 data_pos
uint32 data_len
```

`data_pos` is the byte offset in the JSONL data file of the first byte of the
record JSON object. `data_len` includes the JSON bytes and the trailing newline,
so `data_pos + data_len` equals the next record's expected `data_pos`. The
offset is stored in both the JSONL data record and the binary index record so
readers can cross-check the data/index pair.

When retained logs exceed a stream's budget, runner rotates out old data. If a
client requests an offset older than the current retained range, `GetLog`
returns `OutOfRange` rather than silently starting at the oldest retained entry.
Rotation advances `begin_offset` for the affected stream and does not affect the
other stream.

Rotation deletes whole segment files only. Runner does not trim within a segment
in v1. Disk usage may exceed the configured budget by up to roughly one target
segment, depending on rollover and write timing.

Because API offsets are logical entry sequence numbers while disk files may also
store metadata and framing, the implementation may need an index mapping entry
offsets to file positions.

During a single runner lifetime, data/index inconsistency is treated as storage
corruption. Runner emits an error event and affected `GetLog` reads fail with
`Internal`. Runner should keep supervising the child process if possible, but it
must not silently serve mismatched log data or silently delete corrupt active
data as a v1 recovery strategy. Storage corruption events include the stream,
segment base offset, relevant data and/or index paths, and the corruption reason.

## Log Streaming API

`GetStatus` exposes `begin_offset` and `end_offset` for every child log stream:
stdout and stderr.

`GetLog` reads one source stream at a time: stdout or stderr.

`GetLog` supports both historical reads and live follow:

- request `begin_offset` to start at the oldest retained entry;
- request a retained entry offset to resume from that entry;
- request `end_offset` to wait for future entries;
- request `max_entries > 0` to stream until that many log entries have been
  delivered, then end the RPC;
- request `max_entries = 0` to follow forever until client cancellation.

Valid requested offsets are `begin_offset <= offset <= end_offset` for the
selected stream. If `offset < begin_offset` or `offset > end_offset`, `GetLog`
returns `OutOfRange`.

When `max_entries > 0`, the server streams entries already available at the
requested offset, then continues streaming entries as they materialize until the
requested count has been reached or the client cancels.

When `max_entries = 0`, the server streams entries as soon as they are available
and keeps the stream open until the client cancels.

There is no separate historical-only mode in v1 and no `follow` boolean. Clients
that want currently available entries only should use their own RPC deadline or
cancellation strategy. The server does not impose a v1 idle timeout on log
streams.

Responses may batch multiple log entries for efficiency. Each `LogEntry` carries
its own offset, so batching is a transport detail. The server should
opportunistically batch adjacent available entries without intentional delay. If
one read from the child process yields multiple complete accepted log entries,
active matching `GetLog` streams may receive those entries in one response.

Each `GetLogResponse` contains at most 1024 entries and should stay within the
default 4 MB gRPC message size limit. If either limit would be exceeded, the
server sends a smaller batch. If more entries are immediately available, the
server sends another response without intentional delay.

`GetLog` does not clamp `max_entries`. Very large values are allowed; individual
response batching limits constrain each streamed message, and the client controls
stream cancellation.

Runner does not pin log data for active readers in v1. Rotation may continue
while a `GetLog` stream is active. If an unread entry needed by an active stream
rotates out before it is delivered, the stream fails with `OutOfRange`.

The existing proto field named `limit` should be renamed to `max_entries` to
make it explicit that the limit counts entries, not bytes.

## Configuration

Runner is configured with YAML. YAML is the preferred v1 format because it is
easy to express a command and arguments without shell quoting and can represent
nested log policy cleanly.

Configuration uses grouped top-level sections:

```yaml
server:
  listen_address: "127.0.0.1:9090"

process:
  command: ["./agent", "--config", "agent.yaml"]
  restart_delay: "5s"
  graceful_shutdown_timeout: "30s"

logs:
  directory: "./runner-logs"
  max_entry_size: "16KiB"
  stdout:
    disk_budget: "128MiB"
  stderr:
    disk_budget: "128MiB"
  encoding:
    plain_text: {}

events:
  retained_count: 1024
```

For structured logs, `logs.encoding` selects the structured variant:

```yaml
logs:
  directory: "./runner-logs"
  encoding:
    structured:
      format: "slog_json"
```

Required fields:

- `server.listen_address`;
- `process.command`;
- `logs.directory`.

Defaulted fields:

- `process.restart_delay`: `5s`;
- `process.graceful_shutdown_timeout`: `30s`;
- `logs.max_entry_size`: `16KiB`;
- `logs.stdout.disk_budget`: `128MiB`;
- `logs.stderr.disk_budget`: `128MiB`;
- `logs.encoding`: `plain_text`;
- `events.retained_count`: `1024`.

The `events`, `logs.stdout`, and `logs.stderr` sections are optional when all of
their fields use defaults.

`process.command` is a required non-empty array of non-empty strings. It is
executed directly without shell interpretation. `command[0]` uses normal OS
`PATH` lookup when it does not contain a path separator. Arguments are passed
literally exactly as configured.

`server.listen_address` is required and has no default. Runner does not assume
loopback or all-interface binding.

`logs.directory` is required. It may be absolute or relative. Relative paths are
resolved against runner's current working directory. Runner creates the directory
if it does not exist. If it exists but is not a directory, cannot be created, or
is not writable, startup validation fails before launching the child process.

All YAML string values are literal. Runner does not perform shell interpolation,
globbing, tilde expansion, or environment variable expansion.

Durations use Go duration syntax only, such as `100ms`, `5s`, `2m`, `1h`, or
`1m30s`. Bare integers and human-word durations such as `5 seconds` are invalid.

Size strings use explicit binary units with no spaces and case-sensitive units.
The accepted units are `B`, `KiB`, `MiB`, and `GiB`. Amounts are non-negative
integers. Examples of valid size strings are `0B`, `1024B`, `16KiB`, `128MiB`,
and `1GiB`. Bare integers, decimal units such as `MB`, lowercase units, strings
with spaces such as `128 MiB`, and fractional values such as `1.5MiB` are
invalid. The generic size parser accepts `0B`; individual field validation then
decides whether zero is allowed.

Default configuration values include:

- process restart delay: 5 seconds;
- graceful shutdown timeout: 30 seconds;
- stdout disk budget: 128MiB;
- stderr disk budget: 128MiB;
- maximum log entry size: 16KiB;
- retained runner event count: 1024.

Validation rules include:

- process restart delay must be between 100ms and 1h, inclusive;
- graceful shutdown timeout must be between 0s and 10m, inclusive;
- stdout and stderr disk budgets must each be at least 16MiB;
- maximum log entry size must be between 1KiB and 1MiB, inclusive;
- retained runner event count must be between 1 and 65536, inclusive.

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
- add `offset`, `truncated`, and optional structured source location to
  `LogEntry`;
- keep `GetLogResponse` as `repeated LogEntry entries` and do not require batch
  `begin_offset` or `end_offset` fields;
- use a `oneof` request mode for `GetEventsRequest` with `uint64
  from_sequence_number` and `uint64 last_count`;
- include `next_sequence_number` in `GetEventsResponse`;
- keep stdout and stderr as the only child log sources;
- model runner events separately from child log entries.

`GetLog` should use gRPC status codes consistently:

- `InvalidArgument` for unknown log source or malformed request values;
- `OutOfRange` for a requested or needed offset outside the retained range;
- `Canceled` when the client cancels a streaming request;
- `Internal` for unexpected storage or indexing failures.

`OutOfRange` errors from `GetLog`, including mid-stream rotation errors, should
include structured range details: source stream, requested or needed offset,
current `begin_offset`, and current `end_offset`.
