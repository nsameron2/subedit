//go:build windows

package workspace

import (
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

func hasMultipleLinks(file *os.File, _ fs.FileInfo) (bool, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return false, err
	}
	return info.NumberOfLinks > 1, nil
}
