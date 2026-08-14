//go:build !unix && !windows

package workspace

import (
	"io/fs"
	"os"
	"time"
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
	return os.Chtimes(file.Name(), time.Now(), modified)
}
