package runner

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrTimeout reports that a command exceeded its configured runtime.
	ErrTimeout = errors.New("command timed out")
	// ErrOutputLimit reports that stdout or stderr exceeded its configured cap.
	ErrOutputLimit = errors.New("command output cap exceeded")
	// ErrDocker reports that the container runtime failed before a command
	// produced a normal process result.
	ErrDocker = errors.New("docker failure")
	// ErrCancellation reports that the caller canceled command execution.
	ErrCancellation = context.Canceled
)

// Env is one environment variable passed to a command execution.
//
// Key must be non-empty and must not contain '=' after service-level validation.
// Value is passed verbatim to the runner.
type Env struct {
	// Key is the environment variable name.
	Key string
	// Value is the environment variable value.
	Value string
}

// Request describes one command execution inside a session workspace.
//
// Service-level validation must ensure SessionID is active, Workspace is the
// trusted workspace path, Cmd is non-empty, Env keys are valid, and limits are
// positive before calling a Runner.
type Request struct {
	// SessionID labels the execution and any runtime resources created for it.
	SessionID string
	// Workspace is the host directory mounted as the command working directory.
	Workspace string
	// Cmd contains the executable followed by arguments. It is not interpreted
	// by a shell unless the caller explicitly includes one.
	Cmd []string
	// Env contains environment variables to pass to the process.
	Env []Env
	// Timeout is the maximum runtime before cancellation and cleanup.
	Timeout time.Duration
	// StdoutCap is the maximum captured stdout bytes.
	StdoutCap int64
	// StderrCap is the maximum captured stderr bytes.
	StderrCap int64
	// ContainerImage is the requested container image; runners may apply their
	// own default when it is empty.
	ContainerImage string
}

// Result is the normal outcome of a command that started successfully.
//
// Non-zero process exits are represented as Results with a non-zero ExitCode,
// not as errors. Errors are reserved for setup, timeout, cancellation, cleanup,
// and output-limit failures.
type Result struct {
	// ExitCode is the process exit code returned by the command.
	ExitCode int
	// Stdout is captured standard output up to the configured cap.
	Stdout string
	// Stderr is captured standard error up to the configured cap.
	Stderr string
}

// Runner executes commands for the service.
//
// Run must honor ctx cancellation, req.Timeout, workspace isolation, and output
// caps according to the implementation. It returns a Result for commands that
// start and exit normally, including non-zero exits, or a sentinel error for
// timeout, cancellation, runtime setup, or output-limit failures.
type Runner interface {
	// Run executes req and returns either a normal process Result or an error
	// describing why no valid result is available.
	Run(ctx context.Context, req Request) (Result, error)
}

// Fake is a test runner whose behavior can be supplied by RunFunc.
//
// It is safe to copy when RunFunc is immutable. If RunFunc is nil, Run returns a
// successful default result with stdout "ok\n".
type Fake struct {
	// RunFunc, when non-nil, receives the context and request supplied to Run and
	// returns the desired fake result or error.
	RunFunc func(context.Context, Request) (Result, error)
}

// Run implements Runner for Fake.
//
// It delegates to RunFunc when set. Otherwise it returns exit code 0, stdout
// "ok\n", empty stderr, and nil error. It does not inspect req or ctx unless
// RunFunc does.
func (f Fake) Run(ctx context.Context, req Request) (Result, error) {
	if f.RunFunc != nil {
		return f.RunFunc(ctx, req)
	}
	return Result{ExitCode: 0, Stdout: "ok\n"}, nil
}
