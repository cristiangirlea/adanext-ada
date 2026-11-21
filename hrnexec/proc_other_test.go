//go:build !windows

package hrnexec

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func TestProcessAttributes(t *testing.T) {
	cmd := new(exec.Cmd)
	setProcessAttributes(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatalf("unexpected process attributes: %+v", cmd.SysProcAttr)
	}
}

func isProcessRunning(t *testing.T, pid int) bool {
	t.Helper()
	err := syscall.Kill(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "linux" {
		// A killed orphan can remain a zombie until PID 1 reaps it.
		if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
			return !strings.HasPrefix(string(data)[strings.LastIndexByte(string(data), ')')+1:], " Z ")
		}
	}
	return true
}
