//go:build windows

package workspace

import (
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

func identityFromFile(file *os.File, _ fs.FileInfo) (FileIdentity, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return FileIdentity{}, err
	}
	return FileIdentity{
		kind:   "windows",
		volume: uint64(info.VolumeSerialNumber),
		object: uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
	}, nil
}
