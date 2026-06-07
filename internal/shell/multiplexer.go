package shell

import (
	"fmt"
	"sync"
)

const DefaultMaxSessions = 50

// Multiplexer manages multiple concurrent shell sessions.
type Multiplexer struct {
	sessions    map[string]*Session
	mu          sync.RWMutex
	onOutput    OutputCallback
	onExit      ExitCallback
	MaxSessions int
	closed      bool
}

// NewMultiplexer creates a new shell multiplexer.
func NewMultiplexer(onOutput OutputCallback, onExit ExitCallback) *Multiplexer {
	return &Multiplexer{
		sessions:    make(map[string]*Session),
		onOutput:    onOutput,
		onExit:      onExit,
		MaxSessions: DefaultMaxSessions,
	}
}

// Create starts a new shell session.
func (m *Multiplexer) Create(id, shell string, args []string, cwd string, env map[string]string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil, fmt.Errorf("shell multiplexer closed")
	}
	if _, exists := m.sessions[id]; exists {
		return nil, fmt.Errorf("session %s already exists", id)
	}

	if m.MaxSessions > 0 && len(m.sessions) >= m.MaxSessions {
		return nil, fmt.Errorf("session limit reached (%d)", m.MaxSessions)
	}

	sess, err := NewSession(id, shell, args, cwd, env, m.onOutput, func(sessionID string, exitCode int) {
		if m.onExit != nil {
			m.onExit(sessionID, exitCode)
		}

		// Keep the session registered until the tunnel has published the final
		// exit state so same-ID reuse cannot slip into the gap between exit and
		// output-queue completion bookkeeping.
		m.mu.Lock()
		delete(m.sessions, sessionID)
		m.mu.Unlock()
	})
	if err != nil {
		return nil, err
	}

	m.sessions[id] = sess
	return sess, nil
}

// Get retrieves a session by ID.
func (m *Multiplexer) Get(id string) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	sess, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session %s not found", id)
	}
	return sess, nil
}

// Kill terminates a session.
func (m *Multiplexer) Kill(id string) error {
	m.mu.Lock()
	sess, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("session %s not found", id)
	}
	delete(m.sessions, id)
	m.mu.Unlock()

	return sess.Kill()
}

// Close retires the multiplexer so stale runtime snapshots cannot launch new
// sessions while reset is still tearing the old runtime down.
func (m *Multiplexer) Close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
}

// KillAll terminates all sessions.
func (m *Multiplexer) KillAll() {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	// Reset retires the runtime generation, so any stale snapshot still holding
	// this mux must fail future Create calls instead of launching a hidden shell.
	m.closed = true
	m.sessions = make(map[string]*Session)
	m.mu.Unlock()

	for _, s := range sessions {
		_ = s.Kill()
	}
}

// Count returns the number of active sessions.
func (m *Multiplexer) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// SessionIDs returns the IDs of all active sessions.
func (m *Multiplexer) SessionIDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	return ids
}
