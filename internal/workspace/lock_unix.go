//go:build unix

package workspace

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

type mutationLock struct{ file *os.File }

func tryMutationLock(file *os.File) (*mutationLock, bool, error) {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &mutationLock{file: file}, true, nil
}

func (lock *mutationLock) Close() error {
	unlockErr := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	return errors.Join(unlockErr, lock.file.Close())
}
