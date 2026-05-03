package session

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

var (
	// ErrInvalidID reports that a session ID is empty, too long, uses forbidden
	// characters, or begins with a disallowed prefix.
	ErrInvalidID = errors.New("invalid session id")
	// ErrAlreadyExists reports that Create was called while the single-session
	// manager already has an active session.
	ErrAlreadyExists = errors.New("session already exists")
	// ErrNotFound reports that no active session matches the requested ID.
	ErrNotFound = errors.New("session not found")
	// ErrCompleting reports that a matching session is being completed and no
	// new work may be started for it.
	ErrCompleting = errors.New("session is completing")
)

var idRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// Clock supplies the current time to Manager.
//
// Implementations must be safe for the way the caller shares them with Manager.
// Manager invokes Now while holding its mutex for state transitions and idle
// checks. Now must return the current logical time and should not block on
// Manager methods to avoid deadlock.
type Clock interface {
	// Now returns the current wall-clock or test-controlled time used for session
	// creation, activity updates, and idle expiry decisions.
	Now() time.Time
}

type realClock struct{}

// Now returns the current system time for production session accounting.
func (realClock) Now() time.Time { return time.Now() }

// Manager owns the lifecycle of the agent's single active session.
//
// All exported Manager methods are safe for concurrent use. Callers do not need
// to hold Manager's internal lock; methods acquire it as needed. Operations
// validate session IDs before mutating state and return package sentinel errors
// for invalid, missing, duplicate, or completing sessions.
type Manager struct {
	mu        sync.Mutex
	root      string
	idle      time.Duration
	clock     Clock
	active    *Session
	closeOnce sync.Once
}

// Session is a snapshot of session state returned to callers.
//
// It is a copy, not the Manager's live internal pointer, so callers may read it
// without locks. Mutating a Session value has no effect on Manager state.
type Session struct {
	// ID is the validated client-supplied session identifier.
	ID string
	// Workspace is the absolute or root-relative host directory for session
	// files. Callers must still use pathutil helpers before touching paths below
	// it.
	Workspace string
	// CreatedAt is the clock time when Manager.Create created the session.
	CreatedAt time.Time
	// LastActivityAt is updated when work is acquired or released and is used by
	// idle expiry.
	LastActivityAt time.Time
	// ActiveRPCs is the number of in-flight leases for the session at snapshot
	// time.
	ActiveRPCs int
	// Completing reports whether explicit completion has begun. New leases are
	// rejected while this is true.
	Completing bool
}

// Lease represents an active RPC's claim on a session.
//
// A Lease is returned by Manager.Acquire and must be released exactly once with
// Done when the RPC has finished using the workspace. Lease methods are safe to
// call with nil receivers, but callers should not copy a Lease.
type Lease struct {
	mgr *Manager
	id  string
}

// NewManager creates a Manager that uses the real system clock.
//
// root is created with private permissions when non-empty; an empty root creates
// a private temporary directory. idle is the idle timeout used by ExpireIdle.
// The returned Manager has no active session. Errors come from workspace-root
// creation.
func NewManager(root string, idle time.Duration) (*Manager, error) {
	return NewManagerWithClock(root, idle, realClock{})
}

// NewManagerWithClock creates a Manager using the supplied Clock.
//
// root and idle have the same meaning as NewManager. clock must be non-nil and
// must not call back into Manager from Now. The returned Manager has no active
// session. Errors come from workspace-root creation.
func NewManagerWithClock(root string, idle time.Duration, clock Clock) (*Manager, error) {
	if root == "" {
		var err error
		root, err = os.MkdirTemp("", "remote-agent-*")
		if err != nil {
			return nil, err
		}
	} else if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &Manager{root: root, idle: idle, clock: clock}, nil
}

// ValidateID verifies that id is safe for use as a session identifier and
// workspace directory name.
//
// id must be non-empty, at most 128 bytes, contain only ASCII letters, digits,
// '.', '_', and '-', not be "." or "..", and not start with '.' or '-'. It
// returns ErrInvalidID on failure and nil on success.
func ValidateID(id string) error {
	if !idRE.MatchString(id) || id == "." || id == ".." || id[0] == '.' || id[0] == '-' {
		return ErrInvalidID
	}
	return nil
}

// Create validates id and creates the single active session.
//
// The caller must not hold any Manager-internal lock; Create handles locking.
// It creates the workspace directory below the manager root, stores the active
// session, and returns a snapshot. It returns ErrInvalidID, ErrAlreadyExists, or
// an OS error from workspace creation.
func (m *Manager) Create(id string) (*Session, error) {
	if err := ValidateID(id); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active != nil {
		return nil, ErrAlreadyExists
	}
	now := m.clock.Now()
	workspace := filepath.Join(m.root, id)
	if err := os.Mkdir(workspace, 0o700); err != nil {
		return nil, err
	}
	s := &Session{ID: id, Workspace: workspace, CreatedAt: now, LastActivityAt: now}
	m.active = s
	return clone(s), nil
}

// Get validates and briefly acquires a session before returning its snapshot.
//
// Get updates activity timestamps like other session-scoped RPCs and rejects
// completing sessions. It returns ErrInvalidID, ErrNotFound, ErrCompleting, or a
// Session snapshot. No lease remains held after Get returns.
func (m *Manager) Get(id string) (*Session, error) {
	lease, err := m.Acquire(id)
	if err != nil {
		return nil, err
	}
	defer lease.Done()
	return m.Snapshot(id)
}

// Snapshot returns a copy of the active session without acquiring an RPC lease.
//
// It validates id and checks the active session under Manager's lock. Snapshot
// does not update LastActivityAt or ActiveRPCs. It returns ErrInvalidID,
// ErrNotFound, or a Session copy.
func (m *Manager) Snapshot(id string) (*Session, error) {
	if err := ValidateID(id); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil || m.active.ID != id {
		return nil, ErrNotFound
	}
	return clone(m.active), nil
}

// Acquire validates id and reserves the active session for one in-flight RPC.
//
// The caller must call Lease.Done when session work finishes. Acquire updates
// LastActivityAt, increments ActiveRPCs, and prevents idle expiry until Done is
// called. It returns ErrInvalidID, ErrNotFound, ErrCompleting, or a Lease.
func (m *Manager) Acquire(id string) (*Lease, error) {
	if err := ValidateID(id); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil || m.active.ID != id {
		return nil, ErrNotFound
	}
	if m.active.Completing {
		return nil, ErrCompleting
	}
	m.active.ActiveRPCs++
	m.active.LastActivityAt = m.clock.Now()
	return &Lease{mgr: m, id: id}, nil
}

// Done releases a session lease acquired by Manager.Acquire.
//
// Done is idempotent for nil or already-released leases. It decrements
// ActiveRPCs when the same session is still active, updates LastActivityAt, and
// has no returned error.
func (l *Lease) Done() {
	if l == nil || l.mgr == nil {
		return
	}
	l.mgr.mu.Lock()
	defer l.mgr.mu.Unlock()
	if l.mgr.active != nil && l.mgr.active.ID == l.id && l.mgr.active.ActiveRPCs > 0 {
		l.mgr.active.ActiveRPCs--
		l.mgr.active.LastActivityAt = l.mgr.clock.Now()
	}
	l.mgr = nil
}

// Session returns a current snapshot for the lease's session.
//
// The lease must still be active. The method uses Manager.Snapshot, so it
// returns ErrNotFound if the lease is nil, already released, or the session has
// disappeared; otherwise it returns a copy safe to read without locks.
func (l *Lease) Session() (*Session, error) {
	if l == nil || l.mgr == nil {
		return nil, ErrNotFound
	}
	return l.mgr.Snapshot(l.id)
}

// Complete marks the matching session as completing, removes its workspace, and
// clears it from the manager.
//
// The caller must not hold Manager's lock. New Acquire calls fail once
// completion begins. Complete returns ErrInvalidID, ErrNotFound, or an error
// from os.RemoveAll. Cleanup is best-effort and idempotent at the filesystem
// level, but completing an already-cleared session returns ErrNotFound.
func (m *Manager) Complete(id string) error {
	if err := ValidateID(id); err != nil {
		return err
	}
	var workspace string
	m.mu.Lock()
	if m.active == nil || m.active.ID != id {
		m.mu.Unlock()
		return ErrNotFound
	}
	m.active.Completing = true
	workspace = m.active.Workspace
	m.active = nil
	m.mu.Unlock()
	return os.RemoveAll(workspace)
}

// ActiveCount returns the number of active sessions managed by m.
//
// In v1 the value is always 0 or 1. The method acquires Manager's lock and has
// no side effects or error conditions.
func (m *Manager) ActiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil {
		return 0
	}
	return 1
}

// ExpireIdle completes the active session if it has exceeded the idle timeout.
//
// A session is eligible only when it is active, not completing, has no active
// RPC leases, and now-lastActivity exceeds the configured idle duration. The
// method returns nil when there is nothing to expire, or the error returned by
// Complete when cleanup fails.
func (m *Manager) ExpireIdle() error {
	m.mu.Lock()
	if m.active == nil || m.active.Completing || m.active.ActiveRPCs > 0 || m.clock.Now().Sub(m.active.LastActivityAt) <= m.idle {
		m.mu.Unlock()
		return nil
	}
	id := m.active.ID
	m.mu.Unlock()
	log.Printf("Janitor cleaning expired session %v.", id)
	return m.Complete(id)
}

// RunJanitor periodically calls ExpireIdle until stop is closed.
//
// interval must be positive or time.NewTicker will panic. The method blocks the
// current goroutine, logs cleanup activity and expiry errors, and returns only
// after receiving from stop.
func (m *Manager) RunJanitor(stop <-chan struct{}, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	log.Printf("Janitor starting with interval %v.", interval)
	for {
		select {
		case <-ticker.C:
			log.Printf("Janitor cleaning expired sessions.")
			if err := m.ExpireIdle(); err != nil {
				log.Printf("Janitor error: %v", err)
			}
		case <-stop:
			return
		}
	}
}

// Close removes any active session workspace and prevents duplicate cleanup.
//
// Close is safe to call concurrently and multiple times. It clears the active
// session under lock and returns the first os.RemoveAll error, if any. It does
// not stop a janitor goroutine; callers must close that stop channel separately.
func (m *Manager) Close() error {
	var err error
	m.closeOnce.Do(func() {
		m.mu.Lock()
		active := m.active
		m.active = nil
		m.mu.Unlock()
		if active != nil {
			err = os.RemoveAll(active.Workspace)
		}
	})
	return err
}

// String returns a compact "id:workspace" representation of the session.
//
// It reads only the value receiver fields and has no side effects.
func (s Session) String() string {
	return fmt.Sprintf("%s:%s", s.ID, s.Workspace)
}

func clone(s *Session) *Session {
	if s == nil {
		return nil
	}
	cp := *s
	return &cp
}
