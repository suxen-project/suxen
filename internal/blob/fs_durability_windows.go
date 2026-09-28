//go:build windows

package blob

import "golang.org/x/sys/windows"

func replaceFile(oldPath, newPath string) error {
	oldPathUTF16, err := windows.UTF16PtrFromString(oldPath)
	if err != nil {
		return err
	}
	newPathUTF16, err := windows.UTF16PtrFromString(newPath)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(
		oldPathUTF16,
		newPathUTF16,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
}

func syncParentDirectory(string) error {
	// Windows cannot flush a directory through os.File.Sync. replaceFile uses
	// MOVEFILE_WRITE_THROUGH so the directory entry is durable on return.
	return nil
}
