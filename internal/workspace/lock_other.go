//go:build !unix && !windows

package workspace

import (
	"errors"
	"os"
)

type mutationLock struct{ file *os.File }

func tryMutationLock(file *os.File) (*mutationLock, bool, error) {
	return nil, false, errors.New("workspace mutation locking is unsupported on this platform")
}

func (lock *mutationLock) Close() error { return lock.file.Close() }
