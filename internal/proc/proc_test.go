package proc

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	var pid int
	waitFor(t, time.Second, func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		parsed, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			t.Fatalf("parse pid %q: %v", string(data), err)
		}
		pid = parsed
		return pid > 0
	})
	return pid
}

func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func TestRunSplitsStdoutAndStderr(t *testing.T) {
	result, err := Run(context.Background(), []string{"bash", "-lc", "printf 'out'; printf 'err' >&2"}, "", nil, nil, 0)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", result.ExitCode)
	}
	if result.Stdout != "out" {
		t.Fatalf("expected stdout %q, got %q", "out", result.Stdout)
	}
	if result.Stderr != "err" {
		t.Fatalf("expected stderr %q, got %q", "err", result.Stderr)
	}
}

func TestRunTimeoutKillsProcess(t *testing.T) {
	result, err := Run(context.Background(), []string{"bash", "-lc", "sleep 10"}, "", nil, nil, 100)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !result.TimedOut {
		t.Fatalf("expected timed_out result")
	}
	if result.ExitCode != -1 {
		t.Fatalf("expected timeout exit code -1, got %d", result.ExitCode)
	}
}

func TestRunUsesDefaultTimeoutWhenOmitted(t *testing.T) {
	originalTimeout := defaultRunTimeoutMs
	defaultRunTimeoutMs = 50
	t.Cleanup(func() {
		defaultRunTimeoutMs = originalTimeout
	})

	result, err := Run(context.Background(), []string{"bash", "-lc", "sleep 10"}, "", nil, nil, 0)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !result.TimedOut {
		t.Fatalf("expected proc.run without timeout_ms to use the default timeout")
	}
	if result.ExitCode != -1 {
		t.Fatalf("expected default-timeout exit code -1, got %d", result.ExitCode)
	}
}

func TestRunUsesMinimalBaselineEnvironment(t *testing.T) {
	t.Setenv("PROC_RUN_SECRET", "super-secret")

	result, err := Run(context.Background(), []string{"bash", "-lc", "env"}, "", nil, nil, 1000)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if strings.Contains(result.Stdout, "PROC_RUN_SECRET=super-secret") {
		t.Fatalf("expected proc.run not to inherit arbitrary host env, got %q", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "PATH=") {
		t.Fatalf("expected proc.run baseline env to include PATH, got %q", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "TMPDIR=") {
		t.Fatalf("expected proc.run baseline env to include TMPDIR, got %q", result.Stdout)
	}
}

func TestRunPreservesNarrowDockerConnectivityEnvironment(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///tmp/test-docker.sock")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")
	t.Setenv("PROC_RUN_SECRET", "super-secret")

	result, err := Run(context.Background(), []string{"bash", "-lc", "env"}, "", nil, nil, 1000)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !strings.Contains(result.Stdout, "DOCKER_HOST=unix:///tmp/test-docker.sock") {
		t.Fatalf("expected proc.run to preserve DOCKER_HOST for host-side Docker commands, got %q", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "XDG_RUNTIME_DIR=/run/user/1000") {
		t.Fatalf("expected proc.run to preserve XDG_RUNTIME_DIR for rootless Docker setups, got %q", result.Stdout)
	}
	if strings.Contains(result.Stdout, "SSH_AUTH_SOCK=/tmp/agent.sock") {
		t.Fatalf("expected proc.run not to expose SSH agent sockets globally, got %q", result.Stdout)
	}
	if strings.Contains(result.Stdout, "PROC_RUN_SECRET=super-secret") {
		t.Fatalf("expected proc.run to keep filtering arbitrary host env, got %q", result.Stdout)
	}
}

func TestRunTimeoutKillsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	result, err := Run(
		context.Background(),
		[]string{"bash", "-lc", "sleep 10 & child=$!; printf '%s' \"$child\" > \"$PIDFILE\"; wait"},
		"",
		map[string]string{"PIDFILE": pidFile},
		nil,
		100,
	)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !result.TimedOut {
		t.Fatalf("expected timed_out result")
	}
	pid := waitForPIDFile(t, pidFile)
	waitFor(t, time.Second, func() bool {
		return !pidAlive(pid)
	})
}

func TestRunContextCancelKillsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	var result *RunResult
	var err error
	go func() {
		result, err = Run(
			ctx,
			[]string{"bash", "-lc", "sleep 10 & child=$!; printf '%s' \"$child\" > \"$PIDFILE\"; wait"},
			"",
			map[string]string{"PIDFILE": pidFile},
			nil,
			0,
		)
		close(done)
	}()

	pid := waitForPIDFile(t, pidFile)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("Run did not exit after context cancellation")
	}
	if err == nil {
		t.Fatalf("expected context cancellation error")
	}
	if result == nil {
		t.Fatalf("expected partial run result on cancellation")
	}
	if result.TimedOut {
		t.Fatalf("expected context cancellation, not timeout")
	}
	waitFor(t, time.Second, func() bool {
		return !pidAlive(pid)
	})
}

func TestRunDoesNotStartWhenContextAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	originalStart := startCmd
	started := false
	startCmd = func(cmd *exec.Cmd) error {
		started = true
		return originalStart(cmd)
	}
	t.Cleanup(func() {
		startCmd = originalStart
	})

	result, err := Run(ctx, []string{"bash", "-lc", "exit 0"}, "", nil, nil, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation error, got %v", err)
	}
	if started {
		t.Fatalf("expected canceled proc.run not to call cmd.Start")
	}
	if result == nil {
		t.Fatalf("expected partial result for canceled proc.run")
	}
	if result.TimedOut {
		t.Fatalf("expected context cancellation, not timeout")
	}
}

func TestRunCapsCapturedOutput(t *testing.T) {
	originalLimit := maxRunCaptureBytes
	maxRunCaptureBytes = 64
	t.Cleanup(func() {
		maxRunCaptureBytes = originalLimit
	})

	result, err := Run(
		context.Background(),
		[]string{"bash", "-lc", "head -c 256 </dev/zero | tr '\\000' 'x'; head -c 256 </dev/zero | tr '\\000' 'y' >&2"},
		"",
		nil,
		nil,
		1000,
	)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(result.Stdout) != 64 {
		t.Fatalf("expected stdout to be capped at 64 bytes, got %d", len(result.Stdout))
	}
	if len(result.Stderr) != 64 {
		t.Fatalf("expected stderr to be capped at 64 bytes, got %d", len(result.Stderr))
	}
	if result.Stdout != strings.Repeat("x", 64) {
		t.Fatalf("expected stdout capture to keep the first bytes, got %q", result.Stdout)
	}
	if result.Stderr != strings.Repeat("y", 64) {
		t.Fatalf("expected stderr capture to keep the first bytes, got %q", result.Stderr)
	}
}

func TestMultiplexerSpawnNonPTYSeparatesStreams(t *testing.T) {
	var mu sync.Mutex
	var stdout strings.Builder
	var stderr strings.Builder
	var exitCode int
	mux := NewMultiplexer(
		func(sessionID, stream string, data []byte, priority string) {
			mu.Lock()
			defer mu.Unlock()
			switch stream {
			case "stdout":
				stdout.Write(data)
			case "stderr":
				stderr.Write(data)
			}
		},
		func(sessionID string, code int, priority string) {
			mu.Lock()
			exitCode = code
			mu.Unlock()
		},
	)

	if err := mux.Spawn("s1", []string{"bash", "-lc", "printf 'out'; printf 'err' >&2"}, "", nil, false, 0, 0, PriorityNormal); err != nil {
		t.Fatalf("Spawn returned error: %v", err)
	}

	waitFor(t, time.Second, func() bool {
		return !mux.CheckAlive("s1")
	})

	mu.Lock()
	defer mu.Unlock()
	if stdout.String() != "out" {
		t.Fatalf("expected stdout %q, got %q", "out", stdout.String())
	}
	if stderr.String() != "err" {
		t.Fatalf("expected stderr %q, got %q", "err", stderr.String())
	}
	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
}

func TestMultiplexerExitWaitsForOutputCallbacks(t *testing.T) {
	outputStarted := make(chan struct{})
	releaseOutput := make(chan struct{})
	exitDelivered := make(chan struct{})
	mux := NewMultiplexer(
		func(sessionID, stream string, data []byte, priority string) {
			select {
			case <-outputStarted:
			default:
				close(outputStarted)
			}
			<-releaseOutput
		},
		func(sessionID string, code int, priority string) {
			close(exitDelivered)
		},
	)

	if err := mux.Spawn("ordered-exit", []string{"bash", "-lc", "printf 'final'"}, "", nil, false, 0, 0, PriorityNormal); err != nil {
		t.Fatalf("Spawn returned error: %v", err)
	}

	select {
	case <-outputStarted:
	case <-time.After(time.Second):
		t.Fatalf("expected output callback to start")
	}
	select {
	case <-exitDelivered:
		t.Fatalf("expected exit callback to wait until output callbacks return")
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseOutput)
	select {
	case <-exitDelivered:
	case <-time.After(time.Second):
		t.Fatalf("expected exit callback after releasing output callback")
	}
}

func TestMultiplexerSpawnPTYUsesStdoutStream(t *testing.T) {
	var mu sync.Mutex
	var stdout strings.Builder
	mux := NewMultiplexer(
		func(sessionID, stream string, data []byte, priority string) {
			if stream != "stdout" {
				t.Fatalf("expected PTY stream to be stdout, got %s", stream)
			}
			mu.Lock()
			stdout.Write(data)
			mu.Unlock()
		},
		nil,
	)

	if err := mux.Spawn("pty-1", []string{"bash", "-lc", "printf 'pty-output'"}, "", nil, true, 80, 24, PriorityPriority); err != nil {
		t.Fatalf("Spawn returned error: %v", err)
	}

	waitFor(t, time.Second, func() bool {
		return !mux.CheckAlive("pty-1")
	})

	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(stdout.String(), "pty-output") {
		t.Fatalf("expected PTY output to contain %q, got %q", "pty-output", stdout.String())
	}
}

func TestCheckAliveTracksSessionLifecycle(t *testing.T) {
	mux := NewMultiplexer(nil, nil)
	if err := mux.Spawn("alive-1", []string{"bash", "-lc", "sleep 0.2"}, "", nil, false, 0, 0, PriorityNormal); err != nil {
		t.Fatalf("Spawn returned error: %v", err)
	}
	if !mux.CheckAlive("alive-1") {
		t.Fatalf("expected session to be alive immediately after spawn")
	}
	waitFor(t, time.Second, func() bool {
		return !mux.CheckAlive("alive-1")
	})
}

func TestMultiplexerCloseStdinDeliversEOF(t *testing.T) {
	var mu sync.Mutex
	var stdout strings.Builder
	var stderr strings.Builder
	exitCode := -1
	mux := NewMultiplexer(
		func(sessionID, stream string, data []byte, priority string) {
			mu.Lock()
			defer mu.Unlock()
			switch stream {
			case "stdout":
				stdout.Write(data)
			case "stderr":
				stderr.Write(data)
			}
		},
		func(sessionID string, code int, priority string) {
			mu.Lock()
			exitCode = code
			mu.Unlock()
		},
	)

	if err := mux.Spawn("stdin-eof", []string{"bash", "-lc", "cat; printf eof >&2"}, "", nil, false, 0, 0, PriorityNormal); err != nil {
		t.Fatalf("Spawn returned error: %v", err)
	}
	sess, err := mux.Get("stdin-eof")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if err := sess.WriteStdin([]byte("hello")); err != nil {
		t.Fatalf("WriteStdin returned error: %v", err)
	}
	if err := mux.CloseStdin("stdin-eof"); err != nil {
		t.Fatalf("CloseStdin returned error: %v", err)
	}

	waitFor(t, time.Second, func() bool {
		return mux.Count() == 0
	})

	mu.Lock()
	defer mu.Unlock()
	if stdout.String() != "hello" {
		t.Fatalf("expected stdout %q after EOF, got %q", "hello", stdout.String())
	}
	if stderr.String() != "eof" {
		t.Fatalf("expected stderr %q after EOF, got %q", "eof", stderr.String())
	}
	if exitCode != 0 {
		t.Fatalf("expected exit code 0 after stdin EOF, got %d", exitCode)
	}
	if err := sess.WriteStdin([]byte("again")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("expected closed-pipe error after EOF, got %v", err)
	}
}

func TestMultiplexerCloseStdinRejectsPTYSessions(t *testing.T) {
	mux := NewMultiplexer(nil, nil)
	if err := mux.Spawn("stdin-eof-pty", []string{"bash", "-lc", "sleep 10"}, "", nil, true, 80, 24, PriorityNormal); err != nil {
		t.Fatalf("Spawn returned error: %v", err)
	}
	defer mux.KillAll()
	if err := mux.CloseStdin("stdin-eof-pty"); err == nil || !strings.Contains(err.Error(), "PTY") {
		t.Fatalf("expected PTY stdin EOF to be rejected, got %v", err)
	}
}

func TestMultiplexerSignalTargetsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	mux := NewMultiplexer(nil, nil)
	if err := mux.Spawn(
		"signal-tree",
		[]string{"bash", "-lc", "sleep 10 & child=$!; printf '%s' \"$child\" > \"$PIDFILE\"; wait"},
		"",
		map[string]string{"PIDFILE": pidFile},
		false,
		0,
		0,
		PriorityNormal,
	); err != nil {
		t.Fatalf("Spawn returned error: %v", err)
	}
	pid := waitForPIDFile(t, pidFile)
	if err := mux.Signal("signal-tree", "TERM"); err != nil {
		t.Fatalf("Signal returned error: %v", err)
	}
	waitFor(t, time.Second, func() bool {
		return !mux.CheckAlive("signal-tree")
	})
	waitFor(t, time.Second, func() bool {
		return !pidAlive(pid)
	})
}

func TestMultiplexerSignalRejectsUnknownSignal(t *testing.T) {
	mux := NewMultiplexer(nil, nil)
	if err := mux.Spawn("signal-invalid", []string{"bash", "-lc", "sleep 10"}, "", nil, false, 0, 0, PriorityNormal); err != nil {
		t.Fatalf("Spawn returned error: %v", err)
	}
	defer mux.KillAll()
	if err := mux.Signal("signal-invalid", "definitely-not-a-signal"); err == nil || !strings.Contains(err.Error(), "unknown signal") {
		t.Fatalf("expected unknown-signal error, got %v", err)
	}
	if !mux.CheckAlive("signal-invalid") {
		t.Fatalf("expected invalid proc.signal to leave the session running")
	}
}

func TestMultiplexerSignalVsKillSemantics(t *testing.T) {
	t.Run("signal preserves graceful trap handling", func(t *testing.T) {
		termFile := filepath.Join(t.TempDir(), "term.txt")
		readyFile := filepath.Join(t.TempDir(), "ready.txt")
		mux := NewMultiplexer(nil, nil)
		argv := []string{"python3", "-c", "import os, pathlib, signal, sys, time\npath = pathlib.Path(os.environ['TERMFILE'])\nready = pathlib.Path(os.environ['READYFILE'])\ndef on_term(signum, frame):\n    path.write_text('term')\n    raise SystemExit(0)\nsignal.signal(signal.SIGTERM, on_term)\nready.write_text('ready')\ntime.sleep(10)"}
		if err := mux.Spawn(
			"signal-graceful",
			argv,
			"",
			map[string]string{"TERMFILE": termFile, "READYFILE": readyFile},
			false,
			0,
			0,
			PriorityNormal,
		); err != nil {
			t.Fatalf("Spawn returned error: %v", err)
		}
		waitFor(t, time.Second, func() bool {
			_, err := os.Stat(readyFile)
			return err == nil
		})
		if err := mux.Signal("signal-graceful", "TERM"); err != nil {
			t.Fatalf("Signal returned error: %v", err)
		}
		waitFor(t, time.Second, func() bool {
			return mux.Count() == 0
		})
		if data, err := os.ReadFile(termFile); err != nil {
			t.Fatalf("expected TERM trap file after proc.signal, got err=%v", err)
		} else if strings.TrimSpace(string(data)) != "term" {
			t.Fatalf("unexpected TERM trap file contents after proc.signal: %q", string(data))
		}
	})

	t.Run("kill stays hard and bypasses TERM trap", func(t *testing.T) {
		termFile := filepath.Join(t.TempDir(), "term.txt")
		readyFile := filepath.Join(t.TempDir(), "ready.txt")
		mux := NewMultiplexer(nil, nil)
		argv := []string{"python3", "-c", "import os, pathlib, signal, sys, time\npath = pathlib.Path(os.environ['TERMFILE'])\nready = pathlib.Path(os.environ['READYFILE'])\ndef on_term(signum, frame):\n    path.write_text('term')\n    raise SystemExit(0)\nsignal.signal(signal.SIGTERM, on_term)\nready.write_text('ready')\ntime.sleep(10)"}
		if err := mux.Spawn(
			"kill-hard",
			argv,
			"",
			map[string]string{"TERMFILE": termFile, "READYFILE": readyFile},
			false,
			0,
			0,
			PriorityNormal,
		); err != nil {
			t.Fatalf("Spawn returned error: %v", err)
		}
		waitFor(t, time.Second, func() bool {
			_, err := os.Stat(readyFile)
			return err == nil
		})
		if err := mux.Kill("kill-hard"); err != nil {
			t.Fatalf("Kill returned error: %v", err)
		}
		waitFor(t, time.Second, func() bool {
			return mux.Count() == 0
		})
		if _, err := os.Stat(termFile); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected proc.kill not to run TERM trap, stat err=%v", err)
		}
	})
}

func TestMultiplexerSessionLimit(t *testing.T) {
	mux := NewMultiplexer(nil, nil)
	mux.MaxSessions = 1
	if err := mux.Spawn("limit-1", []string{"bash", "-lc", "sleep 10"}, "", nil, false, 0, 0, PriorityNormal); err != nil {
		t.Fatalf("first Spawn returned error: %v", err)
	}
	defer mux.KillAll()
	if err := mux.Spawn("limit-2", []string{"bash", "-lc", "sleep 10"}, "", nil, false, 0, 0, PriorityNormal); err == nil {
		t.Fatalf("expected session limit error")
	}
}

func TestMultiplexerCloseWaitsForInFlightSpawnLaunch(t *testing.T) {
	originalStart := startCmd
	reachedStart := make(chan struct{})
	releaseStart := make(chan struct{})
	startCmd = func(cmd *exec.Cmd) error {
		close(reachedStart)
		<-releaseStart
		return originalStart(cmd)
	}
	t.Cleanup(func() {
		startCmd = originalStart
	})

	mux := NewMultiplexer(nil, nil)
	spawnDone := make(chan error, 1)
	go func() {
		spawnDone <- mux.Spawn("race-close", []string{"bash", "-lc", "sleep 30"}, "", nil, false, 0, 0, PriorityNormal)
	}()

	<-reachedStart

	closeDone := make(chan struct{})
	go func() {
		mux.Close()
		close(closeDone)
	}()

	select {
	case <-closeDone:
		t.Fatalf("Close returned while Spawn was still inside its launch critical section")
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseStart)
	if err := <-spawnDone; err != nil {
		t.Fatalf("Spawn returned error: %v", err)
	}
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatalf("Close did not complete after Spawn left the launch critical section")
	}

	if mux.Count() != 1 {
		t.Fatalf("expected launched session to be registered before Close completed, got %d session(s)", mux.Count())
	}
	mux.KillAll()
}
