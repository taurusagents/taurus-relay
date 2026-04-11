// Package shell manages PTY-backed shell sessions.
package shell

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// OutputCallback is called when a shell session produces output.
type OutputCallback func(sessionID string, data []byte)

// ExitCallback is called when a shell session's process exits.
type ExitCallback func(sessionID string, exitCode int)

// Session represents a single PTY-backed shell session.
type Session struct {
	ID      string
	PTY     *os.File
	Cmd     *exec.Cmd
	CWD     string
	Created time.Time

	mu       sync.Mutex
	closed   bool
	onOutput OutputCallback
	onExit   ExitCallback

	// For sentinel-based exec
	execMu     sync.Mutex
	execResult chan execResultInternal
}

type execResultInternal struct {
	output   string
	exitCode int
}

// NewSession creates a new PTY-backed shell session.
func NewSession(id, shell string, args []string, cwd string, env map[string]string, onOutput OutputCallback, onExit ExitCallback) (*Session, error) {
	if shell == "" {
		// Try user's shell, fall back to bash, then sh
		shell = os.Getenv("SHELL")
		if shell == "" {
			if _, err := exec.LookPath("bash"); err == nil {
				shell = "bash"
			} else {
				shell = "sh"
			}
		}
	}

	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			cwd = os.Getenv("HOME")
		}
	}

	var cmd *exec.Cmd
	if len(args) > 0 {
		cmd = exec.Command(shell, args...)
	} else {
		cmd = exec.Command(shell)
	}
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}
	// Disable PS1 to reduce noise in output
	cmd.Env = append(cmd.Env, "PS1=")

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("start pty: %w", err)
	}

	// Set initial size
	_ = pty.Setsize(ptmx, &pty.Winsize{Rows: 24, Cols: 120})

	sess := &Session{
		ID:       id,
		PTY:      ptmx,
		Cmd:      cmd,
		CWD:      cwd,
		Created:  time.Now(),
		onOutput: onOutput,
		onExit:   onExit,
	}

	// Start reading output
	go sess.readLoop()

	// Wait for process exit
	go sess.waitLoop()

	return sess, nil
}

// readLoop continuously reads from the PTY and dispatches output.
func (s *Session) readLoop() {
	buf := make([]byte, 8192)
	for {
		n, err := s.PTY.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])

			// Check if an exec is waiting for sentinel
			s.mu.Lock()
			ch := s.execResult
			s.mu.Unlock()

			if ch != nil {
				// Forward to exec handler
				s.handleExecOutput(data)
			}

			// Always call output callback for streaming
			if s.onOutput != nil {
				s.onOutput(s.ID, data)
			}
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("[session %s] read error: %v", s.ID, err)
			}
			return
		}
	}
}

// waitLoop waits for the shell process to exit.
func (s *Session) waitLoop() {
	err := s.Cmd.Wait()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()

	if s.onExit != nil {
		s.onExit(s.ID, exitCode)
	}
}

// Exec buffer for sentinel-based command execution.
var execBuffers sync.Map // sessionID -> *bytes.Buffer

// Exec runs a command in the session using the sentinel protocol.
// It blocks until the command completes or the timeout expires.
func (s *Session) Exec(command string, timeoutMs int) (string, int, time.Duration, error) {
	s.execMu.Lock()
	defer s.execMu.Unlock()

	if timeoutMs <= 0 {
		timeoutMs = 120000 // 2 minutes default
	}

	sentinelID := fmt.Sprintf("%d", time.Now().UnixNano())
	sentinel := fmt.Sprintf("TAURUS_SENTINEL_%s_EXIT_", sentinelID)

	ch := make(chan execResultInternal, 1)
	buf := &bytes.Buffer{}

	s.mu.Lock()
	s.execResult = ch
	s.mu.Unlock()

	execBuffers.Store(s.ID, &execBuf{
		buf:      buf,
		sentinel: sentinel,
		ch:       ch,
	})

	// Wrap the command with sentinel
	// Use printf to avoid echo interpretation issues
	wrappedCmd := fmt.Sprintf(
		"if eval %s 2>&1; then __rc=0; else __rc=$?; fi; printf '\\n%s%%d\\n' \"$__rc\"\n",
		shellQuote(command),
		sentinel,
	)

	start := time.Now()

	_, err := s.PTY.Write([]byte(wrappedCmd))
	if err != nil {
		s.mu.Lock()
		s.execResult = nil
		s.mu.Unlock()
		execBuffers.Delete(s.ID)
		return "", -1, 0, fmt.Errorf("write to pty: %w", err)
	}

	// Wait for sentinel or timeout
	timeout := time.After(time.Duration(timeoutMs) * time.Millisecond)
	select {
	case result := <-ch:
		elapsed := time.Since(start)
		s.mu.Lock()
		s.execResult = nil
		s.mu.Unlock()
		execBuffers.Delete(s.ID)
		return result.output, result.exitCode, elapsed, nil

	case <-timeout:
		s.mu.Lock()
		s.execResult = nil
		s.mu.Unlock()
		execBuffers.Delete(s.ID)
		elapsed := time.Since(start)
		// Return what we have so far
		return buf.String(), -1, elapsed, fmt.Errorf("command timed out after %dms", timeoutMs)
	}
}

type execBuf struct {
	buf      *bytes.Buffer
	sentinel string
	ch       chan execResultInternal
}

// handleExecOutput processes output during an exec, looking for the sentinel.
func (s *Session) handleExecOutput(data []byte) {
	val, ok := execBuffers.Load(s.ID)
	if !ok {
		return
	}
	eb := val.(*execBuf)
	eb.buf.Write(data)

	// Check if we have the sentinel
	content := eb.buf.String()
	idx := strings.Index(content, eb.sentinel)
	if idx < 0 {
		return
	}

	// Parse exit code from sentinel line
	afterSentinel := content[idx+len(eb.sentinel):]
	exitCode := 0
	fmt.Sscanf(afterSentinel, "%d", &exitCode)

	// Output is everything before the sentinel line
	output := content[:idx]
	// Trim the trailing newline before sentinel
	output = strings.TrimRight(output, "\n")

	// Also strip the echo of the command itself (first line is typically the wrapped command)
	// Find the first newline to skip the command echo
	if nlIdx := strings.Index(output, "\n"); nlIdx >= 0 {
		firstLine := output[:nlIdx]
		if strings.Contains(firstLine, "eval") && strings.Contains(firstLine, "TAURUS_SENTINEL") {
			output = output[nlIdx+1:]
		}
	}

	select {
	case eb.ch <- execResultInternal{output: output, exitCode: exitCode}:
	default:
	}
}

// WriteStdin writes raw data to the session's PTY.
func (s *Session) WriteStdin(data []byte) error {
	_, err := s.PTY.Write(data)
	return err
}

// WriteStdinBase64 decodes base64 data and writes it to the PTY.
func (s *Session) WriteStdinBase64(b64 string) error {
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return fmt.Errorf("decode base64: %w", err)
	}
	return s.WriteStdin(data)
}

// Resize changes the PTY window size.
func (s *Session) Resize(cols, rows uint16) error {
	return pty.Setsize(s.PTY, &pty.Winsize{Rows: rows, Cols: cols})
}

// Signal sends a signal to the shell process.
func (s *Session) Signal(sig string) error {
	var syssig syscall.Signal
	switch strings.ToUpper(sig) {
	case "SIGINT":
		syssig = syscall.SIGINT
	case "SIGTERM":
		syssig = syscall.SIGTERM
	case "SIGKILL":
		syssig = syscall.SIGKILL
	default:
		return fmt.Errorf("unknown signal: %s", sig)
	}
	return s.Cmd.Process.Signal(syssig)
}

// Kill terminates the shell session.
func (s *Session) Kill() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}
	s.closed = true

	// Kill the process
	if s.Cmd.Process != nil {
		_ = s.Cmd.Process.Kill()
	}
	// Close the PTY
	return s.PTY.Close()
}

// IsClosed returns whether the session has been closed.
func (s *Session) IsClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// shellQuote wraps a command for safe eval via base64 encoding.
func shellQuote(cmd string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(cmd))
	return fmt.Sprintf("\"$(printf '%%s' %s | base64 -d)\"", encoded)
}
