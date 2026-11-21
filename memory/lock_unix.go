//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

package memory

import (
	"errors"
	"os"
	"syscall"
)

func lockExclusive(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errDirectoryBusy
	}
	return err
}
func releaseLock(f *os.File) { f.Close() }
