package waitq

import (
	"path/filepath"
	"testing"
	"time"
)

var at = time.Date(2026, 9, 15, 19, 0, 0, 0, time.UTC)

func dir(t *testing.T) Dir {
	t.Helper()
	return New(filepath.Join(t.TempDir(), "waiting"))
}

func prompt(id, window, text string, offset time.Duration) Prompt {
	return Prompt{ID: id, At: at.Add(offset), WindowID: window, SessionID: "sess-1", ThreadTS: "1789049440.051109", Text: text}
}

func TestPromptsComeBackInTheOrderTheyWereQueued(t *testing.T) {
	d := dir(t)
	if err := d.Append(prompt("b", "@1", "second", time.Second)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := d.Append(prompt("a", "@1", "first", 0)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := d.Append(prompt("c", "@2", "another window", 0)); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got, err := d.Peek("@1")

	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(got) != 2 || got[0].Text != "first" || got[1].Text != "second" {
		t.Errorf("prompts = %+v", got)
	}
}

func TestAPromptSurvivesTheProcessThatQueuedIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "waiting")
	if err := New(path).Append(prompt("a", "@1", "survive the restart", 0)); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got, err := New(path).Peek("@1")

	if err != nil || len(got) != 1 || got[0].Text != "survive the restart" {
		t.Fatalf("prompts = %+v, err = %v", got, err)
	}
}

func TestADeliveredPromptIsGone(t *testing.T) {
	d := dir(t)
	if err := d.Append(prompt("a", "@1", "delivered", 0)); err != nil {
		t.Fatalf("Append: %v", err)
	}

	if err := d.Remove("a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	got, _ := d.Peek("@1")
	if len(got) != 0 {
		t.Errorf("prompts = %+v", got)
	}
}

func TestWindowsListsOnlyWindowsThatStillWait(t *testing.T) {
	d := dir(t)
	if err := d.Append(prompt("a", "@1", "waits", 0)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := d.Append(prompt("b", "@2", "waits too", 0)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := d.Remove("b"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	windows, err := d.Windows()

	if err != nil {
		t.Fatalf("Windows: %v", err)
	}
	if len(windows) != 1 || windows[0] != "@1" {
		t.Errorf("windows = %v", windows)
	}
}

func TestAnUndeliverablePromptIsKeptAsideNotDeleted(t *testing.T) {
	d := dir(t)
	if err := d.Append(prompt("a", "@1", "window no longer exists", 0)); err != nil {
		t.Fatalf("Append: %v", err)
	}

	moved, err := d.Quarantine("@1", "window is gone", at)

	if err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	if moved != 1 {
		t.Errorf("moved = %d", moved)
	}
	if got, _ := d.Peek("@1"); len(got) != 0 {
		t.Errorf("prompts = %+v, a quarantined prompt must not be delivered", got)
	}
	kept, err := filepath.Glob(filepath.Join(d.Path(), "undelivered", "*.json"))
	if err != nil || len(kept) != 1 {
		t.Errorf("kept = %v, err = %v, the text must survive for a human", kept, err)
	}
}
