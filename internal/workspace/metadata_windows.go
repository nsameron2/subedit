//go:build windows

package workspace

import (
	"io/fs"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

func ownership(fs.FileInfo) (int, int) { return -1, -1 }

func setOwnershipAndMode(file *os.File, _, _ int, mode fs.FileMode) string {
	if err := file.Chmod(mode.Perm()); err != nil {
		return "could not preserve mode: " + err.Error()
	}
	return ""
}

func copyExtendedMetadata(_, _ *os.File) []string { return nil }

func setModificationTime(file *os.File, modified time.Time) error {
	writeTime := windows.NsecToFiletime(modified.UnixNano())
	return windows.SetFileTime(windows.Handle(file.Fd()), nil, nil, &writeTime)
}
