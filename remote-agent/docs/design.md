# Remote Agent v1 Design

## Overview and goals

The remote agent is a small Go service that runs on a remote machine and
executes caller-provided commands inside a Docker sandbox. Callers create a
session, upload files into that session workspace, execute commands against the
workspace, download results, inspect metadata, and complete the session to remove
all associated files.

The v1 implementation is intentionally narrow:

- Go for the service and CLI implementation.
- Make for local build, test, generation, and cleanup workflows.
- Protobuf and gRPC for the public API, with Buf managing proto linting and code
  generation.
- A loopback-only gRPC server. The agent is not safe to expose directly on a
  network interface without SSH tunneling, VPN access, or an authenticated proxy.
- A single active session at a time, with the client providing the `session_id`.
- Docker-based command execution using `ubuntu:26.04`.
- Hardened container defaults, bounded resource use, and best-effort cleanup on
  every success and failure path.
- Outbound networking enabled inside the container so commands can fetch
  dependencies or reach external services when needed.
- Fixed server-side limits for uploads, downloads, command runtime, and captured
  output.
- Unary `Execute`, with bounded stdout and stderr returned in the response.
- A CLI client, `agentctl`, for driving the same gRPC API used by tests and other
  callers.

The Go module path is `github.com/smukherj/homelab-depot/remote-agent`. The
repository root is the parent `homelab-depot` directory.

## Non-goals

v1 does not include:

- Multi-tenant isolation or multiple concurrent sessions.
- Authentication, authorization, TLS termination, or network exposure hardening.
- A hosted control plane.
- Agent container image publishing.
- Interactive command streaming, stdin streaming, or terminal emulation.
- Partial upload or download resume.
- User-configurable sandbox policies beyond the fixed server-side configuration.
- Long-term artifact storage after a session is completed or expires.

## Public API behavior

The public API is the `Agent` gRPC service defined in `proto/agent.proto`.

`GetStatus` is process-scoped and does not require a session. It returns the
number of active sessions, which is always `0` or `1` in v1.

Session-scoped RPCs require a valid `session_id` matching the active session:

- `CreateSession`
- `Upload`
- `Download`
- `GetPathMetadata`
- `Execute`
- `CompleteSession`
- `GetSession`

The server validates `session_id` before touching the session workspace. A
missing, malformed, unknown, or inactive session ID returns an explicit gRPC
error.

The API uses gRPC status codes consistently:

- `InvalidArgument` for malformed IDs, paths, upload streams, modes, commands,
  offsets, limits, or digests.
- `AlreadyExists` when `CreateSession` is called while another session is active.
- `NotFound` when the requested session or path does not exist.
- `FailedPrecondition` when the path exists but is not usable for the requested
  operation, such as downloading a directory.
- `ResourceExhausted` for fixed size and output limits.
- `DeadlineExceeded` for command timeout.
- `Canceled` when the client cancels the RPC.
- `Unavailable` for Docker daemon failures or unavailable local dependencies.
- `Internal` for unexpected server failures.

## Session lifecycle

The client provides the `session_id` in `CreateSessionRequest`. The server
validates the ID before creating any files.

Valid session IDs:

- are non-empty UTF-8 strings;
- are at most 128 bytes;
- contain only ASCII letters, digits, `.`, `_`, and `-`;
- do not begin with `.` or `-`;
- are not `.` or `..`.

The server maintains a single active session behind a mutex. If there is no
active session, `CreateSession` creates a private workspace directory under the
agent's runtime root. If any session is already active, `CreateSession` returns
`AlreadyExists`; this is true even if the requested ID matches the active
session. v1 does not make `CreateSession` idempotent.

All session-scoped RPCs update session activity after validating the session ID.
The session tracks:

- `created_at`
- `last_activity_at`
- active RPC count
- workspace path
- current state: active, completing, or completed

Idle expiry is based on `last_activity_at` plus the configured idle timeout. An
RPC increments the active RPC count before it begins session work and decrements
it when the RPC returns or is canceled. The janitor loop periodically scans the
active session and expires it only when:

- the session is active;
- active RPC count is `0`;
- `now - last_activity_at` exceeds the idle timeout.

The default idle timeout is 5 minutes. The janitor interval is fixed at 30
seconds for v1.

`CompleteSession` transitions the session to completing, rejects new
session-scoped work for that ID, removes the workspace, and clears the active
session. Cleanup is best-effort but must be attempted on explicit completion,
idle expiry, process shutdown, upload failure, command failure, and Docker
failure paths where temporary files or containers may remain.

## File upload, download, and metadata semantics

All file paths supplied by the client are relative to the session workspace.
The server rejects paths with:

- an empty string;
- an absolute path;
- `.` as the entire path or as any segment;
- `..` as any segment;
- empty path segments, including duplicate separators or trailing separators;
- platform path separators other than `/`;
- NUL bytes;
- any resolved path that escapes the workspace through symlinks.

The server resolves paths using `filepath.Clean` only after validating the raw
slash-separated path. For existing parent directories and existing target paths,
the server resolves symlinks and verifies that the final host path remains inside
the session workspace. Upload never follows a symlink for the destination file.

### Upload

`Upload` is client-streaming. The first stream message must contain
`UploadFileHeader`; all subsequent messages must contain `UploadFileChunk`.
Uploading multiple files requires multiple `Upload` RPCs.

The server enforces:

- valid active session ID on every stream message;
- exactly one header, before any chunk;
- valid relative filename;
- safe Unix mode validation;
- monotonically exact chunk offsets;
- configured max chunk size;
- configured max uploaded file size;
- final chunk presence;
- SHA-256 digest on the final chunk;
- digest match against the bytes received by the server.

Safe Unix mode validation accepts regular file permissions only. v1 accepts
`0000` through `0777` permission bits and rejects file type bits, setuid, setgid,
sticky bit, device bits, and values outside the permission mask. The server
applies the requested mode after the file is fully written and verified.

Upload writes to the destination path creating any intermediate directories as
necessary. If any validation, I/O, digest, cancellation, or session error occurs,
the partially uploaded file is left untouched. It's the caller's responsibility
to retry failed uploads. The server _may_ decide to delete the file that failed
to upload.

`UploadResponse.next_offset` returns the next expected offset on success. On
offset errors the RPC returns `InvalidArgument`; callers should start a new
upload because partial resume is not supported.

### Download

`Download` is server-streaming and supports regular files only. The requested
path must pass the same path validation rules used by upload. The server rejects
directories, symlinks that escape the workspace, non-regular files, and files
larger than the configured max download size.

Download responses contain ordered chunks starting at offset `0`. Each
subsequent chunk offset is the previous offset plus the previous data length.
The final chunk sets `final = true` and includes the SHA-256 digest for the file
contents transferred by the server. Partial resume and range download are not
supported.

It's undefined behavior for the client to attempt to download a file that's
currently being uploaded or wasn't uploaded successfully.

### Metadata

`GetPathMetadata` returns metadata for a file or directory inside the session.
The design requires named directory entries so callers can reconstruct immediate
children without inferring names from request paths.

The metadata shape should include:

- entry name;
- file type: regular file, directory, symlink, or other;
- size in bytes;
- Unix mode bits;
- modification time as Unix seconds plus nanoseconds, or an equivalent protobuf
  timestamp.

For a regular file response, metadata describes the requested file. For a
directory response, metadata includes immediate child entries only, sorted by
name for deterministic output. Directory traversal is not recursive in v1.

The current proto sketch may need to evolve to represent named file entries,
types, modes, and modification times. The implementation should update the proto
before generated code is committed.

## Command execution semantics

`Execute` is unary. The request contains the command and arguments in
`ExecuteRequest.cmd` and environment variables in `ExecuteRequest.exec_env`.

The server validates:

- active session ID;
- non-empty command list;
- non-empty executable string;
- configured max argument count and total argument bytes;
- environment keys are non-empty and do not contain `=`;
- configured max environment count and total environment bytes.

Commands run in a new Docker container with the session workspace mounted as the
container working directory. The command is not interpreted by a shell unless the
caller explicitly requests one, for example `["/bin/sh", "-c", "..."]`.

`ExecuteResponse` includes:

- the process exit code when the command starts and exits normally;
- captured stdout up to the configured stdout cap;
- captured stderr up to the configured stderr cap.

The server returns explicit errors instead of a normal `ExecuteResponse` when:

- the command cannot be started because Docker is unavailable or returns a setup
  failure;
- the command exceeds the configured timeout;
- stdout or stderr exceeds the configured cap;
- the client cancels the RPC;
- the session is completed or expires before execution starts;
- the container cannot be cleaned up after a setup or execution failure.

On timeout or cancellation, the server stops and removes the container before
returning. Cleanup failures should be logged and surfaced when they materially
affect isolation or resource reclamation.

## Docker sandbox policy

Each `Execute` call creates one short-lived Docker container from
`ubuntu:26.04`. Containers are never reused across executions.

The container policy is fixed in v1:

- no privileged mode;
- no host networking;
- no host PID, IPC, or UTS namespace sharing;
- all Linux capabilities dropped by default;
- no new privileges;
- read-only root filesystem when compatible with the workspace mount and command
  execution requirements;
- workspace mounted at a fixed container path, such as `/workspace`;
- container working directory set to the workspace mount;
- outbound networking enabled through Docker bridge networking;
- no inbound ports published;
- CPU, memory, process count, and runtime limits applied by the server;
- stdout and stderr captured by the agent with fixed caps;
- container name or labels include the session ID and execution ID for cleanup;
- container removed on success, failure, timeout, cancellation, and agent
  shutdown cleanup.

The workspace mount is the only host path mounted into the container. The agent
must not mount the Docker socket, host home directories, SSH keys, credentials,
or repository root unless those files were explicitly uploaded into the session
workspace by the client.

The default image is `ubuntu:26.04`. v1 does not expose image selection through
the public API.

## Configuration

Configuration is loaded on startup from flags and environment variables. Flags
take precedence over environment variables, which take precedence over defaults.

Required v1 settings:

- listen address, default `127.0.0.1:0` for tests and a documented loopback port
  for production use;
- workspace root, default under the OS temporary directory;
- session idle timeout, default 5 minutes;
- max upload file size;
- max upload chunk size;
- max download file size;
- download chunk size;
- command timeout;
- stdout cap;
- stderr cap;
- Docker image, fixed default `ubuntu:26.04`;
- Docker binary or client endpoint if the implementation needs it;
- janitor interval, fixed default 30 seconds.

All size, timeout, and address values are validated at startup. Invalid
configuration fails fast before the gRPC listener starts.

The server always binds to loopback for v1. Attempts to configure `0.0.0.0`,
`::`, a non-loopback IP, or a hostname that resolves outside loopback should be
rejected.

## Package and module layout

Paths are relative to `remote-agent/`.

```text
cmd/
  agent/
    main.go
  agentctl/
    main.go
internal/
  config/
  service/
  session/
  pathutil/
  transfer/
  docker/
  runner/
  cli/
gen/
  go/
proto/
  agent.proto
docs/
  design.md
Makefile
buf.yaml
buf.gen.yaml
go.mod
go.sum
```

Package responsibilities:

- `cmd/agent`: parse config, start the gRPC server, handle shutdown.
- `cmd/agentctl`: CLI entrypoint.
- `internal/config`: flags, environment variables, defaults, and validation.
- `internal/service`: gRPC service implementation and status/session RPC
  orchestration.
- `internal/session`: single-session lifecycle, idle expiry, workspace cleanup,
  and activity tracking.
- `internal/pathutil`: path validation, workspace containment checks, and safe
  open helpers.
- `internal/transfer`: upload, download, digest, chunk, and metadata behavior.
- `internal/runner`: command execution interface used by the service.
- `internal/docker`: Docker-backed runner implementation.
- `internal/cli`: reusable CLI client commands and output formatting.
- `gen/go`: generated protobuf and gRPC Go code.

## Build system and generated code workflow

Make is the developer entrypoint. Required targets:

- `make help`: list supported targets.
- `make generate`: run Buf generation.
- `make proto-lint`: run Buf lint.
- `make proto-check`: verify generated code is up to date.
- `make test`: run unit tests.
- `make test-integration`: run in-process gRPC integration tests.
- `make test-docker`: run Docker-backed integration tests.
- `make test-e2e`: run compiled binary end-to-end tests.
- `make build`: build `agent` and `agentctl`.
- `make clean`: remove local build artifacts.

Buf owns proto linting and code generation. `buf.yaml` defines the module and
lint rules. `buf.gen.yaml` generates Go protobuf and gRPC bindings under
`gen/go`. Generated code is committed so consumers can build without requiring
Buf, but CI verifies that committed generated code matches `proto/agent.proto`.

## CLI client

`agentctl` is the reference client for the gRPC API. It accepts an agent address
that must point at a loopback listener, SSH tunnel, VPN endpoint, or trusted
local proxy.

Initial commands:

- `agentctl status`
- `agentctl create-session SESSION_ID`
- `agentctl upload SESSION_ID LOCAL_PATH REMOTE_PATH`
- `agentctl download SESSION_ID REMOTE_PATH LOCAL_PATH`
- `agentctl metadata SESSION_ID REMOTE_PATH`
- `agentctl exec SESSION_ID -- CMD [ARG...]`
- `agentctl complete-session SESSION_ID`

The CLI should:

- stream file uploads and downloads using the same chunking rules as the API;
- compute and verify SHA-256 digests;
- preserve requested upload mode when provided;
- return non-zero exit status on gRPC errors, digest mismatch, command timeout,
  and command output cap errors;
- print command stdout and stderr predictably without hiding the gRPC error when
  execution fails before a process exit code exists.

## Error handling

Errors crossing the API boundary use gRPC status codes with stable, concise
messages. Internal logs may contain more detail, but API errors must not leak
host paths outside the workspace root, environment secrets, or Docker daemon
implementation details.

Validation should happen before mutation whenever possible. If an operation has
partially mutated state, the implementation must either complete an atomic
commit or clean up the partial state before returning.

Session cleanup should be idempotent. Repeated cleanup calls may log at debug
level but must not return spurious errors after the session is already gone.

## Security model

v1 protects the host from accidental or low-effort command escape, but it is not
a hardened multi-tenant sandbox.

Security boundaries and assumptions:

- The gRPC listener is loopback-only.
- Callers are trusted once they can reach the listener.
- Direct exposure to untrusted networks is unsupported.
- SSH tunneling, VPN access, or an authenticated local proxy is required for
  remote use.
- The Docker daemon and kernel are trusted computing base.
- Uploaded files and commands are untrusted with respect to the host filesystem.
- Docker sandboxing reduces but does not eliminate risk from malicious code.

The implementation must defend against:

- path traversal;
- symlink escape from the workspace;
- accidental host path mounts;
- unbounded disk, memory, CPU, process, runtime, stdout, and stderr usage;
- stale workspaces and containers;
- malformed upload and download streams.

The implementation does not defend against:

- a caller with access to the local gRPC socket intentionally running harmful
  commands within the allowed sandbox;
- Docker or kernel container escape vulnerabilities;
- secrets deliberately uploaded into the session by a caller;
- authenticated proxy or SSH account compromise.

## Testing strategy

Unit tests cover deterministic logic without Docker:

- config defaults, environment parsing, flag precedence, and invalid values;
- loopback listen-address validation;
- session ID validation;
- path validation and workspace containment checks;
- symlink escape rejection;
- single active session lifecycle;
- idle expiry using last-activity plus active-RPC tracking;
- janitor behavior with active and idle sessions;
- upload header ordering, missing header, duplicate header, chunk offsets, max
  file size, max chunk size, mode validation, digest mismatch, and atomic commit;
- download path validation, non-regular file rejection, max-size enforcement,
  chunk offsets, and digest generation;
- metadata for files, directories, symlinks, modes, sizes, names, and
  modification times;
- fake runner behavior for success, non-zero exit, timeout, cancellation, output
  cap breach, and setup failure.

Integration tests run an in-process gRPC server with fake runners and temporary
workspaces:

- full create/upload/metadata/download/complete flow;
- session collision and unknown session behavior;
- concurrent RPC activity preventing idle expiry;
- cancellation cleanup for upload, download, and execute;
- error-code mapping at the gRPC boundary;
- deterministic CLI-compatible stream behavior.

Docker integration tests run against the real Docker-backed runner:

- successful execution in `ubuntu:26.04`;
- workspace mount and working directory behavior;
- uploaded executable file execution;
- non-zero command exit code capture;
- stdout and stderr cap enforcement;
- command timeout cleanup;
- memory, CPU, and process limits where Docker supports them;
- outbound network availability from the container;
- no privileged mode, no host networking, and dropped capabilities;
- container removal on success, failure, timeout, and cancellation.

End-to-end tests use compiled `agent` and `agentctl` binaries:

- start the agent on a loopback port;
- create a session through `agentctl`;
- upload input files;
- execute a command that produces artifacts;
- inspect metadata;
- download and verify artifacts;
- complete the session;
- verify the workspace and containers are cleaned up.

## CI plan

GitHub Actions workflow location:

```text
.github/workflows/remote-agent.yml
```

The workflow lives at the monorepo root and is scoped to changes under:

- `remote-agent/**`
- `.github/workflows/remote-agent.yml`

CI always runs:

- Go formatting checks;
- `go test ./...` for unit and fake-runner integration tests;
- `make proto-lint`;
- `make proto-check`;
- `make build`.

Docker-backed tests run when Docker is available on the runner. They may be in a
separate job so failures clearly identify environment limitations versus unit
test regressions.

The initial CI should avoid publishing artifacts or container images. Agent
container image publishing is future work.

## Future work

- Authentication and TLS or documented integration with a specific local proxy.
- Multiple concurrent sessions.
- API-level image selection with an allowlist.
- Streaming execute logs.
- Stdin support.
- Upload and download resume.
- Quotas across total workspace disk usage.
- Structured execution result artifacts.
- Agent container image publishing.
- More restrictive network egress policy options.
