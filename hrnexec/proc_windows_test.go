package hrnexec

import (
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

func TestProcessAttributes(t *testing.T) {
	cmd := new(exec.Cmd)
	setProcessAttributes(cmd)
	if p := cmd.SysProcAttr; p == nil || p.CreationFlags != 0x08000000 || !p.HideWindow {
		t.Fatalf("unexpected Windows process attributes: %+v", p)
	}
}

func TestRunRefusesUnfilterableWindowsVariable(t *testing.T) {
	opts := Options{HRNPath: "must-not-start", WithoutEnv: []string{"SystemRoot"}}
	_, err := Run(t.Context(), opts, Task{Mode: "readonly"}, nil)
	if err == nil || !strings.Contains(err.Error(), "SYSTEMROOT excluded") {
		t.Fatalf("Run = %v", err)
	}
}

func isProcessRunning(t *testing.T, pid int) bool {
	t.Helper()
	handle, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(pid))
	if errors.Is(err, syscall.Errno(87)) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(handle)
	state, err := syscall.WaitForSingleObject(handle, 0)
	if err != nil {
		t.Fatal(err)
	}
	return state == syscall.WAIT_TIMEOUT
}
