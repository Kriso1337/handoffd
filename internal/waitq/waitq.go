package waitq

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const undeliveredDir = "undelivered"

type Prompt struct {
	ID        string    `json:"id"`
	At        time.Time `json:"at"`
	WindowID  string    `json:"window_id"`
	SessionID string    `json:"session_id,omitempty"`
	ThreadTS  string    `json:"thread_ts,omitempty"`
	Text      string    `json:"text"`
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

func (d Dir) Append(p Prompt) error {
	if p.ID == "" || p.WindowID == "" {
		return errors.New("prompt has no id or window")
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d.path, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", d.path, err)
	}
	path := d.promptPath(p.ID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write waiting prompt: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write waiting prompt: %w", err)
	}
	return nil
}

func (d Dir) Peek(windowID string) ([]Prompt, error) {
	prompts, err := d.all()
	if err != nil {
		return nil, err
	}
	var out []Prompt
	for _, p := range prompts {
		if p.WindowID == windowID {
			out = append(out, p)
		}
	}
	return out, nil
}

func (d Dir) Windows() ([]string, error) {
	prompts, err := d.all()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range prompts {
		if !seen[p.WindowID] {
			seen[p.WindowID] = true
			out = append(out, p.WindowID)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (d Dir) Remove(id string) error {
	if err := os.Remove(d.promptPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove waiting prompt %s: %w", id, err)
	}
	return nil
}

func (d Dir) Quarantine(windowID, reason string, now time.Time) (int, error) {
	prompts, err := d.Peek(windowID)
	if err != nil {
		return 0, err
	}
	target := filepath.Join(d.path, undeliveredDir)
	if err := os.MkdirAll(target, 0o700); err != nil {
		return 0, fmt.Errorf("create %s: %w", target, err)
	}
	moved := 0
	for _, p := range prompts {
		raw, err := json.Marshal(struct {
			Prompt
			Reason string    `json:"reason"`
			Left   time.Time `json:"left_at"`
		}{Prompt: p, Reason: reason, Left: now.UTC()})
		if err != nil {
			return moved, err
		}
		if err := os.WriteFile(filepath.Join(target, p.ID+".json"), raw, 0o600); err != nil {
			return moved, fmt.Errorf("keep undelivered prompt: %w", err)
		}
		if err := d.Remove(p.ID); err != nil {
			return moved, err
		}
		moved++
	}
	return moved, nil
}

func (d Dir) all() ([]Prompt, error) {
	entries, err := os.ReadDir(d.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read waiting prompts: %w", err)
	}
	var out []Prompt
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".tmp") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(d.path, name))
		if err != nil {
			return nil, fmt.Errorf("read waiting prompt %s: %w", name, err)
		}
		var p Prompt
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("parse waiting prompt %s: %w", name, err)
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (d Dir) promptPath(id string) string {
	return filepath.Join(d.path, id+".json")
}
