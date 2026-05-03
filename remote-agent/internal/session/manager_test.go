package session

import (
	"errors"
	"os"
	"testing"
	"time"
)

type fakeClock struct{ now time.Time }

func (f *fakeClock) Now() time.Time { return f.now }

func TestValidateID(t *testing.T) {
	valid := []string{"session_1", "abc.def", "A-1"}
	for _, id := range valid {
		if err := ValidateID(id); err != nil {
			t.Fatalf("ValidateID should accept valid session ID %q, got error %v", id, err)
		}
	}
	invalid := []string{"", ".", "..", ".hidden", "-bad", "has space", "slash/name"}
	for _, id := range invalid {
		if err := ValidateID(id); err == nil {
			t.Fatalf("ValidateID should reject invalid session ID %q, but it succeeded", id)
		}
	}
}

func TestSingleActiveSessionAndComplete(t *testing.T) {
	mgr, err := NewManager(t.TempDir(), time.Minute)
	if err != nil {
		t.Fatalf("NewManager should create a manager rooted in the test temp dir: %v", err)
	}
	s, err := mgr.Create("s1")
	if err != nil {
		t.Fatalf("Create should accept first valid session ID and create workspace: %v", err)
	}
	if _, err := os.Stat(s.Workspace); err != nil {
		t.Fatalf("Create should create workspace %q, stat failed with %v", s.Workspace, err)
	}
	if _, err := mgr.Create("s2"); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("Create should reject a second active session with ErrAlreadyExists, got %v", err)
	}
	if err := mgr.Complete("s1"); err != nil {
		t.Fatalf("Complete should remove the active session s1, got error %v", err)
	}
	if _, err := os.Stat(s.Workspace); !os.IsNotExist(err) {
		t.Fatalf("Complete should remove workspace %q, stat error should be IsNotExist, got %v", s.Workspace, err)
	}
}

func TestExpireIdleRespectsActiveRPCs(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	mgr, err := NewManagerWithClock(t.TempDir(), time.Minute, clock)
	if err != nil {
		t.Fatalf("NewManagerWithClock should create a manager with the fake clock: %v", err)
	}
	if _, err := mgr.Create("s1"); err != nil {
		t.Fatalf("Create should start session s1 for idle-expiry test: %v", err)
	}
	lease, err := mgr.Acquire("s1")
	if err != nil {
		t.Fatalf("Acquire should reserve active session s1 before idle expiry: %v", err)
	}
	clock.now = clock.now.Add(2 * time.Minute)
	if err := mgr.ExpireIdle(); err != nil {
		t.Fatalf("ExpireIdle should not error while an active RPC lease prevents expiry: %v", err)
	}
	if mgr.ActiveCount() != 1 {
		t.Fatalf("ExpireIdle should keep session active while ActiveRPCs > 0, active count got %d", mgr.ActiveCount())
	}
	lease.Done()
	clock.now = clock.now.Add(2 * time.Minute)
	if err := mgr.ExpireIdle(); err != nil {
		t.Fatalf("ExpireIdle should complete idle session after lease release: %v", err)
	}
	if mgr.ActiveCount() != 0 {
		t.Fatalf("ExpireIdle should remove idle session after timeout with no active RPCs, active count got %d", mgr.ActiveCount())
	}
}
