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

	"github.com/creack/pty"
)

type ExecOutputCallback func(sessionID string, data []byte)
type ExecExitCallback func(sessionID string, exitCode int)

type ExecSession struct {
	ID          string
	ContainerID string
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	tty         bool
	ptyFile     *os.File

	mu    sync.RWMutex
	alive bool
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
	sessions map[string]*ExecSession
	mu       sync.RWMutex
	onOutput ExecOutputCallback
	onExit   ExecExitCallback
}

func NewExecMultiplexer(onOutput ExecOutputCallback, onExit ExecExitCallback) *ExecMultiplexer {
	return &ExecMultiplexer{
		sessions: make(map[string]*ExecSession),
		onOutput: onOutput,
		onExit:   onExit,
	}
}

func (m *ExecMultiplexer) Create(containerID, sessionID, command string, args []string, cwd string, env map[string]string, tty bool, cols, rows uint16) error {
	m.mu.Lock()
	if _, exists := m.sessions[sessionID]; exists {
		m.mu.Unlock()
		return fmt.Errorf("exec session %s already exists", sessionID)
	}
	m.mu.Unlock()

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
		ptmx, err := pty.StartWithSize(cmd, winsize)
		if err != nil {
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

		m.mu.Lock()
		m.sessions[sessionID] = sess
		m.mu.Unlock()

		go m.streamOutput(sessionID, ptmx)
		go m.wait(sessionID, sess)
		return nil
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
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

	m.mu.Lock()
	m.sessions[sessionID] = sess
	m.mu.Unlock()

	go m.streamOutput(sessionID, stdout)
	go m.streamOutput(sessionID, stderr)
	go m.wait(sessionID, sess)
	return nil
}

func (m *ExecMultiplexer) streamOutput(sessionID string, r io.Reader) {
	buf := make([]byte, 8192)
	for {
		n, err := r.Read(buf)
		if n > 0 && m.onOutput != nil {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			m.onOutput(sessionID, chunk)
		}
		if err != nil {
			return
		}
	}
}

func (m *ExecMultiplexer) wait(sessionID string, sess *ExecSession) {
	err := sess.cmd.Wait()
	if sess.ptyFile != nil {
		_ = sess.ptyFile.Close()
	}
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}
	sess.setAlive(false)
	log.Printf("[relay-node/exec] exit session=%s container=%s exit_code=%d err=%v", sessionID, sess.ContainerID, exitCode, err)

	m.mu.Lock()
	delete(m.sessions, sessionID)
	m.mu.Unlock()

	if m.onExit != nil {
		m.onExit(sessionID, exitCode)
	}
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
	m.sessions = make(map[string]*ExecSession)
	m.mu.Unlock()

	for _, s := range sessions {
		_ = s.Kill()
	}
}
