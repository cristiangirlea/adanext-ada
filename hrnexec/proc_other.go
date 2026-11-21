//go:build !windows

package hrnexec

import (
	"os/exec"
	"syscall"
)

func setProcessAttributes(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func manageProcessTree(cmd *exec.Cmd) (kill, close func(), err error) {
	return func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }, func() {}, nil
}
