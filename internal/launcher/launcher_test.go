package launcher

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeCmd struct {
	calls  [][]string
	out    map[string]string
	failOn string
}

func (f *fakeCmd) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if f.failOn != "" && args[0] == f.failOn {
		if out, ok := f.out[args[0]]; ok {
			return []byte(out), errors.New("exit 1")
		}
		return []byte("tmux said no"), errors.New("exit 1")
	}
	return []byte(f.out[args[0]]), nil
}

func tmux(f *fakeCmd) Tmux {
	return New(f, "/opt/homebrew/bin/tmux", "main", "/usr/local/bin/handoffd", "/cfg/config.json")
}

func TestNewWindowPinsNameAgainstAutomaticRename(t *testing.T) {
	f := &fakeCmd{out: map[string]string{"new-window": "@42\n"}}

	id, err := tmux(f).NewWindow(context.Background(), "rev/PRJ-8866", "/repo/wt", "/state/prompt-1.txt")

	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	if id != "@42" {
		t.Errorf("id = %q", id)
	}
	if len(f.calls) != 2 {
		t.Fatalf("calls = %v", f.calls)
	}
	create := strings.Join(f.calls[0], " ")
	for _, want := range []string{"-n rev/PRJ-8866", "-c /repo/wt", "-config /cfg/config.json run-agent /state/prompt-1.txt", "-t main"} {
		if !strings.Contains(create, want) {
			t.Errorf("new-window missing %q: %s", want, create)
		}
	}
	pin := strings.Join(f.calls[1], " ")
	if !strings.Contains(pin, "set-option -w -t @42 automatic-rename off") {
		t.Errorf("automatic-rename not disabled: %s", pin)
	}
}

func TestNewWindowFailsOnEmptyID(t *testing.T) {
	f := &fakeCmd{out: map[string]string{"new-window": "\n"}}

	if _, err := tmux(f).NewWindow(context.Background(), "rev/PRJ-8866", "", "/p.txt"); err == nil {
		t.Error("want error when tmux returns no window id")
	}
}

func TestNewWindowReportsFailure(t *testing.T) {
	f := &fakeCmd{failOn: "new-window"}

	if _, err := tmux(f).NewWindow(context.Background(), "rev/PRJ-8866", "", "/p.txt"); err == nil {
		t.Error("want error from tmux failure")
	}
}

func TestWindowsParsesListing(t *testing.T) {
	f := &fakeCmd{out: map[string]string{
		"list-windows": "@109|codex|codex|0\n@158|rev/PRJ-8866|2.1.267|0\n@66|shell|zsh|0\n",
	}}

	got, err := tmux(f).Windows(context.Background())

	if err != nil {
		t.Fatalf("Windows: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 windows, got %d: %+v", len(got), got)
	}
	if !got[1].HoldsAgent() {
		t.Error("window running claude must count as holding an agent")
	}
	if got[2].HoldsAgent() {
		t.Error("window sitting at a shell must not count as holding an agent")
	}
}

func TestWindowsIgnoresMalformedLines(t *testing.T) {
	f := &fakeCmd{out: map[string]string{"list-windows": "@1|name\n\n@2|name|cmd|0\n"}}

	got, err := tmux(f).Windows(context.Background())

	if err != nil {
		t.Fatalf("Windows: %v", err)
	}
	if len(got) != 1 || got[0].ID != "@2" {
		t.Errorf("windows = %+v", got)
	}
}

func TestDeadWindowDoesNotHoldAgent(t *testing.T) {
	f := &fakeCmd{out: map[string]string{"list-windows": "@7|rev/PRJ-1|2.1.267|1\n"}}

	got, _ := tmux(f).Windows(context.Background())

	if got[0].HoldsAgent() {
		t.Error("dead pane must not count as holding an agent")
	}
}

func TestFindReturnsFalseForUnknownWindow(t *testing.T) {
	f := &fakeCmd{out: map[string]string{"list-windows": "@1|name|cmd|0\n"}}

	_, ok, err := tmux(f).Find(context.Background(), "@99")

	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if ok {
		t.Error("unknown window must not be found")
	}
}

func TestSendPromptTypesLiterallyThenSubmits(t *testing.T) {
	f := &fakeCmd{}

	if err := tmux(f).SendPrompt(context.Background(), "@42", "Thread continuation | review"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}

	if len(f.calls) != 2 {
		t.Fatalf("calls = %v", f.calls)
	}
	first := f.calls[0]
	if first[len(first)-1] != "Thread continuation | review" {
		t.Errorf("text not passed literally: %v", first)
	}
	if !strings.Contains(strings.Join(first, " "), "-l --") {
		t.Errorf("literal flag missing: %v", first)
	}
	if f.calls[1][len(f.calls[1])-1] != "Enter" {
		t.Errorf("prompt not submitted: %v", f.calls[1])
	}
}

func TestSendPromptRejectsMultilineText(t *testing.T) {
	f := &fakeCmd{}

	err := tmux(f).SendPrompt(context.Background(), "@42", "first\nsecond")

	if err == nil {
		t.Fatal("multiline text must be rejected")
	}
	if len(f.calls) != 0 {
		t.Errorf("nothing must be sent: %v", f.calls)
	}
}

func TestWindowsRejectsOutputWithSanitizedSeparators(t *testing.T) {
	f := &fakeCmd{out: map[string]string{
		"list-windows": "@162_2.1.267_2.1.267_0\n@169_rev/service-a!898_2.1.267_0\n",
	}}

	_, err := tmux(f).Windows(context.Background())

	if err == nil {
		t.Fatal("tmux without a locale replaces the separator; this must be an error, not an empty list")
	}
}

func TestWindowsReportsAnEmptySessionAsNoWindows(t *testing.T) {
	f := &fakeCmd{out: map[string]string{"list-windows": "  \n"}}

	got, err := tmux(f).Windows(context.Background())

	if err != nil {
		t.Fatalf("a session with no windows is not a failure: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("windows = %+v", got)
	}
}

func TestWindowsReportsAStoppedServerAsNoWindows(t *testing.T) {
	for _, message := range []string{
		"no server running on /tmp/tmux-501/default",
		"can't find session: main",
		"session not found: main",
	} {
		f := &fakeCmd{out: map[string]string{"list-windows": message}, failOn: "list-windows"}

		got, err := tmux(f).Windows(context.Background())

		if err != nil {
			t.Errorf("%q must read as absence, not failure: %v", message, err)
		}
		if len(got) != 0 {
			t.Errorf("%q: windows = %+v", message, got)
		}
	}
}

func TestWindowsStillReportsARealTmuxFailure(t *testing.T) {
	f := &fakeCmd{out: map[string]string{"list-windows": "permission denied"}, failOn: "list-windows"}

	if _, err := tmux(f).Windows(context.Background()); err == nil {
		t.Error("a tmux failure that is not an absent session must stay an error")
	}
}

func TestWindowsUsesPrintableSeparator(t *testing.T) {
	f := &fakeCmd{out: map[string]string{"list-windows": "@1|name|cmd|0\n"}}

	got, err := tmux(f).Windows(context.Background())

	if err != nil {
		t.Fatalf("Windows: %v", err)
	}
	if len(got) != 1 || got[0].ID != "@1" || got[0].Name != "name" {
		t.Errorf("windows = %+v", got)
	}
	format := strings.Join(f.calls[0], " ")
	if strings.Contains(format, "\t") {
		t.Error("the format must not contain control characters because tmux replaces them")
	}
}

func TestHoldOnExitKeepsPaneForCapture(t *testing.T) {
	f := &fakeCmd{}

	if err := tmux(f).HoldOnExit(context.Background(), "@42"); err != nil {
		t.Fatalf("HoldOnExit: %v", err)
	}

	got := strings.Join(f.calls[0], " ")
	if !strings.Contains(got, "set-option -w -t @42 remain-on-exit on") {
		t.Errorf("call = %q", got)
	}
}

func TestOccupancyDistinguishesFreeFromUnknown(t *testing.T) {
	cases := []struct {
		name   string
		window Window
		holds  bool
		may    bool
	}{
		{"agent", Window{Command: "claude", Occupancy: OccupancyAgent}, true, true},
		{"shell", Window{Command: "zsh", Occupancy: OccupancyFree}, false, false},
		{"dead", Window{Dead: true, Occupancy: OccupancyFree}, false, false},
		{"unreadable", Window{Occupancy: OccupancyUnknown}, false, true},
	}
	for _, c := range cases {
		if got := c.window.HoldsAgent(); got != c.holds {
			t.Errorf("%s: HoldsAgent = %v, want %v", c.name, got, c.holds)
		}
		if got := c.window.MayHoldAgent(); got != c.may {
			t.Errorf("%s: MayHoldAgent = %v, want %v", c.name, got, c.may)
		}
	}
}

func TestOccupancyOfADeadOrShellPaneIsFree(t *testing.T) {
	if got := occupancy(true, true, "claude"); got != OccupancyFree {
		t.Errorf("dead pane = %s, want free", got)
	}
	if got := occupancy(false, true, "bash"); got != OccupancyFree {
		t.Errorf("shell pane = %s, want free", got)
	}
	if got := occupancy(false, true, "claude"); got != OccupancyAgent {
		t.Errorf("agent pane = %s, want agent", got)
	}
	if got := occupancy(false, false, "claude"); got != OccupancyUnknown {
		t.Errorf("unreadable pane = %s, want unknown", got)
	}
}
