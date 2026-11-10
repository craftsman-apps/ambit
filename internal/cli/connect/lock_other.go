//go:build !windows

package connect

import (
	"errors"
	"os"
	"syscall"
)

// tryLock takes an exclusive flock without waiting; false means another process holds it.
func tryLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}
