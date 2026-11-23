//go:build !windows

package proctree

import (
	"os/exec"
	"syscall"
)

func Configure(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func Start(cmd *exec.Cmd) (kill, close func(), err error) {
	Configure(cmd)
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	return Manage(cmd)
}

func Manage(cmd *exec.Cmd) (kill, close func(), err error) {
	return func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }, func() {}, nil
}
