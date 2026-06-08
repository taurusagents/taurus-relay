package proc

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

const (
	DefaultMaxSessions = 50
	PriorityNormal     = "normal"
	PriorityPriority   = "priority"
	defaultExecPath    = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

var dockerConnectivityEnvKeys = []string{
	"DOCKER_HOST",
	"DOCKER_CONTEXT",
	"DOCKER_CONFIG",
	"DOCKER_TLS_VERIFY",
	"DOCKER_CERT_PATH",
	"XDG_RUNTIME_DIR",
}

var (
	defaultRunTimeoutMs = 120_000
	maxRunCaptureBytes  = 1 << 20
	startWithPTY        = pty.StartWithSize
	startCmd            = func(cmd *exec.Cmd) error { return cmd.Start() }
)

// NormalizePriority collapses unknown values back to the default normal lane.
func NormalizePriority(priority string) string {
	if priority == PriorityPriority {
		return PriorityPriority
	}
	return PriorityNormal
}

// OutputCallback receives streamed process output, separated by stream where possible.
type OutputCallback func(sessionID, stream string, data []byte, priority string)

// ExitCallback receives process exit notifications for spawned sessions.
type ExitCallback func(sessionID string, exitCode int, priority string)

// RunResult captures the complete result of a one-shot proc.run call.
type RunResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Duration time.Duration
	TimedOut bool
}

type limitedBuffer struct {
	limit int
	buf   bytes.Buffer
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	originalLen := len(p)
	if b.limit <= 0 {
		return originalLen, nil
	}
	remaining := b.limit - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.buf.Write(p)
	}
	return originalLen, nil
}

func (b *limitedBuffer) String() string {
	return b.buf.String()
}

// Session represents a long-lived spawned process.
type Session struct {
	ID       string
	Priority string
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	tty      bool
	ptyFile  *os.File
	outputWG sync.WaitGroup
	stdinMu  sync.Mutex
	stdinEOF bool

	mu    sync.RWMutex
	alive bool
}

func (s *Session) IsAlive() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.alive
}

func (s *Session) setAlive(v bool) {
	s.mu.Lock()
	s.alive = v
	s.mu.Unlock()
}

func (s *Session) WriteStdin(data []byte) error {
	s.stdinMu.Lock()
	defer s.stdinMu.Unlock()
	if s.stdin == nil {
		return fmt.Errorf("stdin unavailable")
	}
	if s.stdinEOF {
		return io.ErrClosedPipe
	}
	_, err := s.stdin.Write(data)
	return err
}

func (s *Session) WriteStdinBase64(b64 string) error {
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return fmt.Errorf("decode base64: %w", err)
	}
	return s.WriteStdin(data)
}

// CloseStdin delivers EOF to a non-PTY proc session without forcing the whole
// process tree to exit. PTY sessions do not have a separate stdin stream, so
// callers must use terminal semantics there instead of proc stdin-close.
func (s *Session) CloseStdin() error {
	s.stdinMu.Lock()
	defer s.stdinMu.Unlock()
	if s.stdin == nil {
		return fmt.Errorf("stdin unavailable")
	}
	if s.tty {
		return fmt.Errorf("stdin EOF unsupported for PTY sessions")
	}
	if s.stdinEOF {
		return nil
	}
	s.stdinEOF = true
	return s.stdin.Close()
}

func (s *Session) Signal(signal string) error {
	if s.cmd == nil || s.cmd.Process == nil {
		return fmt.Errorf("session process unavailable")
	}
	syssig, err := parseSignal(signal)
	if err != nil {
		return err
	}
	// proc.signal preserves ChildProcess-style graceful control semantics by
	// forwarding the requested signal to the session process group.
	return signalProcessGroup(s.cmd.Process.Pid, syssig)
}

func (s *Session) Resize(cols, rows uint16) error {
	if !s.tty || s.ptyFile == nil {
		return nil
	}
	if cols == 0 || rows == 0 {
		return nil
	}
	return pty.Setsize(s.ptyFile, &pty.Winsize{Cols: cols, Rows: rows})
}

func (s *Session) Kill() error {
	// proc.kill is the dedicated hard-stop path. It is intentionally separate
	// from proc.signal so Taurus can choose graceful signals vs unconditional kill.
	s.stdinMu.Lock()
	if s.stdin != nil {
		s.stdinEOF = true
		_ = s.stdin.Close()
	}
	s.stdinMu.Unlock()
	if s.ptyFile != nil {
		_ = s.ptyFile.Close()
	}
	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	return signalProcessGroup(s.cmd.Process.Pid, syscall.SIGKILL)
}

// Multiplexer tracks spawned proc sessions and dispatches their stream events.
type Multiplexer struct {
	sessions    map[string]*Session
	mu          sync.RWMutex
	onOutput    OutputCallback
	onExit      ExitCallback
	MaxSessions int
	closed      bool
}

func NewMultiplexer(onOutput OutputCallback, onExit ExitCallback) *Multiplexer {
	return &Multiplexer{
		sessions:    make(map[string]*Session),
		onOutput:    onOutput,
		onExit:      onExit,
		MaxSessions: DefaultMaxSessions,
	}
}

// Run executes a single process to completion with optional stdin, caller
// cancellation, and an optional timeout.
func Run(ctx context.Context, argv []string, cwd string, env map[string]string, stdin []byte, timeoutMs int) (*RunResult, error) {
	cmd, err := buildCommand(argv, cwd, env)
	if err != nil {
		return nil, err
	}
	configureCommandProcessGroup(cmd)

	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}

	var stdout limitedBuffer
	stdout.limit = maxRunCaptureBytes
	var stderr limitedBuffer
	stderr.limit = maxRunCaptureBytes
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if ctx == nil {
		ctx = context.Background()
	}
	if timeoutMs <= 0 {
		timeoutMs = defaultRunTimeoutMs
	}
	cancel := func() {}
	ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := runWithContext(ctx, cmd); err != nil {
		return classifyRunResult(err, ctx, &stdout, &stderr, start), classifyRunError(err, ctx)
	}
	return &RunResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: 0,
		Duration: time.Since(start),
	}, nil
}

// Spawn starts a long-lived process session whose output is streamed asynchronously.
func (m *Multiplexer) Spawn(sessionID string, argv []string, cwd string, env map[string]string, tty bool, cols, rows uint16, priority string) error {
	m.mu.Lock()

	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("proc multiplexer closed")
	}
	if _, exists := m.sessions[sessionID]; exists {
		m.mu.Unlock()
		return fmt.Errorf("proc session %s already exists", sessionID)
	}
	if m.MaxSessions > 0 && len(m.sessions) >= m.MaxSessions {
		m.mu.Unlock()
		return fmt.Errorf("proc session limit reached (%d)", m.MaxSessions)
	}

	// Keep the mux lock held across the OS launch and map insertion so Close or
	// KillAll cannot retire this runtime between the preflight checks and the
	// actual child-process start.
	cmd, err := buildCommand(argv, cwd, env)
	if err != nil {
		m.mu.Unlock()
		return err
	}

	priority = NormalizePriority(priority)

	if tty {
		var winsize *pty.Winsize
		if cols > 0 && rows > 0 {
			winsize = &pty.Winsize{Cols: cols, Rows: rows}
		}
		ptmx, err := startWithPTY(cmd, winsize)
		if err != nil {
			m.mu.Unlock()
			return fmt.Errorf("start pty proc: %w", err)
		}

		sess := &Session{ID: sessionID, Priority: priority, cmd: cmd, stdin: ptmx, tty: true, ptyFile: ptmx, alive: true}
		sess.outputWG.Add(1)
		m.sessions[sessionID] = sess
		m.mu.Unlock()
		go m.streamOutput(sess, "stdout", ptmx)
		go m.wait(sess)
		return nil
	}

	configureCommandProcessGroup(cmd)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		m.mu.Unlock()
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		m.mu.Unlock()
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		m.mu.Unlock()
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := startCmd(cmd); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("start proc: %w", err)
	}

	sess := &Session{ID: sessionID, Priority: priority, cmd: cmd, stdin: stdinPipe, alive: true}
	sess.outputWG.Add(2)
	m.sessions[sessionID] = sess
	m.mu.Unlock()
	go m.streamOutput(sess, "stdout", stdoutPipe)
	go m.streamOutput(sess, "stderr", stderrPipe)
	go m.wait(sess)
	return nil
}

// Close retires the multiplexer so stale runtime snapshots cannot launch new
// proc sessions while reset is still tearing the old runtime down.
func (m *Multiplexer) Close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
}

func (m *Multiplexer) streamOutput(sess *Session, stream string, r io.Reader) {
	defer sess.outputWG.Done()
	buf := make([]byte, 8192)
	for {
		n, err := r.Read(buf)
		if n > 0 && m.onOutput != nil {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			m.onOutput(sess.ID, stream, chunk, sess.Priority)
		}
		if err != nil {
			return
		}
	}
}

func (m *Multiplexer) wait(sess *Session) {
	exitCode, _ := waitForExit(sess.cmd)
	sess.setAlive(false)
	// Wait for every reader goroutine to finish draining output before the exit
	// callback runs. The tunnel uses that callback to emit proc.exit, so keeping
	// the session registered until after this point prevents both exit/output
	// reordering and unsafe session_id reuse while the old queue still exists.
	sess.outputWG.Wait()
	if sess.ptyFile != nil {
		_ = sess.ptyFile.Close()
	}

	if m.onExit != nil {
		m.onExit(sess.ID, exitCode, sess.Priority)
	}

	m.mu.Lock()
	delete(m.sessions, sess.ID)
	m.mu.Unlock()
}

func (m *Multiplexer) Get(sessionID string) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sess, ok := m.sessions[sessionID]
	if !ok {
		return nil, fmt.Errorf("proc session %s not found", sessionID)
	}
	return sess, nil
}

func (m *Multiplexer) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

func (m *Multiplexer) CheckAlive(sessionID string) bool {
	sess, err := m.Get(sessionID)
	if err != nil {
		return false
	}
	return sess.IsAlive()
}

func (m *Multiplexer) Resize(sessionID string, cols, rows uint16) error {
	sess, err := m.Get(sessionID)
	if err != nil {
		return err
	}
	return sess.Resize(cols, rows)
}

func (m *Multiplexer) CloseStdin(sessionID string) error {
	sess, err := m.Get(sessionID)
	if err != nil {
		return err
	}
	return sess.CloseStdin()
}

func (m *Multiplexer) Signal(sessionID, signal string) error {
	sess, err := m.Get(sessionID)
	if err != nil {
		return err
	}
	return sess.Signal(signal)
}

func (m *Multiplexer) Kill(sessionID string) error {
	sess, err := m.Get(sessionID)
	if err != nil {
		return err
	}
	return sess.Kill()
}

func (m *Multiplexer) KillAll() {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, sess := range m.sessions {
		sessions = append(sessions, sess)
	}
	// Once a runtime is reset, stale snapshots may still hold this mux pointer.
	// Mark it closed before dropping sessions so late Spawn calls cannot start
	// orphan processes on the retired runtime.
	m.closed = true
	m.sessions = make(map[string]*Session)
	m.mu.Unlock()

	for _, sess := range sessions {
		_ = sess.Kill()
	}
}

func buildCommand(argv []string, cwd string, env map[string]string) (*exec.Cmd, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("argv is required")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Env = defaultCommandEnv()
	for k, v := range env {
		if k == "" {
			continue
		}
		cmd.Env = upsertEnv(cmd.Env, k, v)
	}
	return cmd, nil
}

func defaultCommandEnv() []string {
	env := []string{
		fmt.Sprintf("PATH=%s", getenvOrDefault("PATH", defaultExecPath)),
		fmt.Sprintf("TMPDIR=%s", os.TempDir()),
		fmt.Sprintf("LANG=%s", getenvOrDefault("LANG", "C.UTF-8")),
	}
	if lcAll := os.Getenv("LC_ALL"); lcAll != "" {
		env = append(env, fmt.Sprintf("LC_ALL=%s", lcAll))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		env = append(env, fmt.Sprintf("HOME=%s", home))
	}
	// Keep proc commands on a minimal baseline env, but explicitly preserve only
	// the narrow host variables the Docker CLI may need to reach the operator's
	// daemon or rootless socket without inheriting the whole service environment.
	for _, key := range dockerConnectivityEnvKeys {
		if value := os.Getenv(key); value != "" {
			env = append(env, fmt.Sprintf("%s=%s", key, value))
		}
	}
	return env
}

func getenvOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func upsertEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func configureCommandProcessGroup(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func runWithContext(ctx context.Context, cmd *exec.Cmd) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := startCmd(cmd); err != nil {
		return err
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	select {
	case err := <-waitCh:
		return err
	case <-ctx.Done():
		if cmd.Process != nil {
			_ = signalProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
		}
		<-waitCh
		return ctx.Err()
	}
}

func classifyRunResult(err error, ctx context.Context, stdout, stderr fmt.Stringer, started time.Time) *RunResult {
	exitCode := 0
	timedOut := ctx.Err() == context.DeadlineExceeded
	if timedOut {
		exitCode = -1
	} else if exitErr, ok := err.(*exec.ExitError); ok {
		exitCode = exitErr.ExitCode()
	} else {
		exitCode = -1
	}
	return &RunResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
		Duration: time.Since(started),
		TimedOut: timedOut,
	}
}

func classifyRunError(err error, ctx context.Context) error {
	if err == nil || ctx.Err() == context.DeadlineExceeded {
		return nil
	}
	if _, ok := err.(*exec.ExitError); ok {
		return nil
	}
	return err
}

func waitForExit(cmd *exec.Cmd) (int, error) {
	err := cmd.Wait()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}
	return exitCode, err
}

func signalProcessGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pid, sig); err != nil {
		if err == syscall.ESRCH {
			if directErr := syscall.Kill(pid, sig); directErr == nil || directErr == syscall.ESRCH {
				return nil
			} else {
				return directErr
			}
		}
		return err
	}
	return nil
}

func parseSignal(signal string) (syscall.Signal, error) {
	switch strings.ToUpper(strings.TrimSpace(signal)) {
	case "SIGINT", "INT":
		return syscall.SIGINT, nil
	case "SIGTERM", "TERM":
		return syscall.SIGTERM, nil
	case "SIGKILL", "KILL":
		return syscall.SIGKILL, nil
	default:
		return 0, fmt.Errorf("unknown signal: %s", signal)
	}
}
