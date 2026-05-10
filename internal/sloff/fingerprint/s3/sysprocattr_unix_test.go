//go:build !windows

package s3_test

import (
	"os/exec"
	"syscall"
)

// newSysProcAttr puts the spawned kumo process into its own group so a
// stray test exit can clean it up via Setpgid + signal-to-pgid.
func newSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// killGroup signals the kumo process group so any helper processes kumo
// itself spawned also exit. SIGTERM is preferred over SIGKILL so kumo can
// flush any pending state before going down.
func killGroup(cmd *exec.Cmd) error {
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		return err
	}
	return syscall.Kill(-pgid, syscall.SIGTERM)
}
