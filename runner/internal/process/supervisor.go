package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/smukherj/homelab-depot/runner/internal/events"
	"github.com/smukherj/homelab-depot/runner/internal/logparse"
	"github.com/smukherj/homelab-depot/runner/internal/logstore"
)

const (
	eventRunnerStartup       = "runner.startup"
	eventRunnerShutdown      = "runner.shutdown"
	eventChildStart          = "process.child_start"
	eventChildExit           = "process.child_exit"
	eventRestartScheduled    = "process.restart_scheduled"
	eventGracefulTermination = "process.graceful_termination"
	eventForceKill           = "process.force_kill"
	eventStartFailed         = "process.start_failed"
	eventLogCaptureFailed    = "process.log_capture_failed"
)

// Clock returns the current time for process status and lifecycle events.
type Clock func() time.Time

// EventRecorder records runner events. events.Ring satisfies this interface.
type EventRecorder interface {
	Record(events.Severity, string, string, map[string]string) events.Event
}

// LogStore accepts parsed child logs and exposes the current per-stream end ID
// for parser rejection events. logstore.Store satisfies this interface.
type LogStore interface {
	Append(context.Context, logparse.Entry) (logstore.Entry, error)
	EndID(logstore.Source) uint64
}

// CommandFactory constructs an exec.Cmd for the configured command. It exists
// so tests can inspect command construction; production callers should leave it
// nil to use exec.CommandContext without shell interpretation.
type CommandFactory func(context.Context, string, ...string) *exec.Cmd

// Options configures a one-child supervisor. Command must contain the program
// followed by arguments. WorkingDirectory, Events, and Store are borrowed by
// the supervisor and must remain valid until Shutdown returns.
type Options struct {
	Command                 []string
	WorkingDirectory        string
	RestartDelay            time.Duration
	GracefulShutdownTimeout time.Duration
	Encoding                logparse.Encoding
	MaxEntryBytes           uint64
	Events                  EventRecorder
	Store                   LogStore
	Clock                   Clock
	CommandFactory          CommandFactory
}

// Status is a point-in-time snapshot of the supervised process state. Pointer
// timestamp and exit fields are nil when unset.
type Status struct {
	Running          bool
	CurrentStartTime *time.Time
	LastExitTime     *time.Time
	LastExitCode     *int32
	LastExitSignal   *string
	RestartCount     uint64
	Command          []string
}

// Supervisor owns the lifecycle of one configured child process. Its methods
// are safe for concurrent use.
type Supervisor struct {
	opts    Options
	clock   Clock
	factory CommandFactory

	// Supervisor state.
	// Protects all state variables below.
	mu     sync.Mutex
	status Status
	// The currently running command. Nil when
	// the Supervisor hasn't yet started the command or
	// is waiting to restart the command.
	cmd *exec.Cmd
	// Whether the supervisor was asked to shutdown.
	stopping bool
	// Whether the super has started running the command.
	started bool

	// State of the supervisor loop that keeps restarting the managed process.
	loopCtx    context.Context
	cancelLoop context.CancelFunc
	done       chan struct{}
}

// New constructs a supervisor without launching the child process. The caller
// owns all dependencies passed in opts and must call Start followed by Shutdown
// to release the child process.
func New(opts Options) (*Supervisor, error) {
	if len(opts.Command) == 0 {
		return nil, errors.New("process command is required")
	}
	for i, arg := range opts.Command {
		if arg == "" {
			return nil, fmt.Errorf("process command argument %d must not be empty", i)
		}
	}
	if opts.Store == nil {
		return nil, errors.New("log store is required")
	}
	if opts.Encoding != logparse.EncodingPlainText && opts.Encoding != logparse.EncodingSlogJSON {
		return nil, fmt.Errorf("log encoding %q is invalid", opts.Encoding)
	}
	if opts.MaxEntryBytes == 0 {
		return nil, errors.New("max entry bytes must be greater than zero")
	}
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	factory := opts.CommandFactory
	if factory == nil {
		factory = exec.CommandContext
	}
	status := Status{Command: append([]string(nil), opts.Command...)}
	return &Supervisor{
		opts:    cloneOptions(opts),
		clock:   clock,
		factory: factory,
		status:  status,
		done:    make(chan struct{}),
	}, nil
}

// Start launches the configured child and starts the supervision loop. The
// first child start happens synchronously so startup errors are returned to the
// caller with command context and are also recorded as runner events.
func (s *Supervisor) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("process supervisor already started")
	}
	s.started = true
	s.loopCtx, s.cancelLoop = context.WithCancel(context.Background())
	s.mu.Unlock()

	s.record(events.SeverityInfo, eventRunnerStartup, "runner process supervisor started", nil)
	run, err := s.startChild(ctx)
	if err != nil {
		s.recordStartFailed(err)
		s.mu.Lock()
		s.stopping = true
		if s.cancelLoop != nil {
			s.cancelLoop()
		}
		close(s.done)
		s.mu.Unlock()
		return err
	}
	go s.loop(run)
	return nil
}

// Shutdown intentionally stops the supervised child, records shutdown events,
// waits for the supervision loop to finish, and prevents further restarts.
func (s *Supervisor) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return nil
	}
	if !s.stopping {
		// Signal the process to shutdown by cancelling its context and sending a
		// termination signal to the process itself.
		s.stopping = true
		if s.cancelLoop != nil {
			s.cancelLoop()
		}
		if s.cmd != nil && s.cmd.Process != nil {
			s.recordLocked(events.SeverityInfo, eventGracefulTermination, "terminating child process during runner shutdown", map[string]string{
				"pid": strconv.Itoa(s.cmd.Process.Pid),
			})
			_ = s.cmd.Process.Signal(syscall.SIGTERM)
		}
	}
	done := s.done
	timeout := s.opts.GracefulShutdownTimeout
	cmd := s.cmd
	s.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		// A process was currently running. We're waiting for the cancellation triggered above to
		// take effect.
		select {
		case <-done:
		case <-time.After(timeout):
			s.mu.Lock()
			if s.cmd == cmd && s.status.Running {
				s.recordLocked(events.SeverityWarn, eventForceKill, "force-killing child process after shutdown timeout", map[string]string{
					"pid": strconv.Itoa(cmd.Process.Pid),
				})
				_ = cmd.Process.Kill()
			}
			s.mu.Unlock()
			select {
			case <-done:
			case <-ctx.Done():
				return ctx.Err()
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	} else {
		// A process is not currently running and we're waiting for
		// the loop to shut down.
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.record(events.SeverityInfo, eventRunnerShutdown, "runner process supervisor stopped", nil)
	return nil
}

// Status returns a concurrency-safe snapshot of the current child process
// state.
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneStatus(s.status)
}

func (s *Supervisor) loop(run childRun) {
	defer close(s.done)
	for {
		result := s.waitChild(run)
		s.updateExit(result)

		s.mu.Lock()
		stopping := s.stopping
		s.mu.Unlock()
		if stopping {
			return
		}

		s.mu.Lock()
		s.status.RestartCount++
		s.mu.Unlock()
		s.record(events.SeverityInfo, eventRestartScheduled, "child process restart scheduled", map[string]string{
			"delay": s.opts.RestartDelay.String(),
		})
		select {
		case <-time.After(s.opts.RestartDelay):
		case <-s.loopCtx.Done():
			return
		}

		next, err := s.startChild(s.loopCtx)
		if err != nil {
			s.recordStartFailed(err)
			continue
		}
		run = next
	}
}

type childRun struct {
	cmd  *exec.Cmd
	done chan captureResult
}

type captureResult struct {
	// Error returned by the running command.
	cmdErr error
	// Errors while capturing logs from the running command.
	logErrs []error
}

func (s *Supervisor) startChild(ctx context.Context) (childRun, error) {
	cmd := s.factory(context.Background(), s.opts.Command[0], s.opts.Command[1:]...)
	cmd.Dir = s.opts.WorkingDirectory

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return childRun{}, fmt.Errorf("prepare stdout for command %q: %w", s.opts.Command[0], err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return childRun{}, fmt.Errorf("prepare stderr for command %q: %w", s.opts.Command[0], err)
	}
	if err := ctx.Err(); err != nil {
		return childRun{}, err
	}
	if err := cmd.Start(); err != nil {
		return childRun{}, fmt.Errorf("start command %q with args %v: %w", s.opts.Command[0], s.opts.Command[1:], err)
	}

	startTime := s.clock()
	s.mu.Lock()
	s.cmd = cmd
	s.status.Running = true
	s.status.CurrentStartTime = &startTime
	s.status.LastExitCode = nil
	s.status.LastExitSignal = nil
	s.mu.Unlock()
	s.record(events.SeverityInfo, eventChildStart, "child process started", map[string]string{
		"pid":     strconv.Itoa(cmd.Process.Pid),
		"command": s.opts.Command[0],
	})

	done := make(chan captureResult, 1)
	go s.captureAndWait(cmd, stdout, stderr, done)
	return childRun{cmd: cmd, done: done}, nil
}

// launches Go routines to capture stdout and stderr respectively and returns their results (containing any errors)
// in the given done channel.
func (s *Supervisor) captureAndWait(cmd *exec.Cmd, stdout io.Reader, stderr io.Reader, done chan<- captureResult) {
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := s.captureStream(stdout, logparse.SourceStdout); err != nil {
			errCh <- err
		}
	}()
	go func() {
		defer wg.Done()
		if err := s.captureStream(stderr, logparse.SourceStderr); err != nil {
			errCh <- err
		}
	}()

	waitErr := cmd.Wait()
	wg.Wait()
	close(errCh)
	var logErrs []error
	for err := range errCh {
		logErrs = append(logErrs, err)
		s.record(events.SeverityError, eventLogCaptureFailed, "child log capture failed", map[string]string{"reason": err.Error()})
	}
	done <- captureResult{cmdErr: waitErr, logErrs: logErrs}
}

func (s *Supervisor) captureStream(reader io.Reader, source logparse.Source) error {
	parser, err := logparse.NewStreamParser(logparse.Options{
		Source:        source,
		Encoding:      s.opts.Encoding,
		MaxEntryBytes: s.opts.MaxEntryBytes,
		Clock:         logparse.Clock(s.clock),
		Events:        s.opts.Events,
		CurrentEndID:  func(src logparse.Source) uint64 { return s.opts.Store.EndID(src) },
	})
	if err != nil {
		return err
	}

	// Background context is ok instead of the context passed in to the supervisor because the
	// command was launched with the supervisor context. Cancellation of the context will cancel
	// the running command and close its stdout/stderr streams that cause this stream capture to
	// end as well.
	ctx := context.Background()
	buf := make([]byte, 32*1024)
	for {
		n, readErr := reader.Read(buf)
		if n > 0 {
			if err := s.appendParsed(ctx, parser, buf[:n]); err != nil {
				return err
			}
		}
		if readErr == nil {
			continue
		}
		if !errors.Is(readErr, io.EOF) {
			return fmt.Errorf("read %s: %w", source, readErr)
		}
		// On EOF error, we close the parser and parse any remaining lines.
		entries, err := parser.Close()
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if _, err := s.opts.Store.Append(ctx, entry); err != nil {
				return fmt.Errorf("append %s log: %w", source, err)
			}
		}
		return nil
	}
}

func (s *Supervisor) appendParsed(ctx context.Context, parser *logparse.StreamParser, chunk []byte) error {
	entries, err := parser.Write(chunk)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if _, err := s.opts.Store.Append(ctx, entry); err != nil {
			return fmt.Errorf("append %s log: %w", entry.Source, err)
		}
	}
	return nil
}

func (s *Supervisor) waitChild(run childRun) captureResult {
	return <-run.done
}

func (s *Supervisor) updateExit(result captureResult) {
	exitTime := s.clock()
	code, signal := exitDetails(result.cmdErr)

	s.mu.Lock()
	if s.cmd != nil {
		s.cmd = nil
	}
	s.status.Running = false
	s.status.CurrentStartTime = nil
	s.status.LastExitTime = &exitTime
	s.status.LastExitCode = code
	s.status.LastExitSignal = signal
	s.mu.Unlock()

	details := map[string]string{}
	if code != nil {
		details["exit_code"] = strconv.Itoa(int(*code))
	}
	if signal != nil {
		details["signal"] = *signal
	}
	s.record(events.SeverityInfo, eventChildExit, "child process exited", details)
}

func exitDetails(err error) (*int32, *string) {
	if err == nil {
		code := int32(0)
		return &code, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				signal := status.Signal().String()
				return nil, &signal
			}
			code := int32(status.ExitStatus())
			return &code, nil
		}
	}
	code := int32(1)
	return &code, nil
}

func (s *Supervisor) recordStartFailed(err error) {
	s.record(events.SeverityError, eventStartFailed, "child process start failed", map[string]string{
		"command": s.opts.Command[0],
		"reason":  err.Error(),
	})
}

func (s *Supervisor) record(severity events.Severity, code string, message string, details map[string]string) {
	if s.opts.Events == nil {
		return
	}
	s.opts.Events.Record(severity, code, message, details)
}

func (s *Supervisor) recordLocked(severity events.Severity, code string, message string, details map[string]string) {
	if s.opts.Events == nil {
		return
	}
	s.opts.Events.Record(severity, code, message, details)
}

func cloneStatus(status Status) Status {
	status.Command = append([]string(nil), status.Command...)
	if status.CurrentStartTime != nil {
		v := *status.CurrentStartTime
		status.CurrentStartTime = &v
	}
	if status.LastExitTime != nil {
		v := *status.LastExitTime
		status.LastExitTime = &v
	}
	if status.LastExitCode != nil {
		v := *status.LastExitCode
		status.LastExitCode = &v
	}
	if status.LastExitSignal != nil {
		v := *status.LastExitSignal
		status.LastExitSignal = &v
	}
	return status
}

func cloneOptions(opts Options) Options {
	opts.Command = append([]string(nil), opts.Command...)
	return opts
}
