package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/smukherj/homelab-depot/runner/internal/events"
	"github.com/smukherj/homelab-depot/runner/internal/logparse"
	"github.com/smukherj/homelab-depot/runner/internal/logstore"
)

type fakeStore struct {
	mu      sync.Mutex
	entries []logparse.Entry
	endIDs  map[logparse.Source]uint64
}

func newFakeStore() *fakeStore {
	return &fakeStore{endIDs: map[logparse.Source]uint64{
		logparse.SourceStdout: 0,
		logparse.SourceStderr: 0,
	}}
}

func (f *fakeStore) Append(_ context.Context, entry logparse.Entry) (logstore.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.endIDs[entry.Source]
	f.endIDs[entry.Source] = id + 1
	f.entries = append(f.entries, entry)
	return logstore.Entry{ID: id, Source: entry.Source, Message: entry.Message, Level: entry.Level}, nil
}

func (f *fakeStore) EndID(source logstore.Source) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.endIDs[source]
}

func (f *fakeStore) snapshot() []logparse.Entry {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]logparse.Entry, len(f.entries))
	copy(out, f.entries)
	return out
}

func TestCommandRunsWithoutShellExpansionAndRoutesStreams(t *testing.T) {
	store := newFakeStore()
	ring := newTestRing(t)
	sup := newTestSupervisor(t, store, ring, "emit", "$PROCESS_SUPERVISOR_TEST_VALUE")
	t.Setenv("PROCESS_SUPERVISOR_TEST_VALUE", "expanded")

	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v, want nil", err)
	}
	defer shutdownSupervisor(t, sup)

	waitFor(t, 5*time.Second, func() bool {
		return len(store.snapshot()) >= 2
	})
	entries := store.snapshot()
	var stdout, stderr string
	for _, entry := range entries {
		switch entry.Source {
		case logparse.SourceStdout:
			stdout += entry.Message
		case logparse.SourceStderr:
			stderr += entry.Message
		}
	}
	if !strings.Contains(stdout, "arg=$PROCESS_SUPERVISOR_TEST_VALUE\n") {
		t.Errorf("stdout = %q, want literal unexpanded argument", stdout)
	}
	if !strings.Contains(stderr, "stderr-line\n") {
		t.Errorf("stderr = %q, want stderr captured separately", stderr)
	}
}

func TestExitCodeZeroSchedulesRestartAndCountsRestart(t *testing.T) {
	store := newFakeStore()
	ring := newTestRing(t)
	sup := newTestSupervisor(t, store, ring, "exit", "0")
	sup.opts.RestartDelay = 10 * time.Millisecond

	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v, want nil", err)
	}
	defer shutdownSupervisor(t, sup)

	waitFor(t, 5*time.Second, func() bool {
		return sup.Status().RestartCount >= 1
	})
	status := sup.Status()
	if status.LastExitCode == nil || *status.LastExitCode != 0 {
		t.Fatalf("LastExitCode = %v, want 0", status.LastExitCode)
	}
	if status.RestartCount < 1 {
		t.Errorf("RestartCount = %d, want at least 1 after exit and scheduled restart", status.RestartCount)
	}
	if !hasEvent(t, ring, eventRestartScheduled) {
		t.Errorf("events did not include %s", eventRestartScheduled)
	}
}

func TestNonZeroExitPopulatesStatus(t *testing.T) {
	store := newFakeStore()
	ring := newTestRing(t)
	sup := newTestSupervisor(t, store, ring, "exit", "17")
	sup.opts.RestartDelay = time.Second

	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v, want nil", err)
	}
	defer shutdownSupervisor(t, sup)

	waitFor(t, time.Second, func() bool {
		code := sup.Status().LastExitCode
		return code != nil && *code == 17
	})
	status := sup.Status()
	if status.Running {
		t.Errorf("Running = true, want false while waiting for restart delay")
	}
	if status.LastExitSignal != nil {
		t.Errorf("LastExitSignal = %q, want nil for ordinary exit", *status.LastExitSignal)
	}
}

func TestRestartStartFailureKeepsSupervisorResponsive(t *testing.T) {
	store := newFakeStore()
	ring := newTestRing(t)
	sup := newTestSupervisor(t, store, ring, "exit", "0")
	sup.opts.RestartDelay = 10 * time.Millisecond

	var mu sync.Mutex
	starts := 0
	sup.factory = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		mu.Lock()
		starts++
		start := starts
		mu.Unlock()
		if start == 1 {
			return exec.CommandContext(ctx, name, args...)
		}
		return exec.CommandContext(ctx, "/path/that/does/not/exist")
	}

	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v, want nil", err)
	}
	defer shutdownSupervisor(t, sup)

	waitFor(t, 5*time.Second, func() bool {
		return hasEvent(t, ring, eventStartFailed)
	})
}

func TestSignalTerminationPopulatesStatus(t *testing.T) {
	store := newFakeStore()
	ring := newTestRing(t)
	sup := newTestSupervisor(t, store, ring, "sleep")

	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v, want nil", err)
	}
	waitFor(t, 5*time.Second, func() bool { return sup.Status().Running })

	sup.mu.Lock()
	cmd := sup.cmd
	sup.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		t.Fatal("child process was not running")
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("Signal(SIGTERM) error = %v, want nil", err)
	}
	defer shutdownSupervisor(t, sup)

	waitFor(t, 5*time.Second, func() bool {
		return sup.Status().LastExitSignal != nil
	})
	if got := *sup.Status().LastExitSignal; got == "" {
		t.Errorf("LastExitSignal = empty, want signal name")
	}
}

func TestShutdownCooperativeChildAvoidsRestart(t *testing.T) {
	store := newFakeStore()
	ring := newTestRing(t)
	sup := newTestSupervisor(t, store, ring, "sleep")
	sup.opts.GracefulShutdownTimeout = time.Second

	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v, want nil", err)
	}
	waitFor(t, 5*time.Second, func() bool { return sup.Status().Running })

	if err := sup.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v, want nil", err)
	}
	status := sup.Status()
	if status.Running {
		t.Errorf("Running = true, want false after shutdown")
	}
	if status.RestartCount != 0 {
		t.Errorf("RestartCount = %d, want 0 because intentional shutdown does not restart", status.RestartCount)
	}
	if !hasEvent(t, ring, eventGracefulTermination) {
		t.Errorf("events did not include %s", eventGracefulTermination)
	}
}

func TestShutdownForceKillsUncooperativeChild(t *testing.T) {
	store := newFakeStore()
	ring := newTestRing(t)
	sup := newTestSupervisor(t, store, ring, "ignore-term")
	sup.opts.GracefulShutdownTimeout = 20 * time.Millisecond

	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v, want nil", err)
	}
	waitFor(t, 5*time.Second, func() bool { return len(store.snapshot()) >= 1 })

	if err := sup.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v, want nil", err)
	}
	if !hasEvent(t, ring, eventForceKill) {
		t.Errorf("events did not include %s", eventForceKill)
	}
}

func TestStartErrorIsWrappedAndRecorded(t *testing.T) {
	store := newFakeStore()
	ring := newTestRing(t)
	sup, err := New(Options{
		Command:                 []string{"/path/that/does/not/exist"},
		WorkingDirectory:        t.TempDir(),
		RestartDelay:            time.Second,
		GracefulShutdownTimeout: time.Second,
		Encoding:                logparse.EncodingPlainText,
		MaxEntryBytes:           1024,
		Events:                  ring,
		Store:                   store,
	})
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}

	err = sup.Start(context.Background())
	if err == nil {
		t.Fatal("Start() error = nil, want start failure")
	}
	if !strings.Contains(err.Error(), `start command "/path/that/does/not/exist"`) {
		t.Errorf("Start() error = %v, want command context", err)
	}
	if !hasEvent(t, ring, eventStartFailed) {
		t.Errorf("events did not include %s", eventStartFailed)
	}
}

func newTestSupervisor(t *testing.T, store *fakeStore, ring *events.Ring, mode string, args ...string) *Supervisor {
	t.Helper()
	t.Setenv("PROCESS_SUPERVISOR_HELPER", "1")
	command := []string{os.Args[0], "-test.run", "^$", "--", mode}
	command = append(command, args...)
	sup, err := New(Options{
		Command:                 command,
		WorkingDirectory:        t.TempDir(),
		RestartDelay:            time.Second,
		GracefulShutdownTimeout: time.Second,
		Encoding:                logparse.EncodingPlainText,
		MaxEntryBytes:           1024,
		Events:                  ring,
		Store:                   store,
	})
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	return sup
}

func newTestRing(t *testing.T) *events.Ring {
	t.Helper()
	ring, err := events.NewRing(128, 4096, time.Now)
	if err != nil {
		t.Fatalf("events.NewRing() error = %v, want nil", err)
	}
	return ring
}

func shutdownSupervisor(t *testing.T, sup *Supervisor) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := sup.Shutdown(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Shutdown() error = %v, want nil", err)
	}
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition was not satisfied before timeout")
}

func hasEvent(t *testing.T, ring *events.Ring, code string) bool {
	t.Helper()
	snapshot, err := ring.Query(context.Background(), events.Query{Mode: events.QueryAll})
	if err != nil {
		t.Fatalf("ring.Query() error = %v, want nil", err)
	}
	for _, event := range snapshot.Events {
		if event.Code == code {
			return true
		}
	}
	return false
}

func TestMain(m *testing.M) {
	if os.Getenv("PROCESS_SUPERVISOR_HELPER") == "1" {
		os.Exit(processHelperMain(os.Args))
	}
	os.Exit(m.Run())
}

func processHelperMain(args []string) int {
	separator := -1
	for i, arg := range args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator == -1 || separator+1 >= len(args) {
		return 2
	}
	mode := args[separator+1]
	helperArgs := args[separator+2:]
	switch mode {
	case "emit":
		arg := ""
		if len(helperArgs) > 0 {
			arg = helperArgs[0]
		}
		fmt.Fprintf(os.Stdout, "arg=%s\n", arg)
		fmt.Fprint(os.Stderr, "stderr-line\n")
	case "exit":
		code := 0
		if len(helperArgs) > 0 {
			parsed, err := strconv.Atoi(helperArgs[0])
			if err != nil {
				fmt.Fprintf(os.Stderr, "bad exit code: %v\n", err)
				return 2
			}
			code = parsed
		}
		return code
	case "sleep":
		time.Sleep(10 * time.Second)
	case "ignore-term":
		signalIgnored()
		fmt.Fprint(os.Stdout, "ready\n")
		select {}
	default:
		return 2
	}
	return 0
}

func signalIgnored() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM)
	go func() {
		for range ch {
		}
	}()
}
