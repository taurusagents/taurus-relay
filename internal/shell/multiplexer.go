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

	if _, exists := m.sessions[id]; exists {
		return nil, fmt.Errorf("session %s already exists", id)
	}

	if m.MaxSessions > 0 && len(m.sessions) >= m.MaxSessions {
		return nil, fmt.Errorf("session limit reached (%d)", m.MaxSessions)
	}

	sess, err := NewSession(id, shell, args, cwd, env, m.onOutput, func(sessionID string, exitCode int) {
		// Clean up from map when process exits
		m.mu.Lock()
		delete(m.sessions, sessionID)
		m.mu.Unlock()

		if m.onExit != nil {
			m.onExit(sessionID, exitCode)
		}
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

// KillAll terminates all sessions.
func (m *Multiplexer) KillAll() {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
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
