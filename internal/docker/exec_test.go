package docker

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

func waitFor(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if fn() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %v", timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestExecMultiplexerWaitRemovesContainerIndex(t *testing.T) {
	m := NewExecMultiplexer(nil, nil)
	cmd := exec.Command("bash", "-lc", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start process: %v", err)
	}

	sess := &ExecSession{ID: "s1", ContainerID: "c1", cmd: cmd, alive: true}
	m.mu.Lock()
	m.addSessionLocked(sess)
	m.mu.Unlock()

	go m.wait(sess.ID, sess)

	waitFor(t, time.Second, func() bool {
		return m.Count() == 0 && m.countForContainer("c1") == 0
	})
}

func TestExecMultiplexerKillByContainerKillsAndReaps(t *testing.T) {
	m := NewExecMultiplexer(nil, nil)
	cmd := exec.Command("bash", "-lc", "sleep 10")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start process: %v", err)
	}

	sess := &ExecSession{ID: "s2", ContainerID: "c2", cmd: cmd, alive: true}
	m.mu.Lock()
	m.addSessionLocked(sess)
	m.mu.Unlock()

	go m.wait(sess.ID, sess)

	killed, err := m.KillByContainer("c2", 1000)
	if err != nil {
		t.Fatalf("KillByContainer returned error: %v", err)
	}
	if killed != 1 {
		t.Fatalf("expected 1 killed session, got %d", killed)
	}
	if m.countForContainer("c2") != 0 {
		t.Fatalf("expected no indexed sessions for container c2")
	}
}

func TestExecMultiplexerMutationBlocksCreate(t *testing.T) {
	m := NewExecMultiplexer(nil, nil)
	m.BeginContainerMutation("c3")
	defer m.EndContainerMutation("c3")

	err := m.Create("c3", "s3", "bash", nil, "", nil, false, 0, 0)
	if err == nil {
		t.Fatalf("expected create to fail while container mutation is active")
	}
	if !strings.Contains(err.Error(), "lifecycle transition") {
		t.Fatalf("unexpected error: %v", err)
	}
}
