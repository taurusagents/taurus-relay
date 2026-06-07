package docker

import (
	"os"
	"os/exec"
	"path/filepath"
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

func installFakeDocker(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	scriptPath := filepath.Join(binDir, "docker")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
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

	go m.wait(sess)

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

	go m.wait(sess)

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

func TestExecMultiplexerCloseWaitsForInFlightCreateLaunch(t *testing.T) {
	installFakeDocker(t)
	originalStart := startExecCmd
	reachedStart := make(chan struct{})
	releaseStart := make(chan struct{})
	startExecCmd = func(cmd *exec.Cmd) error {
		close(reachedStart)
		<-releaseStart
		return originalStart(cmd)
	}
	t.Cleanup(func() {
		startExecCmd = originalStart
	})

	m := NewExecMultiplexer(nil, nil)
	createDone := make(chan error, 1)
	go func() {
		createDone <- m.Create("c4", "s4", "bash", []string{"-lc", "exit 0"}, "", nil, false, 0, 0)
	}()

	<-reachedStart

	closeDone := make(chan struct{})
	go func() {
		m.Close()
		close(closeDone)
	}()

	select {
	case <-closeDone:
		t.Fatalf("Close returned while Create was still inside its launch critical section")
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseStart)
	if err := <-createDone; err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatalf("Close did not complete after Create left the launch critical section")
	}

	if m.Count() != 1 {
		t.Fatalf("expected launched exec session to be registered before Close completed, got %d session(s)", m.Count())
	}
	m.KillAll()
}
