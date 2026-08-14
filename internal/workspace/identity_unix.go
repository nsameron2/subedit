//go:build !windows

package workspace

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

func identityFromFile(_ *os.File, info fs.FileInfo) (FileIdentity, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return FileIdentity{}, errors.New("filesystem did not expose a stable file identity")
	}
	return FileIdentity{kind: "unix", volume: uint64(stat.Dev), object: uint64(stat.Ino)}, nil
}
