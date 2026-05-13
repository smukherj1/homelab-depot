# Supervisor Simplification Alignment Notes

This document captures the current findings from comparing the simplified
`internal/process/supervisor.go` implementation against `docs/runner.md` and
Phase 7 of `docs/implementation-plan.md`.

No code changes should be made from these notes until we fully align on the
intended supervisor semantics. The next step is to decide which differences are
intentional simplifications that should be reflected in the docs and tests, and
which differences are implementation bugs that should be fixed.

## Likely Intentional Simplifications

### Asynchronous Start

`Supervisor.Start(ctx)` now starts the supervision loop and returns before the
first child process launch has necessarily succeeded or failed.

Current behavior:

- `Start` validates that the supervisor has not already started.
- `Start` records `runner.startup`.
- `Start` launches the supervisor loop in a goroutine.
- Child launch errors are recorded later as `process.start_failed` events.
- `Start` usually returns nil even when the configured child command cannot be
  executed.

This differs from the current Phase 7 test expectation that child start errors
are returned from `Start` with command context.

If this simplification is accepted, docs and tests should say that `Start`
means "the supervisor loop started", not "the child process started". Child
process launch failures become lifecycle events rather than synchronous API
errors.

### Retry After Start Failure

The simplified loop keeps retrying after `cmd.Start()` fails.

Current behavior:

- A failed child launch records `process.start_failed`.
- The loop waits the configured restart delay.
- The loop attempts to start the child again.

This is consistent with the broader product goal that the runner attempts to
keep the child running while it is alive and not intentionally shutting down.
However, the behavior should be explicitly documented and covered by tests if
we keep it.

## Likely Implementation Bugs

These behaviors appear to violate the existing product docs rather than merely
simplify them.

### Shutdown Schedules Restart

During intentional shutdown, the current loop can still run its restart-delay
post-step after observing the child exit. That increments `RestartCount` and
records `process.restart_scheduled`.

This conflicts with `docs/runner.md`, which says intentional shutdown:

- sends graceful termination to the child;
- waits for the graceful shutdown timeout;
- force-kills if needed;
- does not restart the child.

Recommended alignment: keep the existing product behavior and fix the
implementation so shutdown exits do not count as restarts and do not emit
restart-scheduled events.

### Command Context Bypasses Graceful Shutdown

The child is created with the supervisor loop context through
`exec.CommandContext`. `beginShutdown()` cancels that loop context before sending
SIGTERM. In Go, canceling the context associated with `exec.CommandContext`
kills the process, so the configured graceful SIGTERM wait can be bypassed.

Observed result in tests:

- cooperative shutdown reports `signal=killed`;
- uncooperative shutdown does not reach the explicit force-kill path;
- `process.force_kill` is not recorded.

Recommended alignment: preserve the documented shutdown sequence. The loop
needs cancellation for restart scheduling and startup control, but cancellation
should not itself be the child-kill mechanism during intentional shutdown.

### Fast Exit Can Produce Log Capture Failures

`captureAndWait` calls `cmd.Wait()` before the stdout and stderr capture
goroutines finish. `cmd.Wait()` can close the pipes while capture goroutines are
still reading, which can surface as a log capture failure for a normal fast
exit.

Observed event:

- `process.log_capture_failed` with a reason like `read stdout: read |0: file
already closed`

Recommended alignment: normal child exit should not produce log-capture failure
events. stdout and stderr capture should drain reliably for quick-exiting
children.

### Shutdown Context Is Ignored

`Shutdown(ctx)` accepts a context, but the wait path currently uses only
`time.After(s.opts.GracefulShutdownTimeout)`. The caller's context does not
bound shutdown.

The docs do not currently specify the method-level Go API contract, but this is
surprising for a method that accepts a context. We should decide whether to
document this as unsupported or fix the implementation to respect caller
cancellation.

## Current Failing Tests

`go test ./...` currently fails only in `internal/process`.

### Keep or Adapt: `TestShutdownCooperativeChildAvoidsRestart`

Current failure:

- `RestartCount = 1`, expected `0`.

Recommended action: keep this expectation if the product docs stay as-is.
Intentional shutdown should not count as a restart and should not schedule a
restart.

### Keep: `TestShutdownForceKillsUncooperativeChild`

Current failure:

- no `process.force_kill` event is recorded.

Recommended action: keep this expectation. It catches the regression where
context cancellation kills the child before the explicit graceful-timeout path
can force-kill and record the event.

### Replace: `TestStartErrorIsWrappedAndRecorded`

Current failure:

- `Start()` returns nil when the child command cannot be executed.

Recommended action if async `Start` is accepted:

- remove the expectation that `Start` returns the child launch error;
- add a test that `Start` returns nil once the supervisor loop starts;
- verify `process.start_failed` is recorded asynchronously;
- verify the supervisor remains responsive and can be shut down cleanly.

## Tests To Add After Alignment

Add or update focused process tests for the accepted semantics:

- Shutdown while a child is running does not record
  `process.restart_scheduled`.
- Shutdown while a child is running does not increment `RestartCount`.
- A cooperative child receives SIGTERM and exits before the force-kill timeout.
- A child that ignores SIGTERM is force-killed after
  `GracefulShutdownTimeout`, and `process.force_kill` is recorded.
- A fast-exiting child that writes stdout and stderr does not produce
  `process.log_capture_failed`.
- `Shutdown` before `Start` returns promptly.
- `Shutdown(ctx)` respects caller cancellation, if we decide that is part of
  the API contract.
- Start failure retry records `process.start_failed` asynchronously and can be
  stopped cleanly.

## Proposed Alignment Direction

Accept these simplifications:

- `Start` starts the supervisor loop asynchronously and does not synchronously
  prove the child can be launched.
- Start failures are lifecycle events, not `Start` return errors.
- Start failures are retried using the configured restart delay.

Fix these implementation issues:

- intentional shutdown must not schedule restarts or increment restart count;
- graceful shutdown must not be bypassed by `exec.CommandContext`
  cancellation;
- normal fast child exit should not produce log capture failures;
- decide and then implement or document `Shutdown(ctx)` cancellation behavior;
- eliminate the concurrent `Start`/`Shutdown` race or narrow the concurrency
  contract.

After we align on this direction, update `docs/runner.md`,
`docs/implementation-plan.md`, and the process tests before making supervisor
code changes.
