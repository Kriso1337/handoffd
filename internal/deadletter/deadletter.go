package deadletter

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Kriso1337/handoffd/internal/notifylog"
)

type Record struct {
	At           time.Time              `json:"at"`
	Reason       string                 `json:"reason"`
	Notification notifylog.Notification `json:"notification"`
}

type Queue struct {
	path string
	mu   sync.Mutex
}

func New(path string) *Queue {
	return &Queue{path: path}
}

func (q *Queue) Add(n notifylog.Notification, reason string, at time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	records, err := q.read()
	if err != nil {
		return err
	}
	kept := make([]Record, 0, len(records)+1)
	for _, r := range records {
		if r.Notification.ID != n.ID {
			kept = append(kept, r)
		}
	}
	kept = append(kept, Record{At: at, Reason: reason, Notification: n})
	return q.write(kept)
}

func (q *Queue) Peek() ([]Record, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.read()
}

func (q *Queue) Remove(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	records, err := q.read()
	if err != nil {
		return err
	}
	kept := make([]Record, 0, len(records))
	for _, r := range records {
		if r.Notification.ID != id {
			kept = append(kept, r)
		}
	}
	if len(kept) == len(records) {
		return nil
	}
	return q.write(kept)
}

func (q *Queue) read() ([]Record, error) {
	f, err := os.Open(q.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open dead letters: %w", err)
	}
	defer f.Close()
	var out []Record
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var r Record
		if err := json.Unmarshal(scanner.Bytes(), &r); err == nil && r.Notification.ID != "" {
			out = append(out, r)
		}
	}
	if err := scanner.Err(); err != nil {
		return out, fmt.Errorf("read dead letters: %w", err)
	}
	return out, nil
}

func (q *Queue) write(records []Record) error {
	if err := os.MkdirAll(filepath.Dir(q.path), 0o700); err != nil {
		return fmt.Errorf("create dead letter dir: %w", err)
	}
	var buf []byte
	for _, r := range records {
		raw, err := json.Marshal(r)
		if err != nil {
			return fmt.Errorf("encode dead letter: %w", err)
		}
		buf = append(append(buf, raw...), '\n')
	}
	tmp := q.path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return fmt.Errorf("write dead letters: %w", err)
	}
	if err := os.Rename(tmp, q.path); err != nil {
		return fmt.Errorf("replace dead letters: %w", err)
	}
	return nil
}
