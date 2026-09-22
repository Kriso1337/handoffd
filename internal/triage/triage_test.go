package triage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func waiter(t *testing.T) (Waiter, string) {
	t.Helper()
	dir := t.TempDir()
	return NewWaiter(dir, 300*time.Millisecond, 10*time.Millisecond), dir
}

func TestPathIsDerivedFromTimestamp(t *testing.T) {
	w, dir := waiter(t)

	got := w.Path("1789054475.628329")

	if got != filepath.Join(dir, "verdict-1789054475628329.json") {
		t.Errorf("path = %q", got)
	}
}

func TestWaitReadsVerdictAndRemovesFile(t *testing.T) {
	w, _ := waiter(t)
	path := w.Path("1.1")
	if err := os.WriteFile(path, []byte(`{"react":true,"reason":"asked to check MR","task":"Review MR 3"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := w.Wait(context.Background(), path)

	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !got.React || got.Task != "Review MR 3" {
		t.Errorf("verdict = %+v", got)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("verdict file must be removed after reading")
	}
}

func TestWaitAcceptsFencedJSON(t *testing.T) {
	w, _ := waiter(t)
	path := w.Path("1.2")
	body := "```json\n{\"react\": false, \"reason\": \"chatter\", \"task\": \"\"}\n```"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := w.Wait(context.Background(), path)

	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got.React {
		t.Errorf("verdict = %+v, want react false", got)
	}
}

func TestWaitPicksUpFileWrittenLater(t *testing.T) {
	w, _ := waiter(t)
	path := w.Path("1.3")
	go func() {
		time.Sleep(40 * time.Millisecond)
		os.WriteFile(path, []byte(`{"react":false,"reason":"not actionable","task":""}`), 0o600)
	}()

	got, err := w.Wait(context.Background(), path)

	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got.React {
		t.Error("want react false")
	}
}

func TestWaitTimesOutWhenNothingWritten(t *testing.T) {
	w, _ := waiter(t)

	_, err := w.Wait(context.Background(), w.Path("1.4"))

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want deadline exceeded", err)
	}
}

func TestWaitRejectsReactWithoutTask(t *testing.T) {
	w, _ := waiter(t)
	path := w.Path("1.5")
	if err := os.WriteFile(path, []byte(`{"react":true,"reason":"actionable","task":"  "}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := w.Wait(context.Background(), path); err == nil {
		t.Error("react without a task must be rejected")
	}
}

func TestWaitRejectsGarbage(t *testing.T) {
	w, _ := waiter(t)
	path := w.Path("1.6")
	if err := os.WriteFile(path, []byte("actionable after consideration"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := w.Wait(context.Background(), path); err == nil {
		t.Error("non-JSON verdict must be rejected")
	}
}

func TestWaitIgnoresEmptyFileUntilWritten(t *testing.T) {
	w, _ := waiter(t)
	path := w.Path("1.7")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(40 * time.Millisecond)
		os.WriteFile(path, []byte(`{"react":false,"reason":"x","task":""}`), 0o600)
	}()

	if _, err := w.Wait(context.Background(), path); err != nil {
		t.Errorf("Wait: %v", err)
	}
}

func TestWaitStopsOnCancelledContext(t *testing.T) {
	w, _ := waiter(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := w.Wait(ctx, w.Path("1.8")); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want canceled", err)
	}
}

func TestFromPaneTakesLastJSONObject(t *testing.T) {
	pane := "prompt with an example {\"react\": true|false}\n⏺ {\n  \"react\": false,\n  \"reason\": \"chatter\",\n  \"task\": \"\"\n}\n❯"

	got, err := FromPane(pane)

	if err != nil {
		t.Fatalf("FromPane: %v", err)
	}
	if got.React || got.Reason != "chatter" {
		t.Errorf("verdict = %+v", got)
	}
}

func TestFromPaneFailsWithoutJSON(t *testing.T) {
	if _, err := FromPane("no verdict-like output"); err == nil {
		t.Error("want error when pane has no JSON")
	}
}

func TestFromPaneRejectsReactWithoutTask(t *testing.T) {
	if _, err := FromPane(`⏺ {"react": true, "reason": "actionable", "task": ""}`); err == nil {
		t.Error("react without task must be rejected even from pane")
	}
}

func TestPeekReadsVerdictOnceWithoutWaiting(t *testing.T) {
	dir := t.TempDir()
	w := NewWaiter(dir, time.Second, time.Millisecond)
	path := w.Path("1.1")
	if _, ok := w.Peek(path); ok {
		t.Fatal("missing file must not yield a verdict")
	}
	if err := os.WriteFile(path, []byte(`{"react": false, "reason": "no"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	got, ok := w.Peek(path)

	if !ok || got.Reason != "no" {
		t.Errorf("verdict = %+v ok = %v", got, ok)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("consumed verdict file must be removed")
	}
}
