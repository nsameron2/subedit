//go:build !windows

package workspace

import "os"

func replaceFile(root *os.Root, tempRel, targetRel, _, _ string) error {
	// os.Root.Rename resolves both parents relative to already-open directory
	// descriptors, so a renamed/replaced absolute workspace path cannot redirect
	// the commit.
	return root.Rename(tempRel, targetRel)
}

func syncRootDirectory(root *os.Root, directoryRel string) error {
	if directoryRel == "" {
		directoryRel = "."
	}
	file, err := root.Open(directoryRel)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
