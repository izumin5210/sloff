//go:build !windows

package dynamodb_test

import (
	"errors"
	"os/exec"
	"syscall"
)

// newSysProcAttr puts the spawned kumo process into its own group so a
// stray test exit can clean it up via Setpgid + signal-to-pgid.
func newSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// killGroup signals the kumo process group so any helper processes it
// spawned also exit.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return errors.New("no process to kill")
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		return err
	}
	return syscall.Kill(-pgid, syscall.SIGTERM)
}
