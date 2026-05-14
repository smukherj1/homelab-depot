# Supervisor Simplification Alignment Notes

This document captures the agreed supervisor semantics after comparing the
simplified `internal/process/supervisor.go` implementation against
`docs/runner.md` and Phase 7 of `docs/implementation-plan.md`.

This is the planned change list for the next docs, code, and test pass. Product
docs and implementation docs still need to be updated to match this alignment
before the process supervisor is finalized.

## Accepted Semantics

### Asynchronous Start

`Supervisor.Start(ctx)` starts the supervision loop and returns before the first
child process launch has necessarily succeeded or failed.

Accepted behavior:

- `Start` validates that the supervisor has not already started or stopped.
- `Start` records `runner.startup`.
- `Start` launches the supervisor loop in a goroutine.
- `Start` returns nil once the supervisor loop has been started.
- Child launch errors are recorded later as `process.start_failed` events.
- `Start` usually returns nil even when the configured child command cannot be
  executed.

Docs and tests should say that `Start` means "the supervisor loop started", not
"the child process started". Child process launch failures are lifecycle events
rather than synchronous API errors.

### Start Context

`Supervisor.Start(ctx)` keeps accepting a context. The supervisor derives its
internal loop context from the supplied context.

Accepted behavior:

- Canceling the start context stops future launch or restart attempts.
- Canceling the start context kills the currently running child through
  `exec.CommandContext`.
- `Shutdown()` uses the same internal cancellation path.

### Retry After Start Failure

The supervisor keeps retrying after `cmd.Start()` fails.

Accepted behavior:

- A failed child launch records `process.start_failed`.
- The failed launch attempt is reflected in `launch_attempt_count`.
- The loop records `process.restart_scheduled`.
- The loop waits the configured restart delay.
- The loop attempts to start the child again.

This matches the broader goal that runner attempts to keep the child running
while it is alive and not intentionally shut down.

### Launch Attempt Count

Replace `restart_count` with `launch_attempt_count` throughout the API,
internal status model, docs, and tests.

Accepted behavior:

- `launch_attempt_count` is the number of child launch attempts made by the
  supervisor loop.
- It is `0` before the first launch attempt.
- It becomes `1` for the initial launch attempt.
- It increments by one for every later retry after either a child exit or a
  launch failure.
- Start failures do not update `last_exit_time`, `last_exit_code`, or
  `last_exit_signal`, because no child process existed.

This avoids special casing failed launches while keeping exit status fields tied
to real child process lifetimes.

### Restart Scheduled Event

`process.restart_scheduled` is the generic fixed-delay retry event.

Accepted behavior:

- It is recorded after a successfully started child exits.
- It is recorded after a failed launch attempt.
- It means "another launch attempt has been scheduled after the configured
  restart delay".

`process.start_failed` still distinguishes launch failures from child exits.

### Shutdown

Remove graceful shutdown behavior from v1.

Accepted behavior:

- Remove `process.graceful_shutdown_timeout` from YAML config, config structs,
  defaults, validation, docs, and tests.
- Remove documented supervisor behavior that sends graceful termination, waits
  for a timeout, and force-kills later.
- Remove `process.graceful_termination` and `process.force_kill` from the
  documented event set and tests.
- `Shutdown()` intentionally cancels the internal supervisor context and waits
  for the supervisor loop to stop.
- `Shutdown()` has no caller cancellation path.
- `Shutdown()` returns nil promptly when called before `Start` and does not
  record `runner.shutdown` in that case.

Because shutdown uses the same cancellation path as `exec.CommandContext`, the
child is killed by context cancellation. If the loop observes a child exit
during shutdown and runs one final restart-delay step, it may record
`process.restart_scheduled`, increment `launch_attempt_count` for an additional
attempt, and immediately kill that attempt due to the canceled context. This is
accepted simplification.

### Concurrent Start And Shutdown

`Start` and `Shutdown` should be concurrency-safe, but the contract should stay
narrow.

Accepted behavior:

- It is safe to call `Start` and `Shutdown` concurrently.
- If shutdown wins before the loop starts, `Start` may return an error such as
  "supervisor is shutting down".
- If the loop starts, `Shutdown()` waits for it to stop.
- Exact event ordering is not guaranteed for simultaneous `Start` and
  `Shutdown` calls.

## Implementation Fixes To Keep

### Fast Exit Log Capture

Normal child exit must not produce log-capture failure events.

Current issue:

- `captureAndWait` calls `cmd.Wait()` before stdout and stderr capture
  goroutines finish.
- `cmd.Wait()` can close the pipes while capture goroutines are still reading.
- A normal quick exit can record `process.log_capture_failed` with a reason like
  `read stdout: read |0: file already closed`.

Planned fix:

- Keep the fix localized to `captureAndWait`, or at most a small helper used by
  that function.
- Start stdout and stderr capture goroutines.
- Call `cmd.Wait()`.
- Wait for both capture goroutines to finish before reporting capture errors.
- Treat normal EOF or pipe closure caused by process exit as successful drain.
- Continue recording real read, parse, or store errors as
  `process.log_capture_failed`.

## Planned Docs Changes

Update `docs/runner.md`:

- Replace `restart_count` with `launch_attempt_count`.
- Define launch attempts as all `cmd.Start()` attempts, including failed ones.
- Document asynchronous supervisor startup semantics if Go API behavior is
  described.
- Document retry after start failure.
- Document `process.restart_scheduled` as a retry-after-exit-or-start-failure
  event.
- Remove graceful shutdown timeout and graceful/force-kill behavior.
- Remove `process.graceful_termination` and `process.force_kill` from the event
  set.
- Update YAML examples and defaulted fields to remove
  `process.graceful_shutdown_timeout`.
- Update shutdown wording to describe context cancellation and no graceful
  sequence.

Update `docs/implementation-plan.md`:

- Phase 2: rename status field from `restart_count` to
  `launch_attempt_count`.
- Phase 3: remove graceful shutdown timeout parsing, defaults, validation, and
  boundary tests.
- Phase 7: replace graceful shutdown requirements with context-cancellation
  shutdown requirements.
- Phase 7: replace `RestartCount` tests with `LaunchAttemptCount` tests.
- Phase 7: replace synchronous start-error test expectations with asynchronous
  `process.start_failed` and retry expectations.
- Phase 7: remove force-kill and graceful-termination tests.
- Phase 7: add fast-exit log drain coverage.
- Phase 10: remove e2e expectations for cooperative graceful shutdown and
  uncooperative force-kill.

## Planned Code Changes

Update protobuf and generated code:

- Rename `restart_count` to `launch_attempt_count` in status messages.

Update `internal/config`:

- Remove `GracefulShutdownTimeout`.
- Remove `process.graceful_shutdown_timeout` from raw YAML structs.
- Remove default, min, max, parsing, validation, and tests for that field.

Update `internal/process`:

- Rename `RestartCount` to `LaunchAttemptCount`.
- Increment launch attempt count immediately before each `cmd.Start()` attempt.
- Keep `last_exit_*` fields updated only from real child exits.
- Keep `Start(ctx)` asynchronous.
- Keep start failures as `process.start_failed` events and retry after
  `process.restart_scheduled`.
- Change `Shutdown(ctx)` to `Shutdown()` if not already done.
- Remove graceful termination and force-kill code paths and event constants.
- Make pre-start `Shutdown()` a nil-returning no-op with no shutdown event.
- Keep concurrent `Start`/`Shutdown` safe under the narrow contract above.
- Fix `captureAndWait` so fast child exits drain stdout and stderr without
  spurious `process.log_capture_failed` events.

Update server/status mapping:

- Map internal `LaunchAttemptCount` to protobuf `launch_attempt_count`.
- Remove any remaining restart-count naming in tests and fixtures.

## Planned Tests

Update or replace current process tests:

- `Start` returns nil once the supervisor loop starts, without waiting for the
  child process to launch.
- A bad command records `process.start_failed` asynchronously.
- A bad command records `process.restart_scheduled` and retries after the fixed
  delay.
- `launch_attempt_count` increments for the initial launch attempt.
- `launch_attempt_count` increments for retries after child exits.
- `launch_attempt_count` increments for retries after failed launch attempts.
- Start failures do not update `last_exit_time`, `last_exit_code`, or
  `last_exit_signal`.
- Exit code `0` still schedules a retry.
- Non-zero exit and signal termination populate the correct status fields.
- Shutdown before `Start` returns promptly and records no `runner.shutdown`
  event.
- Shutdown after `Start` cancels the child and waits for the loop to stop.
- Shutdown may record one final `process.restart_scheduled` event and may
  increment `launch_attempt_count` for an additional canceled attempt.
- Concurrent `Start` and `Shutdown` are safe; tests should assert broad allowed
  outcomes rather than exact event ordering.
- A fast-exiting child that writes stdout and stderr does not produce
  `process.log_capture_failed`.
- stdout and stderr capture are still routed to the correct parser/store.

Remove or rewrite current process tests:

- Remove expectations that cooperative shutdown avoids restart scheduling.
- Remove expectations that shutdown preserves `RestartCount`.
- Remove tests for `process.graceful_termination`.
- Remove tests for `process.force_kill`.
- Remove tests for `GracefulShutdownTimeout`.
- Replace `TestStartErrorIsWrappedAndRecorded` with asynchronous start-failure
  retry coverage.

## Verification

After applying the planned docs and code changes, run:

```sh
make generate
make proto-lint
make proto-check
make test
make test-integration
make test-e2e
make build
```
