package outbox

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
)

type Kind string

const (
	KindReply      Kind = "reply"
	KindMRNote     Kind = "mr_note"
	KindMarker     Kind = "marker"
	KindReviewDone Kind = "review_done"

	PhaseTaken       = "taken"
	PhaseReviewStart = "review_start"
	PhaseReviewDone  = "review_done"

	StatusPosted   = "posted"
	StatusRejected = "rejected"
	StatusPending  = "pending"
	StatusUnknown  = "acceptance_unknown"

	receiptSuffix = ".receipt.json"
	rejectedDir   = "rejected"
)

type Record struct {
	ID        string    `json:"id"`
	At        time.Time `json:"at"`
	ThreadTS  string    `json:"thread_ts"`
	Kind      Kind      `json:"kind"`
	Text      string    `json:"text,omitempty"`
	SHA       string    `json:"sha,omitempty"`
	Phase     string    `json:"phase,omitempty"`
	Blockers  int       `json:"blockers,omitempty"`
	Others    int       `json:"others,omitempty"`
	Decision  string    `json:"decision,omitempty"`
	SessionID string    `json:"session_id"`
}

func (r Record) Validate() error {
	switch {
	case r.ID == "":
		return errors.New("record has no id")
	case r.ThreadTS == "":
		return errors.New("record has no thread_ts")
	case r.SessionID == "":
		return errors.New("record has no session_id")
	}
	switch r.Kind {
	case KindReply, KindMRNote:
		if strings.TrimSpace(r.Text) == "" {
			return fmt.Errorf("%s has no text", r.Kind)
		}
	case KindMarker:
		if r.Phase != PhaseTaken && r.Phase != PhaseReviewStart {
			return fmt.Errorf("marker phase %q is not taken or review_start", r.Phase)
		}
	case KindReviewDone:
	default:
		return fmt.Errorf("kind %q is unknown", r.Kind)
	}
	if r.Kind != KindReply && r.SHA == "" {
		return fmt.Errorf("%s needs a sha", r.Kind)
	}
	return nil
}

type Receipt struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	Permalink string    `json:"permalink,omitempty"`
	TS        string    `json:"ts,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	Attempts  int       `json:"attempts,omitempty"`
	At        time.Time `json:"at"`
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

func (d Dir) recordPath(id string) string {
	return filepath.Join(d.path, id+".json")
}

func (d Dir) receiptPath(id string) string {
	return filepath.Join(d.path, id+receiptSuffix)
}

func (d Dir) Write(r Record) (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	path := d.recordPath(r.ID)
	if err := writeJSON(path, r); err != nil {
		return "", fmt.Errorf("write outbox record: %w", err)
	}
	return path, nil
}

func (d Dir) Pending() ([]Record, []Malformed, error) {
	entries, err := os.ReadDir(d.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read outbox: %w", err)
	}
	var records []Record
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
		var r Record
		if err := readJSON(path, &r); err != nil {
			malformed = append(malformed, Malformed{Path: path, Err: err})
			continue
		}
		if r.ID != id {
			malformed = append(malformed, Malformed{Path: path, Err: fmt.Errorf("record id %q does not match file %s", r.ID, name)})
			continue
		}
		if err := r.Validate(); err != nil {
			malformed = append(malformed, Malformed{Path: path, Err: err})
			continue
		}
		records = append(records, r)
	}
	sort.Slice(records, func(i, j int) bool {
		if !records[i].At.Equal(records[j].At) {
			return records[i].At.Before(records[j].At)
		}
		return records[i].ID < records[j].ID
	})
	return records, malformed, nil
}

func (d Dir) WriteReceipt(rc Receipt) error {
	if rc.ID == "" {
		return errors.New("receipt has no id")
	}
	if err := writeJSON(d.receiptPath(rc.ID), rc); err != nil {
		return fmt.Errorf("write outbox receipt: %w", err)
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

func (d Dir) Retry() (int, error) {
	entries, err := os.ReadDir(d.path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read outbox: %w", err)
	}
	retried := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), receiptSuffix) {
			continue
		}
		path := filepath.Join(d.path, e.Name())
		var rc Receipt
		if err := readJSON(path, &rc); err != nil || rc.Status != StatusPending {
			continue
		}
		if err := os.Remove(path); err != nil {
			return retried, fmt.Errorf("drop receipt %s: %w", path, err)
		}
		retried++
	}
	return retried, nil
}

func (d Dir) Sweep(now time.Time, ttl time.Duration) (int, error) {
	entries, err := os.ReadDir(d.path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read outbox: %w", err)
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
			return removed, fmt.Errorf("sweep outbox: %w", err)
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
