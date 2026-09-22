package launcher

import (
	"context"
	"strings"
	"testing"
)

func wez(f *fakeCmd) WezTerm {
	return NewWezTerm(f, "/usr/local/bin/wezterm", "/usr/local/bin/handoffd", "/cfg/config.json")
}

func TestWezTermSpawnsAgentPaneAndNamesItsTab(t *testing.T) {
	f := &fakeCmd{out: map[string]string{"cli": "7\n"}}

	id, err := wez(f).NewWindow(context.Background(), "rev/PRJ-8866", "/repo/wt", "/state/prompt-1.json")

	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	if id != "7" {
		t.Errorf("id = %q", id)
	}
	spawn := strings.Join(f.calls[0], " ")
	for _, want := range []string{"cli spawn", "--cwd /repo/wt", "-- /usr/local/bin/handoffd -config /cfg/config.json run-agent /state/prompt-1.json"} {
		if !strings.Contains(spawn, want) {
			t.Errorf("spawn %q missing %q", spawn, want)
		}
	}
	if title := strings.Join(f.calls[1], " "); !strings.Contains(title, "cli set-tab-title --pane-id 7 rev/PRJ-8866") {
		t.Errorf("title call = %q", title)
	}
}

func TestWezTermRejectsNonNumericPaneID(t *testing.T) {
	f := &fakeCmd{out: map[string]string{"cli": "error: no mux server\n"}}

	if _, err := wez(f).NewWindow(context.Background(), "x", "", "/p"); err == nil {
		t.Error("garbage pane id must be an error")
	}
}

func TestWezTermListsPanesAsWindows(t *testing.T) {
	f := &fakeCmd{out: map[string]string{
		"cli": `[{"window_id":0,"tab_id":3,"pane_id":7,"workspace":"default","title":"claude","tab_title":"rev/PRJ-8866","cwd":"file:///repo/wt","tty_name":"/dev/ttys007"},
{"window_id":0,"tab_id":4,"pane_id":9,"workspace":"default","title":"zsh","cwd":"file:///home","tty_name":"/dev/ttys009"}]`,
		"-A": "ttys007 -zsh\nttys007 /Users/me/.local/bin/claude\nttys009 -zsh\n??       launchd\n",
	}}

	got, err := wez(f).Windows(context.Background())

	if err != nil {
		t.Fatalf("Windows: %v", err)
	}
	if len(got) != 2 || got[0].ID != "7" || got[0].Name != "rev/PRJ-8866" || got[1].Name != "zsh" {
		t.Errorf("windows = %+v", got)
	}
	if got[0].Command != "claude" || !got[0].HoldsAgent() {
		t.Errorf("pane with an agent process on its tty = %+v", got[0])
	}
	if got[1].Command != "zsh" || got[1].HoldsAgent() {
		t.Errorf("pane that fell back to the shell = %+v", got[1])
	}
	win, ok, err := wez(f).Find(context.Background(), "9")
	if err != nil || !ok || win.ID != "9" {
		t.Errorf("Find = %+v %v %v", win, ok, err)
	}
	if _, ok, _ := wez(f).Find(context.Background(), "42"); ok {
		t.Error("unknown pane must not be found")
	}
}

func TestWezTermSendPromptTypesTextWithReturn(t *testing.T) {
	f := &fakeCmd{}

	if err := wez(f).SendPrompt(context.Background(), "7", "Continuation | review"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}

	call := f.calls[0]
	if !strings.Contains(strings.Join(call, " "), "cli send-text --pane-id 7 --no-paste") || call[len(call)-1] != "Continuation | review\r" {
		t.Errorf("call = %q", call)
	}
	if err := wez(f).SendPrompt(context.Background(), "7", "a\nb"); err == nil {
		t.Error("multiline must be rejected")
	}
}

func TestWezTermKillAndCaptureTargetThePane(t *testing.T) {
	f := &fakeCmd{out: map[string]string{"cli": "screen text"}}

	if err := wez(f).KillWindow(context.Background(), "7"); err != nil {
		t.Fatalf("KillWindow: %v", err)
	}
	text, err := wez(f).CapturePane(context.Background(), "7")
	if err != nil || text != "screen text" {
		t.Errorf("CapturePane = %q, %v", text, err)
	}
	if !strings.Contains(strings.Join(f.calls[0], " "), "cli kill-pane --pane-id 7") {
		t.Errorf("kill call = %q", f.calls[0])
	}
	if !strings.Contains(strings.Join(f.calls[1], " "), "cli get-text --pane-id 7 --start-line -400") {
		t.Errorf("capture call = %q", f.calls[1])
	}
	if err := wez(f).HoldOnExit(context.Background(), "7"); err != nil {
		t.Errorf("HoldOnExit is a no-op for wezterm: %v", err)
	}
}

func TestForegroundByTTYPrefersNonShellProcesses(t *testing.T) {
	got := foregroundByTTY("ttys001 -zsh\nttys001 node\nttys001 /bin/sleep\nttys002 bash\nttys003 /opt/codex\nttys003 -zsh\n?? kernel_task\n")

	if got["ttys001"] != "node" || got["ttys002"] != "bash" || got["ttys003"] != "codex" {
		t.Errorf("foreground = %v", got)
	}
	if _, ok := got["??"]; ok {
		t.Error("processes without a tty must be ignored")
	}
}

func TestWezTermReportsUnknownOccupancyWhenPsFails(t *testing.T) {
	f := &fakeCmd{out: map[string]string{"cli": `[{"pane_id":7,"title":"claude","tab_title":"rev/x","tty_name":"/dev/ttys007"}]`}, failOn: "-A"}

	got, err := wez(f).Windows(context.Background())

	if err != nil || len(got) != 1 {
		t.Fatalf("windows = %+v err = %v", got, err)
	}
	w := got[0]
	if w.Occupancy != OccupancyUnknown {
		t.Errorf("occupancy = %s, a pane whose process could not be read is not known to hold an agent", w.Occupancy)
	}
	if w.HoldsAgent() {
		t.Error("an unreadable pane must not be reported as holding an agent")
	}
	if !w.MayHoldAgent() {
		t.Error("an unreadable pane must not be reported as free either")
	}
}

func TestWezTermReportsUnknownOccupancyForATTYPsDoesNotList(t *testing.T) {
	f := &fakeCmd{out: map[string]string{
		"cli": `[{"pane_id":7,"title":"claude","tab_title":"rev/x","tty_name":"/dev/ttys007"}]`,
		"-A":  "ttys001 node\n",
	}}

	got, err := wez(f).Windows(context.Background())

	if err != nil || len(got) != 1 {
		t.Fatalf("windows = %+v err = %v", got, err)
	}
	if got[0].Occupancy != OccupancyUnknown {
		t.Errorf("occupancy = %s, a tty missing from ps is not a known state", got[0].Occupancy)
	}
}

func TestWezTermReportsAShellPaneAsFree(t *testing.T) {
	f := &fakeCmd{out: map[string]string{
		"cli": `[{"pane_id":7,"title":"zsh","tab_title":"rev/x","tty_name":"/dev/ttys007"}]`,
		"-A":  "ttys007 -zsh\n",
	}}

	got, err := wez(f).Windows(context.Background())

	if err != nil || len(got) != 1 {
		t.Fatalf("windows = %+v err = %v", got, err)
	}
	if got[0].Occupancy != OccupancyFree || got[0].MayHoldAgent() {
		t.Errorf("a pane back at the shell must be free: %+v", got[0])
	}
}
