//go:build !windows

package content

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func lockStagingFile(file *os.File, nonblocking bool) error {
	flags := unix.LOCK_EX
	if nonblocking {
		flags |= unix.LOCK_NB
	}
	return unix.Flock(int(file.Fd()), flags)
}

func tryLockStagingFile(file *os.File) (bool, error) {
	err := lockStagingFile(file, true)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}
