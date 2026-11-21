package memory

import (
	"os"
	"syscall"
	"unsafe"
)

var kernel32 = syscall.NewLazyDLL("kernel32.dll")
var lockFileEx = kernel32.NewProc("LockFileEx")
var unlockFileEx = kernel32.NewProc("UnlockFileEx")

const (
	lockfileFailImmediately = 1
	lockfileExclusiveLock   = 2
	errorLockViolation      = syscall.Errno(33)
)

func lockExclusive(f *os.File) error {
	var offset syscall.Overlapped
	ok, _, err := lockFileEx.Call(f.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0, 1, 0, uintptr(unsafe.Pointer(&offset)))
	if ok != 0 {
		return nil
	}
	if err == errorLockViolation {
		return errDirectoryBusy
	}
	return err
}
func releaseLock(f *os.File) {
	var offset syscall.Overlapped
	unlockFileEx.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&offset)))
	f.Close()
}
