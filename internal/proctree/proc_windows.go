package proctree

import (
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"
)

const (
	createNoWindow                  = 0x08000000
	processSetQuota                 = 0x0100
	jobObjectKillOnClose            = 0x2000
	jobObjectExtendedLimitInfoClass = 9
)

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	createJobObject         = kernel32.NewProc("CreateJobObjectW")
	setInformationJobObject = kernel32.NewProc("SetInformationJobObject")
	assignProcessToJob      = kernel32.NewProc("AssignProcessToJobObject")
	terminateJobObject      = kernel32.NewProc("TerminateJobObject")
)

type jobBasicLimitInformation struct {
	perProcessUserTimeLimit int64
	perJobUserTimeLimit     int64
	limitFlags              uint32
	minimumWorkingSetSize   uintptr
	maximumWorkingSetSize   uintptr
	activeProcessLimit      uint32
	affinity                uintptr
	priorityClass           uint32
	schedulingClass         uint32
}

type jobExtendedLimitInformation struct {
	basicLimitInformation jobBasicLimitInformation
	// Windows aligns the I/O counters to eight bytes on 32-bit targets.
	_                     [8 - unsafe.Sizeof(uintptr(0))]byte
	ioCounters            [6]uint64
	processMemoryLimit    uintptr
	jobMemoryLimit        uintptr
	peakProcessMemoryUsed uintptr
	peakJobMemoryUsed     uintptr
}

func Configure(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow, HideWindow: true}
}

// Start assigns the suspended process before its first instruction can spawn a child.
func Start(cmd *exec.Cmd) (kill, release func(), err error) {
	Configure(cmd)
	const createSuspended = 0x4
	cmd.SysProcAttr.CreationFlags |= createSuspended
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	kill, release, err = Manage(cmd)
	if err == nil {
		const suspendResume = 0x0800
		var handle syscall.Handle
		handle, err = syscall.OpenProcess(suspendResume, false, uint32(cmd.Process.Pid))
		if err == nil {
			resume := syscall.NewLazyDLL("ntdll.dll").NewProc("NtResumeProcess")
			err = resume.Find()
			if err == nil {
				status, _, _ := resume.Call(uintptr(handle))
				if status != 0 {
					err = fmt.Errorf("resume process: NTSTATUS %#x", status)
				}
			}
			syscall.CloseHandle(handle)
		}
	}
	if err != nil {
		cmd.Process.Kill()
		if release != nil {
			release()
		}
		cmd.Wait()
		return nil, nil, err
	}
	return kill, release, nil
}

func Manage(cmd *exec.Cmd) (kill, close func(), err error) {
	job, _, err := createJobObject.Call(0, 0)
	if job == 0 {
		return nil, nil, fmt.Errorf("create process job: %w", err)
	}
	closeJob := func() { _ = syscall.CloseHandle(syscall.Handle(job)) }
	assigned := false
	defer func() {
		if !assigned {
			closeJob()
		}
	}()
	info := jobExtendedLimitInformation{
		basicLimitInformation: jobBasicLimitInformation{limitFlags: jobObjectKillOnClose},
	}
	ok, _, err := setInformationJobObject.Call(job, jobObjectExtendedLimitInfoClass, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	if ok == 0 {
		return nil, nil, fmt.Errorf("configure process job: %w", err)
	}
	process, err := syscall.OpenProcess(processSetQuota|syscall.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return nil, nil, fmt.Errorf("open process for job assignment: %w", err)
	}
	defer syscall.CloseHandle(process)
	// Assign the child before sending its first protocol message.
	ok, _, err = assignProcessToJob.Call(job, uintptr(process))
	if ok == 0 {
		return nil, nil, fmt.Errorf("assign process to job: %w", err)
	}
	assigned = true
	return func() { _, _, _ = terminateJobObject.Call(job, 1) }, closeJob, nil
}
