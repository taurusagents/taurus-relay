package shell

import (
	"strings"
	"testing"
)

func TestMultiplexerCreateSessionLimitError(t *testing.T) {
	m := NewMultiplexer(nil, nil)
	m.MaxSessions = 1
	// Register a placeholder session directly so the cap check trips without
	// having to launch a real PTY shell.
	m.sessions["existing"] = &Session{}

	_, err := m.Create("new", "", nil, "", nil)
	if err == nil {
		t.Fatalf("expected session limit error")
	}
	// The rejection travels back to remote callers, so it must clearly identify
	// itself as the relay-configured cap and name the knob that changes it.
	if !strings.Contains(err.Error(), "shell session limit reached (1)") || !strings.Contains(err.Error(), "--max-sessions") {
		t.Fatalf("expected actionable relay-cap error, got %q", err.Error())
	}
}

func TestMultiplexerUnlimitedWhenMaxSessionsZero(t *testing.T) {
	m := NewMultiplexer(nil, nil)
	m.MaxSessions = 0
	m.sessions["existing"] = &Session{}

	// With an unlimited cap the limit check must never fire; any error here
	// would come from actually launching a shell, so probe only the cap branch
	// by ensuring the duplicate-ID check (which runs before launch) is reached.
	_, err := m.Create("existing", "", nil, "", nil)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected duplicate-session error (cap must not fire), got %v", err)
	}
}
