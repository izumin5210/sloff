//go:build windows

package dynamodb_test

import (
	"errors"
	"os/exec"
	"syscall"
)

// newSysProcAttr is a no-op on Windows since process groups behave
// differently; killGroup falls through and the caller resorts to
// Process.Kill on the leader.
func newSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}

// killGroup is unsupported on Windows; the kumo_test.go fallback path
// takes over and Kill()s the leader directly.
func killGroup(_ *exec.Cmd) error {
	return errors.New("no process group kill on windows")
}
