//go:build windows

package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const windowsProcSignalsUnsupportedMessage = "proc signals are unsupported on Windows because Taurus node mode is Linux-only; Windows relay support is connect-only"

type namedSignal string

func (s namedSignal) String() string { return string(s) }
func (namedSignal) Signal()          {}

func configureCommandProcessGroup(cmd *exec.Cmd) {
	// Windows builds still compile the proc package for release artifacts, but
	// Taurus node mode is explicitly unsupported there, so we intentionally avoid
	// Unix-only process-group setup instead of inventing incomplete semantics.
	_ = cmd
}

func hardKillProcess(pid int) error {
	if pid <= 0 {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

func signalProcessGroup(pid int, sig os.Signal) error {
	if pid <= 0 {
		return nil
	}
	name := strings.ToUpper(strings.TrimSpace(sig.String()))
	switch name {
	case "SIGKILL", "KILL":
		return hardKillProcess(pid)
	case "SIGINT", "INT", "SIGTERM", "TERM":
		return fmt.Errorf(windowsProcSignalsUnsupportedMessage)
	default:
		return fmt.Errorf("unknown signal: %s", sig.String())
	}
}

func parseSignal(signal string) (os.Signal, error) {
	switch strings.ToUpper(strings.TrimSpace(signal)) {
	case "SIGINT", "INT":
		return namedSignal("SIGINT"), nil
	case "SIGTERM", "TERM":
		return namedSignal("SIGTERM"), nil
	case "SIGKILL", "KILL":
		return namedSignal("SIGKILL"), nil
	default:
		return nil, fmt.Errorf("unknown signal: %s", signal)
	}
}
