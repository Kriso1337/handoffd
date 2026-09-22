package launcher

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

type realCmd struct{}

func (realCmd) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func TestSmokeHerdrAgainstALiveServer(t *testing.T) {
	if os.Getenv("HERDR_SMOKE") == "" {
		t.Skip("set HERDR_SMOKE=1 with a running herdr server")
	}
	ctx := context.Background()
	h := NewHerdr(realCmd{}, "/opt/homebrew/bin/herdr", "", "/bin/echo", "/cfg/Application Support/config.json")

	id, err := h.NewWindow(ctx, "rev/SMOKE-1", "/tmp", "/state/prompt-smoke.txt")
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	t.Logf("created pane %s", id)
	t.Cleanup(func() {
		if err := h.KillWindow(ctx, id); err != nil {
			t.Errorf("KillWindow: %v", err)
		}
	})
	time.Sleep(1500 * time.Millisecond)

	windows, err := h.Windows(ctx)
	if err != nil {
		t.Fatalf("Windows: %v", err)
	}
	var found bool
	for _, w := range windows {
		t.Logf("pane %s name=%q command=%q holds=%v", w.ID, w.Name, w.Command, w.HoldsAgent())
		if w.ID == id {
			found = true
			if w.Name != "rev/SMOKE-1" {
				t.Errorf("name = %q, want the label we set", w.Name)
			}
		}
	}
	if !found {
		t.Fatalf("created pane %s missing from %+v", id, windows)
	}

	w, ok, err := h.Find(ctx, id)
	if err != nil || !ok || w.ID != id {
		t.Fatalf("Find(%s) = (%+v, %v, %v)", id, w, ok, err)
	}

	if _, ok, err := h.Find(ctx, "w99:p99"); err != nil || ok {
		t.Errorf("Find(missing) = (%v, %v), want absent with no error", ok, err)
	}

	text, err := h.CapturePane(ctx, id)
	if err != nil {
		t.Fatalf("CapturePane: %v", err)
	}
	t.Logf("captured: %q", strings.TrimSpace(text))
	if !strings.Contains(text, "run-agent") {
		t.Errorf("capture must show the command that ran: %q", text)
	}
}

func TestSmokeCmuxAgainstARunningApp(t *testing.T) {
	if os.Getenv("CMUX_SMOKE") == "" {
		t.Skip("set CMUX_SMOKE=1 with cmux running and CMUX_SOCKET_PASSWORD exported")
	}
	ctx := context.Background()
	c := NewCmux(realCmd{}, "/opt/homebrew/bin/cmux", "/bin/echo", "/cfg/Application Support/config.json")

	id, err := c.NewWindow(ctx, "rev/SMOKE-2", "/tmp", "/state/prompt-smoke.txt")
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	t.Logf("created workspace %s", id)
	t.Cleanup(func() {
		if err := c.KillWindow(ctx, id); err != nil {
			t.Errorf("KillWindow: %v", err)
		}
	})
	time.Sleep(1500 * time.Millisecond)

	windows, err := c.Windows(ctx)
	if err != nil {
		t.Fatalf("Windows: %v", err)
	}
	var found bool
	for _, w := range windows {
		t.Logf("workspace %s name=%q occupancy=%s", w.ID, w.Name, w.Occupancy)
		if w.ID == id {
			found = true
			if w.Name != "rev/SMOKE-2" {
				t.Errorf("name = %q, want the name we set", w.Name)
			}
		}
	}
	if !found {
		t.Fatalf("created workspace %s missing from the listing", id)
	}

	w, ok, err := c.Find(ctx, id)
	if err != nil || !ok || w.ID != id {
		t.Fatalf("Find(%s) = (%+v, %v, %v)", id, w, ok, err)
	}
	if _, ok, err := c.Find(ctx, "00000000-0000-0000-0000-000000000000"); err != nil || ok {
		t.Errorf("Find(missing) = (%v, %v), want absent with no error", ok, err)
	}

	text, err := c.CapturePane(ctx, id)
	if err != nil {
		t.Fatalf("CapturePane: %v", err)
	}
	t.Logf("captured: %q", strings.TrimSpace(text))
	if !strings.Contains(text, "run-agent") {
		t.Errorf("capture must show the command that ran: %q", text)
	}
}
