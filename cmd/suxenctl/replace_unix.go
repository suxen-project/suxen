//go:build !windows

package main

import "os"

func replaceDownload(temporary, destination string) error {
	return os.Rename(temporary, destination)
}
