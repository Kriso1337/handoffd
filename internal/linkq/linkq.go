package linkq

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Kriso1337/handoffd/internal/mention"
	"github.com/Kriso1337/handoffd/internal/mrref"
)

const (
	StatusLinked   = "linked"
	StatusRejected = "rejected"

	receiptSuffix = ".receipt.json"
	rejectedDir   = "rejected"
)

type Request struct {
	ID       string     `json:"id"`
	At       time.Time  `json:"at"`
	ThreadTS string     `json:"thread_ts"`
	Kind     string     `json:"kind"`
	Refs     mrref.Refs `json:"refs,omitzero"`
	Channel  string     `json:"channel,omitempty"`
	Label    string     `json:"label,omitempty"`
	Subject  string     `json:"subject,omitempty"`
	WindowID string     `json:"window_id"`
	Worktree string     `json:"worktree,omitempty"`
	Force    bool       `json:"force,omitempty"`
}

func (r Request) Validate() error {
	switch {
	case r.ID == "":
		return errors.New("request has no id")
	case r.ThreadTS == "":
		return errors.New("request has no thread_ts")
	case r.WindowID == "":
		return errors.New("request has no window_id")
	}
	if _, ok := ParseKind(r.Kind); !ok {
		return fmt.Errorf("kind %q is not supported (review, help, other)", r.Kind)
	}
	return nil
}

var kinds = map[string]mention.Kind{
	"review": mention.KindReview,
	"help":   mention.KindHelp,
	"other":  mention.KindOther,
}

func ParseKind(name string) (mention.Kind, bool) {
	kind, ok := kinds[name]
	return kind, ok
}

type Receipt struct {
	ID         string    `json:"id"`
	Status     string    `json:"status"`
	WindowID   string    `json:"window_id,omitempty"`
	WindowName string    `json:"window_name,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	At         time.Time `json:"at"`
}

type Malformed struct {
	Path string
	Err  error
}

type Dir struct {
	path string
}

func New(path string) Dir {
	return Dir{path: path}
}

func (d Dir) Path() string {
	return d.path
}

func NewID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func (d Dir) requestPath(id string) string {
	return filepath.Join(d.path, id+".json")
}

func (d Dir) receiptPath(id string) string {
	return filepath.Join(d.path, id+receiptSuffix)
}

func (d Dir) Write(r Request) (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	path := d.requestPath(r.ID)
	if err := writeJSON(path, r); err != nil {
		return "", fmt.Errorf("write link request: %w", err)
	}
	return path, nil
}

func (d Dir) Pending() ([]Request, []Malformed, error) {
	entries, err := os.ReadDir(d.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read link requests: %w", err)
	}
	var requests []Request
	var malformed []Malformed
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasSuffix(name, receiptSuffix) || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".tmp") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if _, err := os.Stat(d.receiptPath(id)); err == nil {
			continue
		}
		path := filepath.Join(d.path, name)
		var r Request
		if err := readJSON(path, &r); err != nil {
			malformed = append(malformed, Malformed{Path: path, Err: err})
			continue
		}
		if r.ID != id {
			malformed = append(malformed, Malformed{Path: path, Err: fmt.Errorf("request id %q does not match file %s", r.ID, name)})
			continue
		}
		if err := r.Validate(); err != nil {
			malformed = append(malformed, Malformed{Path: path, Err: err})
			continue
		}
		requests = append(requests, r)
	}
	sort.Slice(requests, func(i, j int) bool {
		if !requests[i].At.Equal(requests[j].At) {
			return requests[i].At.Before(requests[j].At)
		}
		return requests[i].ID < requests[j].ID
	})
	return requests, malformed, nil
}

func (d Dir) WriteReceipt(rc Receipt) error {
	if rc.ID == "" {
		return errors.New("receipt has no id")
	}
	if err := writeJSON(d.receiptPath(rc.ID), rc); err != nil {
		return fmt.Errorf("write link receipt: %w", err)
	}
	return nil
}

func (d Dir) ReadReceipt(id string) (Receipt, bool, error) {
	var rc Receipt
	err := readJSON(d.receiptPath(id), &rc)
	if errors.Is(err, os.ErrNotExist) {
		return Receipt{}, false, nil
	}
	if err != nil {
		return Receipt{}, false, err
	}
	return rc, true, nil
}

func (d Dir) Quarantine(path, reason string, now time.Time) error {
	target := filepath.Join(d.path, rejectedDir, filepath.Base(path))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(target), err)
	}
	if err := os.Rename(path, target); err != nil {
		return fmt.Errorf("quarantine %s: %w", path, err)
	}
	id := strings.TrimSuffix(filepath.Base(path), ".json")
	return d.WriteReceipt(Receipt{ID: id, Status: StatusRejected, Reason: reason, At: now})
}

func (d Dir) Sweep(now time.Time, ttl time.Duration) (int, error) {
	entries, err := os.ReadDir(d.path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read link requests: %w", err)
	}
	cutoff := now.Add(-ttl)
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(d.path, e.Name())); err != nil {
			return removed, fmt.Errorf("sweep link requests: %w", err)
		}
		removed++
	}
	return removed, nil
}

func Wait(ctx context.Context, d Dir, id string, timeout, interval time.Duration) (Receipt, bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		rc, ok, err := d.ReadReceipt(id)
		if err != nil || ok {
			return rc, ok, err
		}
		if time.Now().After(deadline) {
			return Receipt{}, false, nil
		}
		select {
		case <-ctx.Done():
			return Receipt{}, false, ctx.Err()
		case <-time.After(interval):
		}
	}
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}
