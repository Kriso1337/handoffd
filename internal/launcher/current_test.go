package launcher

import (
	"context"
	"strings"
	"testing"
)

func env(pairs map[string]string) func(string) string {
	return func(key string) string { return pairs[key] }
}

func TestTmuxResolvesItsWindowFromThePaneInTheEnvironment(t *testing.T) {
	f := &fakeCmd{out: map[string]string{
		"display-message": "@26\n",
		"list-windows":    "@26|help/teammate-1200|claude|0\n",
	}}
	tm := New(f, "/opt/homebrew/bin/tmux", "main", "/bin/self", "/cfg/config.json")
	tm.env = env(map[string]string{"TMUX_PANE": "%26"})

	w, ok, err := tm.Current(context.Background())

	if err != nil || !ok {
		t.Fatalf("Current = (%+v, %v, %v)", w, ok, err)
	}
	if w.ID != "@26" || w.Name != "help/teammate-1200" {
		t.Errorf("window = %+v", w)
	}
	if got := strings.Join(f.calls[0], " "); !strings.Contains(got, "display-message -p -t %26 #{window_id}") {
		t.Errorf("resolve call = %s", got)
	}
}

func TestTmuxReportsNoWindowOutsideTmux(t *testing.T) {
	tm := New(&fakeCmd{}, "/opt/homebrew/bin/tmux", "main", "/bin/self", "/cfg/config.json")
	tm.env = env(nil)

	if _, ok, err := tm.Current(context.Background()); ok || err != nil {
		t.Errorf("Current = (%v, %v), want absence without an error", ok, err)
	}
}

func TestTmuxReportsNoWindowWhenTheServerIsGone(t *testing.T) {
	f := &fakeCmd{out: map[string]string{"display-message": "no server running on /tmp/tmux-501/default"}, failOn: "display-message"}
	tm := New(f, "/opt/homebrew/bin/tmux", "main", "/bin/self", "/cfg/config.json")
	tm.env = env(map[string]string{"TMUX_PANE": "%26"})

	if _, ok, err := tm.Current(context.Background()); ok || err != nil {
		t.Errorf("Current = (%v, %v), want absence without an error", ok, err)
	}
}

func TestTmuxRenamePinsTheNewName(t *testing.T) {
	f := &fakeCmd{}

	if err := tmux(f).Rename(context.Background(), "@26", "rev/PRJ-6135"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("calls = %v", f.calls)
	}
	if got := strings.Join(f.calls[0], " "); !strings.Contains(got, "rename-window -t @26 rev/PRJ-6135") {
		t.Errorf("rename call = %s", got)
	}
	if got := strings.Join(f.calls[1], " "); !strings.Contains(got, "automatic-rename off") {
		t.Errorf("name not pinned: %s", got)
	}
}

func TestHerdrResolvesItsPaneFromTheEnvironment(t *testing.T) {
	f := &fakeHerdr{out: map[string]string{
		"pane get":          herdrGetJSON,
		"pane process-info": herdrProcessJSON,
	}}
	h := NewHerdr(f, "/opt/homebrew/bin/herdr", "", "/bin/self", "/cfg/config.json")
	h.env = env(map[string]string{"HERDR_PANE_ID": "w1:p1"})

	w, ok, err := h.Current(context.Background())

	if err != nil || !ok || w.ID != "w1:p1" {
		t.Fatalf("Current = (%+v, %v, %v)", w, ok, err)
	}
}

func TestCmuxResolvesItsWorkspaceFromTheEnvironment(t *testing.T) {
	f := &fakeCmux{out: map[string]string{"workspace list": cmuxListJSON}}
	c := NewCmux(f, "/opt/homebrew/bin/cmux", "/bin/self", "/cfg/config.json")
	c.env = env(map[string]string{"CMUX_WORKSPACE_ID": "22222222-2222-2222-2222-222222222222"})

	w, ok, err := c.Current(context.Background())

	if err != nil || !ok || w.Name != "rev/PRJ-8866" {
		t.Fatalf("Current = (%+v, %v, %v)", w, ok, err)
	}
}

func TestWezTermResolvesItsPaneFromTheEnvironment(t *testing.T) {
	f := &fakeCmd{out: map[string]string{
		"cli": `[{"pane_id":7,"title":"claude","tab_title":"rev/x","tty_name":"/dev/ttys007"}]`,
		"-A":  "ttys007 claude\n",
	}}
	w := NewWezTerm(f, "/bin/wezterm", "/bin/self", "/cfg/config.json")
	w.env = env(map[string]string{"WEZTERM_PANE": "7"})

	got, ok, err := w.Current(context.Background())

	if err != nil || !ok || got.ID != "7" {
		t.Fatalf("Current = (%+v, %v, %v)", got, ok, err)
	}
}

func TestEveryBackendReportsAbsenceOutsideItsTerminal(t *testing.T) {
	ctx := context.Background()
	tm := New(&fakeCmd{}, "/b", "main", "/s", "/c")
	tm.env = env(nil)
	wz := NewWezTerm(&fakeCmd{}, "/b", "/s", "/c")
	wz.env = env(nil)
	hd := NewHerdr(&fakeHerdr{}, "/b", "", "/s", "/c")
	hd.env = env(nil)
	cm := NewCmux(&fakeCmux{}, "/b", "/s", "/c")
	cm.env = env(nil)

	for name, backend := range map[string]Backend{"tmux": tm, "wezterm": wz, "herdr": hd, "cmux": cm} {
		w, ok, err := backend.Current(ctx)
		if ok || err != nil {
			t.Errorf("%s: Current = (%+v, %v, %v), want absence without an error", name, w, ok, err)
		}
	}
}
