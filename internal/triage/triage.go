package triage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Verdict struct {
	React  bool   `json:"react"`
	Reason string `json:"reason"`
	Task   string `json:"task"`
}

type Outcome int

const (
	OutcomeDropped Outcome = iota
	OutcomeEscalated
	OutcomeTimedOut
)

func (o Outcome) String() string {
	switch o {
	case OutcomeEscalated:
		return "escalated"
	case OutcomeTimedOut:
		return "timed out"
	default:
		return "dropped"
	}
}

type Windows interface {
	NewWindow(ctx context.Context, name, cwd, promptFile string) (string, error)
}

type Killer interface {
	KillWindow(ctx context.Context, id string) error
}

type Waiter struct {
	dir      string
	timeout  time.Duration
	interval time.Duration
}

func NewWaiter(dir string, timeout, interval time.Duration) Waiter {
	return Waiter{dir: dir, timeout: timeout, interval: interval}
}

func (w Waiter) Path(ts string) string {
	return filepath.Join(w.dir, "verdict-"+strings.ReplaceAll(ts, ".", "")+".json")
}

func (w Waiter) Peek(path string) (Verdict, bool) {
	verdict, err := read(path)
	if err != nil {
		return Verdict{}, false
	}
	os.Remove(path)
	return verdict, true
}

func (w Waiter) Wait(ctx context.Context, path string) (Verdict, error) {
	deadline := time.Now().Add(w.timeout)
	for {
		verdict, err := read(path)
		if err == nil {
			os.Remove(path)
			return verdict, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return Verdict{}, err
		}
		if time.Now().After(deadline) {
			return Verdict{}, fmt.Errorf("verdict %s: %w", path, context.DeadlineExceeded)
		}
		select {
		case <-ctx.Done():
			return Verdict{}, ctx.Err()
		case <-time.After(w.interval):
		}
	}
}

func Parse(text string) (Verdict, error) {
	return decode(strings.TrimSpace(text))
}

func FromPane(pane string) (Verdict, error) {
	start := strings.LastIndex(pane, "{")
	end := strings.LastIndex(pane, "}")
	if start < 0 || end < start {
		return Verdict{}, errors.New("no JSON object in pane output")
	}
	return decode(collapse(pane[start : end+1]))
}

func read(path string) (Verdict, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Verdict{}, err
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return Verdict{}, os.ErrNotExist
	}
	return decode(text)
}

func decode(text string) (Verdict, error) {
	var v Verdict
	if err := json.Unmarshal([]byte(stripFences(text)), &v); err != nil {
		return Verdict{}, fmt.Errorf("parse verdict: %w: %s", err, text)
	}
	if v.React && strings.TrimSpace(v.Task) == "" {
		return Verdict{}, errors.New("verdict asks to react without a task")
	}
	return v, nil
}

func collapse(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func stripFences(text string) string {
	if !strings.HasPrefix(text, "```") {
		return text
	}
	text = strings.TrimPrefix(text, "```json")
	text = strings.TrimPrefix(text, "```")
	if idx := strings.LastIndex(text, "```"); idx >= 0 {
		text = text[:idx]
	}
	return strings.TrimSpace(text)
}
