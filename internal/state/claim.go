package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

type Claim struct {
	file *os.File
}

func Acquire(path string) (*Claim, bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("open %s: %w", path, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("claim %s: %w", path, err)
	}
	return &Claim{file: file}, true, nil
}

func (c *Claim) Release() error {
	if c == nil || c.file == nil {
		return nil
	}
	file := c.file
	c.file = nil
	return errors.Join(syscall.Flock(int(file.Fd()), syscall.LOCK_UN), file.Close())
}
