package tailer

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type collector struct {
	mu    sync.Mutex
	lines []string
}

func (c *collector) add(line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, line)
}

func (c *collector) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.lines...)
}

func (c *collector) waitFor(t *testing.T, want int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := c.snapshot(); len(got) >= want {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout: want %d lines, got %v", want, c.snapshot())
	return nil
}

func appendTo(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatalf("append: %v", err)
	}
}

func startTailer(t *testing.T, tl *Tailer) *collector {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := &collector{}
	go tl.Run(ctx, c.add)
	return c
}

func TestReadsAppendedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(path, []byte("old line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := startTailer(t, New(path, 5*time.Millisecond))
	time.Sleep(30 * time.Millisecond)

	appendTo(t, path, "first\nsecond\n")

	got := c.waitFor(t, 2)
	if got[0] != "first" || got[1] != "second" {
		t.Errorf("lines = %v", got)
	}
}

func TestSkipsExistingContentByDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(path, []byte("history\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := startTailer(t, New(path, 5*time.Millisecond))
	time.Sleep(50 * time.Millisecond)

	for _, line := range c.snapshot() {
		if line == "history" {
			t.Error("pre-existing line must not be replayed")
		}
	}
}

func TestWaitsForCompleteLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c := startTailer(t, New(path, 5*time.Millisecond))
	time.Sleep(30 * time.Millisecond)

	appendTo(t, path, "par")
	time.Sleep(30 * time.Millisecond)
	if len(c.snapshot()) != 0 {
		t.Fatalf("partial line emitted: %v", c.snapshot())
	}

	appendTo(t, path, "tial\n")

	if got := c.waitFor(t, 1); got[0] != "partial" {
		t.Errorf("line = %q", got[0])
	}
}

func TestFollowsFileReplacedByRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c := startTailer(t, New(path, 5*time.Millisecond))
	time.Sleep(30 * time.Millisecond)
	appendTo(t, path, "before\n")
	c.waitFor(t, 1)

	if err := os.Rename(path, filepath.Join(dir, "log1")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("after\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := c.waitFor(t, 2)
	if got[1] != "after" {
		t.Errorf("lines = %v, want second line from the new file", got)
	}
}

func TestRereadsTruncatedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(path, []byte("aaaaaaaaaa\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := startTailer(t, New(path, 5*time.Millisecond))
	time.Sleep(30 * time.Millisecond)

	if err := os.WriteFile(path, []byte("fresh\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := c.waitFor(t, 1)
	if got[0] != "fresh" {
		t.Errorf("line = %q", got[0])
	}
}

func TestFromHeadReplaysExistingContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := startTailer(t, NewFromHead(path, 5*time.Millisecond))

	got := c.waitFor(t, 2)
	if got[0] != "one" || got[1] != "two" {
		t.Errorf("lines = %v", got)
	}
}

func TestRunFailsWhenFileMissing(t *testing.T) {
	tl := New(filepath.Join(t.TempDir(), "absent"), 5*time.Millisecond)

	if err := tl.Run(context.Background(), func(string) {}); err == nil {
		t.Error("want error for missing file")
	}
}
