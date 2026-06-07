package docker

import (
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

type ExecOutputCallback func(sessionID string, data []byte)
type ExecExitCallback func(sessionID string, exitCode int)

var (
	startExecWithPTY = pty.StartWithSize
	startExecCmd     = func(cmd *exec.Cmd) error { return cmd.Start() }
)

type ExecSession struct {
	ID          string
	ContainerID string
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	tty         bool
	ptyFile     *os.File
	outputWG    sync.WaitGroup

	mu    sync.RWMutex
	alive bool
}

type containerGate struct {
	mu       sync.Mutex
	cond     *sync.Cond
	accesses int
	mutating bool
}

func newContainerGate() *containerGate {
	gate := &containerGate{}
	gate.cond = sync.NewCond(&gate.mu)
	return gate
}

func (s *ExecSession) IsAlive() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.alive
}

func (s *ExecSession) setAlive(v bool) {
	s.mu.Lock()
	s.alive = v
	s.mu.Unlock()
}

func (s *ExecSession) WriteStdin(data []byte) error {
	if s.stdin == nil {
		return fmt.Errorf("stdin unavailable")
	}
	_, err := s.stdin.Write(data)
	return err
}

func (s *ExecSession) WriteStdinBase64(b64 string) error {
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return fmt.Errorf("decode base64: %w", err)
	}
	return s.WriteStdin(data)
}

func (s *ExecSession) Signal(signal string, shellPID int) error {
	log.Printf("[relay-node/exec] signal session=%s container=%s signal=%s shell_pid=%d", s.ID, s.ContainerID, signal, shellPID)
	if shellPID > 0 {
		sigName := strings.TrimPrefix(strings.ToUpper(signal), "SIG")
		cmd := exec.Command("docker", "exec", s.ContainerID, "kill", "-s", sigName, strconv.Itoa(shellPID))
		if err := cmd.Run(); err == nil {
			return nil
		}
	}

	if s.cmd == nil || s.cmd.Process == nil {
		return fmt.Errorf("session process unavailable")
	}
	return s.cmd.Process.Signal(parseSignal(signal))
}

func (s *ExecSession) Resize(cols, rows uint16) error {
	if !s.tty || s.ptyFile == nil {
		return nil
	}
	if cols == 0 || rows == 0 {
		return nil
	}
	return pty.Setsize(s.ptyFile, &pty.Winsize{Cols: cols, Rows: rows})
}

func (s *ExecSession) Kill() error {
	log.Printf("[relay-node/exec] kill session=%s container=%s", s.ID, s.ContainerID)
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	if s.ptyFile != nil {
		_ = s.ptyFile.Close()
	}
	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	return s.cmd.Process.Kill()
}

func parseSignal(signal string) syscall.Signal {
	switch strings.ToUpper(signal) {
	case "SIGINT", "INT":
		return syscall.SIGINT
	case "SIGTERM", "TERM":
		return syscall.SIGTERM
	case "SIGKILL", "KILL":
		return syscall.SIGKILL
	default:
		return syscall.SIGTERM
	}
}

type ExecMultiplexer struct {
	sessions            map[string]*ExecSession
	sessionsByContainer map[string]map[string]struct{}
	containerGates      map[string]*containerGate
	mu                  sync.RWMutex
	onOutput            ExecOutputCallback
	onExit              ExecExitCallback
	closed              bool
}

func NewExecMultiplexer(onOutput ExecOutputCallback, onExit ExecExitCallback) *ExecMultiplexer {
	return &ExecMultiplexer{
		sessions:            make(map[string]*ExecSession),
		sessionsByContainer: make(map[string]map[string]struct{}),
		containerGates:      make(map[string]*containerGate),
		onOutput:            onOutput,
		onExit:              onExit,
	}
}

func (m *ExecMultiplexer) gateForContainer(containerID string) *containerGate {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gateForContainerLocked(containerID)
}

func (m *ExecMultiplexer) gateForContainerLocked(containerID string) *containerGate {
	gate, ok := m.containerGates[containerID]
	if !ok {
		gate = newContainerGate()
		m.containerGates[containerID] = gate
	}
	return gate
}

func (m *ExecMultiplexer) beginContainerAccess(containerID string) (*containerGate, error) {
	gate := m.gateForContainer(containerID)
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.mutating {
		return nil, fmt.Errorf("container %s is in lifecycle transition; cannot start exec", containerID)
	}
	gate.accesses++
	return gate, nil
}

func (m *ExecMultiplexer) endContainerAccess(gate *containerGate) {
	if gate == nil {
		return
	}
	gate.mu.Lock()
	gate.accesses--
	if gate.accesses == 0 {
		gate.cond.Broadcast()
	}
	gate.mu.Unlock()
}

// WithContainerExec serializes a one-shot container exec against lifecycle
// mutations without registering a long-lived exec session.
func (m *ExecMultiplexer) WithContainerExec(containerID string, action func() error) error {
	gate, err := m.beginContainerAccess(containerID)
	if err != nil {
		return err
	}
	defer m.endContainerAccess(gate)
	return action()
}

func (m *ExecMultiplexer) acquireContainerMutation(containerID string) *containerGate {
	gate := m.gateForContainer(containerID)
	gate.mu.Lock()
	for gate.mutating {
		gate.cond.Wait()
	}
	gate.mutating = true
	for gate.accesses > 0 {
		gate.cond.Wait()
	}
	gate.mu.Unlock()
	return gate
}

func (m *ExecMultiplexer) releaseContainerMutation(gate *containerGate) {
	if gate == nil {
		return
	}
	gate.mu.Lock()
	gate.mutating = false
	gate.cond.Broadcast()
	gate.mu.Unlock()
}

// WithContainerMutation serializes a lifecycle mutation against in-flight
// container exec launches and one-shot exec commands.
func (m *ExecMultiplexer) WithContainerMutation(containerID string, action func() error) error {
	gate := m.acquireContainerMutation(containerID)
	defer m.releaseContainerMutation(gate)
	return action()
}

func (m *ExecMultiplexer) addSessionLocked(sess *ExecSession) {
	m.sessions[sess.ID] = sess
	byContainer, ok := m.sessionsByContainer[sess.ContainerID]
	if !ok {
		byContainer = make(map[string]struct{})
		m.sessionsByContainer[sess.ContainerID] = byContainer
	}
	byContainer[sess.ID] = struct{}{}
}

func (m *ExecMultiplexer) removeSessionLocked(sessionID string) *ExecSession {
	sess, ok := m.sessions[sessionID]
	if !ok {
		return nil
	}
	delete(m.sessions, sessionID)
	if byContainer, exists := m.sessionsByContainer[sess.ContainerID]; exists {
		delete(byContainer, sessionID)
		if len(byContainer) == 0 {
			delete(m.sessionsByContainer, sess.ContainerID)
		}
	}
	return sess
}

func (m *ExecMultiplexer) BeginContainerMutation(containerID string) {
	gate := m.gateForContainer(containerID)
	gate.mu.Lock()
	gate.mutating = true
	gate.mu.Unlock()
}

func (m *ExecMultiplexer) EndContainerMutation(containerID string) {
	gate := m.gateForContainer(containerID)
	gate.mu.Lock()
	gate.mutating = false
	gate.cond.Broadcast()
	gate.mu.Unlock()
}

// Close retires the multiplexer so stale runtime snapshots cannot launch new
// docker exec sessions while reset is still tearing the old runtime down.
func (m *ExecMultiplexer) Close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
}

func (m *ExecMultiplexer) sessionsForContainer(containerID string) []*ExecSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	byContainer, ok := m.sessionsByContainer[containerID]
	if !ok || len(byContainer) == 0 {
		return nil
	}
	sessions := make([]*ExecSession, 0, len(byContainer))
	for sessionID := range byContainer {
		if sess, exists := m.sessions[sessionID]; exists {
			sessions = append(sessions, sess)
		}
	}
	return sessions
}

func (m *ExecMultiplexer) countForContainer(containerID string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessionsByContainer[containerID])
}

func (m *ExecMultiplexer) Create(containerID, sessionID, command string, args []string, cwd string, env map[string]string, tty bool, cols, rows uint16) error {
	gate, err := m.beginContainerAccess(containerID)
	if err != nil {
		return err
	}
	defer m.endContainerAccess(gate)

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("exec multiplexer closed")
	}
	if _, exists := m.sessions[sessionID]; exists {
		m.mu.Unlock()
		return fmt.Errorf("exec session %s already exists", sessionID)
	}

	// Hold the mux lock across docker exec launch and registration so runtime
	// reset cannot close this retired mux in the gap between validation and the
	// actual child-process start.
	if command == "" {
		command = "bash"
	}

	dockerArgs := []string{"exec", "-i"}
	if tty {
		dockerArgs = append(dockerArgs, "-t")
	}
	if cwd != "" {
		dockerArgs = append(dockerArgs, "-w", cwd)
	}
	for k, v := range env {
		dockerArgs = append(dockerArgs, "-e", fmt.Sprintf("%s=%s", k, v))
	}
	dockerArgs = append(dockerArgs, containerID, command)
	dockerArgs = append(dockerArgs, args...)

	log.Printf("[relay-node/exec] start session=%s container=%s command=%s args=%d cwd=%q tty=%t", sessionID, containerID, command, len(args), cwd, tty)
	cmd := exec.Command("docker", dockerArgs...)

	if tty {
		var winsize *pty.Winsize
		if cols > 0 && rows > 0 {
			winsize = &pty.Winsize{Cols: cols, Rows: rows}
		}
		ptmx, err := startExecWithPTY(cmd, winsize)
		if err != nil {
			m.mu.Unlock()
			log.Printf("[relay-node/exec] start failed session=%s container=%s: %v", sessionID, containerID, err)
			return fmt.Errorf("start docker exec with pty: %w", err)
		}

		sess := &ExecSession{
			ID:          sessionID,
			ContainerID: containerID,
			cmd:         cmd,
			stdin:       ptmx,
			tty:         true,
			ptyFile:     ptmx,
			alive:       true,
		}
		sess.outputWG.Add(1)
		m.addSessionLocked(sess)
		m.mu.Unlock()
		go m.streamOutput(sess, ptmx)
		go m.wait(sess)
		return nil
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		m.mu.Unlock()
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.mu.Unlock()
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		m.mu.Unlock()
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := startExecCmd(cmd); err != nil {
		m.mu.Unlock()
		log.Printf("[relay-node/exec] start failed session=%s container=%s: %v", sessionID, containerID, err)
		return fmt.Errorf("start docker exec: %w", err)
	}

	sess := &ExecSession{
		ID:          sessionID,
		ContainerID: containerID,
		cmd:         cmd,
		stdin:       stdin,
		tty:         false,
		alive:       true,
	}
	sess.outputWG.Add(2)
	m.addSessionLocked(sess)
	m.mu.Unlock()
	go m.streamOutput(sess, stdout)
	go m.streamOutput(sess, stderr)
	go m.wait(sess)
	return nil
}

func (m *ExecMultiplexer) streamOutput(sess *ExecSession, r io.Reader) {
	defer sess.outputWG.Done()
	buf := make([]byte, 8192)
	for {
		n, err := r.Read(buf)
		if n > 0 && m.onOutput != nil {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			m.onOutput(sess.ID, chunk)
		}
		if err != nil {
			return
		}
	}
}

func (m *ExecMultiplexer) wait(sess *ExecSession) {
	err := sess.cmd.Wait()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}
	sess.setAlive(false)
	log.Printf("[relay-node/exec] exit session=%s container=%s exit_code=%d err=%v", sess.ID, sess.ContainerID, exitCode, err)
	sess.outputWG.Wait()
	if sess.ptyFile != nil {
		_ = sess.ptyFile.Close()
	}

	if m.onExit != nil {
		m.onExit(sess.ID, exitCode)
	}

	m.mu.Lock()
	m.removeSessionLocked(sess.ID)
	m.mu.Unlock()
}

func (m *ExecMultiplexer) Get(sessionID string) (*ExecSession, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[sessionID]
	if !ok {
		return nil, fmt.Errorf("exec session %s not found", sessionID)
	}
	return s, nil
}

func (m *ExecMultiplexer) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

func (m *ExecMultiplexer) CheckAlive(sessionID string) bool {
	s, err := m.Get(sessionID)
	if err != nil {
		return false
	}
	return s.IsAlive()
}

func (m *ExecMultiplexer) Kill(sessionID string) error {
	s, err := m.Get(sessionID)
	if err != nil {
		return err
	}
	return s.Kill()
}

func (m *ExecMultiplexer) KillByContainer(containerID string, waitTimeoutMs int) (int, error) {
	sessions := m.sessionsForContainer(containerID)
	if len(sessions) == 0 {
		return 0, nil
	}
	log.Printf("[relay-node/exec] kill_by_container container=%s sessions=%d", containerID, len(sessions))
	for _, s := range sessions {
		if err := s.Kill(); err != nil {
			log.Printf("[relay-node/exec] kill_by_container session=%s container=%s kill_err=%v", s.ID, containerID, err)
		}
	}

	if waitTimeoutMs <= 0 {
		return len(sessions), nil
	}

	deadline := time.Now().Add(time.Duration(waitTimeoutMs) * time.Millisecond)
	for {
		remaining := m.countForContainer(containerID)
		if remaining == 0 {
			return len(sessions), nil
		}
		if time.Now().After(deadline) {
			return len(sessions), fmt.Errorf("timed out waiting for %d exec session(s) to exit for container %s", remaining, containerID)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (m *ExecMultiplexer) Resize(sessionID string, cols, rows uint16) error {
	s, err := m.Get(sessionID)
	if err != nil {
		return err
	}
	return s.Resize(cols, rows)
}

func (m *ExecMultiplexer) KillAll() {
	m.mu.Lock()
	sessions := make([]*ExecSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	// Reset retires the entire runtime. Close this mux before clearing maps so
	// stale snapshots cannot start a new docker exec on the old runtime.
	m.closed = true
	m.sessions = make(map[string]*ExecSession)
	m.sessionsByContainer = make(map[string]map[string]struct{})
	m.containerGates = make(map[string]*containerGate)
	m.mu.Unlock()

	for _, s := range sessions {
		_ = s.Kill()
	}
}
