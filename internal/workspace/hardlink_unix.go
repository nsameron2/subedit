//go:build !windows

package workspace

import (
	"io/fs"
	"os"
	"syscall"
)

func hasMultipleLinks(_ *os.File, info fs.FileInfo) (bool, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, nil
	}
	return stat.Nlink > 1, nil
}
