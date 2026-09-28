//go:build windows

package content

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func lockStagingFile(file *os.File, nonblocking bool) error {
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK)
	if nonblocking {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	var overlapped windows.Overlapped
	// Lock beyond any supported upload body so independent readers can still
	// open and read staged bytes while ownership is held.
	overlapped.Offset = 0xffffffff
	overlapped.OffsetHigh = 0x7fffffff
	return windows.LockFileEx(windows.Handle(file.Fd()), flags, 0, 1, 0, &overlapped)
}

func tryLockStagingFile(file *os.File) (bool, error) {
	err := lockStagingFile(file, true)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}
