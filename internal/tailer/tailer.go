package tailer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

type Tailer struct {
	path     string
	poll     time.Duration
	fromHead bool
}

func New(path string, poll time.Duration) *Tailer {
	return &Tailer{path: path, poll: poll}
}

func NewFromHead(path string, poll time.Duration) *Tailer {
	return &Tailer{path: path, poll: poll, fromHead: true}
}

func (t *Tailer) Run(ctx context.Context, onLine func(string)) error {
	file, info, offset, err := t.open(t.fromHead)
	if err != nil {
		return err
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	var partial strings.Builder

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		chunk, err := reader.ReadString('\n')
		offset += int64(len(chunk))

		if err == nil {
			partial.WriteString(chunk)
			onLine(strings.TrimSuffix(partial.String(), "\n"))
			partial.Reset()
			continue
		}
		if !errors.Is(err, io.EOF) {
			return fmt.Errorf("read %s: %w", t.path, err)
		}
		partial.WriteString(chunk)

		replaced, err := t.rotated(info, offset)
		if err != nil {
			return err
		}
		if replaced {
			file.Close()
			file, info, offset, err = t.open(true)
			if err != nil {
				return err
			}
			reader = bufio.NewReader(file)
			partial.Reset()
			continue
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(t.poll):
		}
	}
}

func (t *Tailer) open(fromHead bool) (*os.File, os.FileInfo, int64, error) {
	file, err := os.Open(t.path)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("open %s: %w", t.path, err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, 0, fmt.Errorf("stat %s: %w", t.path, err)
	}
	if fromHead {
		return file, info, 0, nil
	}
	offset, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		file.Close()
		return nil, nil, 0, fmt.Errorf("seek %s: %w", t.path, err)
	}
	return file, info, offset, nil
}

func (t *Tailer) rotated(info os.FileInfo, offset int64) (bool, error) {
	current, err := os.Stat(t.path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", t.path, err)
	}
	if !os.SameFile(current, info) {
		return true, nil
	}
	return current.Size() < offset, nil
}
