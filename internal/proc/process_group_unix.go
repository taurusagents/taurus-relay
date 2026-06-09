//go:build !windows

package proc

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

func configureCommandProcessGroup(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func hardKillProcess(pid int) error {
	return signalProcessGroup(pid, syscall.SIGKILL)
}

func signalProcessGroup(pid int, sig os.Signal) error {
	unixSig, ok := sig.(syscall.Signal)
	if !ok {
		return fmt.Errorf("unsupported signal type %T", sig)
	}
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pid, unixSig); err != nil {
		if err == syscall.ESRCH {
			if directErr := syscall.Kill(pid, unixSig); directErr == nil || directErr == syscall.ESRCH {
				return nil
			} else {
				return directErr
			}
		}
		return err
	}
	return nil
}

func parseSignal(signal string) (os.Signal, error) {
	switch strings.ToUpper(strings.TrimSpace(signal)) {
	case "SIGINT", "INT":
		return syscall.SIGINT, nil
	case "SIGTERM", "TERM":
		return syscall.SIGTERM, nil
	case "SIGKILL", "KILL":
		return syscall.SIGKILL, nil
	default:
		return nil, fmt.Errorf("unknown signal: %s", signal)
	}
}
