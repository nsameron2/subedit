//go:build windows

package workspace

import (
	"os"

	"golang.org/x/sys/windows"
)

func replaceFile(_ *os.Root, _, _ string, tempPath, targetPath string) error {
	from, err := windows.UTF16PtrFromString(tempPath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(targetPath)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

// Windows' write-through replacement flushes the operation; opening and
// syncing directories is not supported by the standard library there.
func syncRootDirectory(*os.Root, string) error { return nil }
