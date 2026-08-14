//go:build unix

package workspace

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func ownership(info fs.FileInfo) (int, int) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1, -1
	}
	return int(stat.Uid), int(stat.Gid)
}

func setOwnershipAndMode(file *os.File, uid, gid int, mode fs.FileMode) string {
	if uid >= 0 && gid >= 0 {
		if err := file.Chown(uid, gid); err != nil {
			// An ordinary user often cannot chown even to the same identity on some
			// filesystems. The content edit remains safe and this is only metadata.
			if chmodErr := file.Chmod(mode.Perm()); chmodErr != nil {
				return fmt.Sprintf("could not preserve ownership (%v) or mode (%v)", err, chmodErr)
			}
			return "could not preserve ownership: " + err.Error()
		}
	}
	if err := file.Chmod(mode.Perm()); err != nil {
		return "could not preserve mode: " + err.Error()
	}
	return ""
}

func copyExtendedMetadata(source, target *os.File) []string {
	size, err := unix.Flistxattr(int(source.Fd()), nil)
	if err != nil || size == 0 {
		return nil
	}
	names := make([]byte, size)
	size, err = unix.Flistxattr(int(source.Fd()), names)
	if err != nil {
		return []string{"could not list extended attributes: " + err.Error()}
	}
	var warnings []string
	start := 0
	for start < size {
		end := start
		for end < size && names[end] != 0 {
			end++
		}
		if end == start {
			start++
			continue
		}
		name := string(names[start:end])
		valueSize, getErr := unix.Fgetxattr(int(source.Fd()), name, nil)
		if getErr != nil {
			warnings = append(warnings, "could not read xattr "+name+": "+getErr.Error())
			start = end + 1
			continue
		}
		value := make([]byte, valueSize)
		_, getErr = unix.Fgetxattr(int(source.Fd()), name, value)
		if getErr != nil {
			warnings = append(warnings, "could not read xattr "+name+": "+getErr.Error())
		} else if setErr := unix.Fsetxattr(int(target.Fd()), name, value, 0); setErr != nil {
			warnings = append(warnings, "could not preserve xattr "+name+": "+setErr.Error())
		}
		start = end + 1
	}
	return warnings
}

func setModificationTime(file *os.File, modified time.Time) error {
	// We do not claim to preserve source atime: reading the source may itself
	// update it, and portable stat APIs do not expose it consistently. Give the
	// staged file a normal current access time while preserving source mtime.
	now := time.Now()
	return unix.Futimes(int(file.Fd()), []unix.Timeval{
		unix.NsecToTimeval(now.UnixNano()),
		unix.NsecToTimeval(modified.UnixNano()),
	})
}
