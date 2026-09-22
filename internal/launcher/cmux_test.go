package launcher

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeCmux struct {
	calls  [][]string
	out    map[string]string
	failOn string
}

func (f *fakeCmux) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	key := strings.Join(args, " ")
	if f.failOn != "" && strings.Contains(key, f.failOn) {
		return nil, errors.New("exit 1")
	}
	for prefix, out := range f.out {
		if strings.HasPrefix(key, prefix) {
			return []byte(out), nil
		}
	}
	return []byte("OK"), nil
}

const cmuxListJSON = `{
  "window_ref" : "window:1",
  "workspaces" : [
    { "current_directory" : "/Users/developer", "custom_title" : null, "has_custom_title" : false,
      "id" : "11111111-1111-1111-1111-111111111111", "index" : 0, "ref" : "workspace:1",
      "selected" : true, "title" : "developer@Dev-Mac:~" },
    { "current_directory" : "/tmp", "custom_title" : "rev/PRJ-8866", "has_custom_title" : true,
      "id" : "22222222-2222-2222-2222-222222222222", "index" : 1, "ref" : "workspace:4",
      "selected" : false, "title" : "rev/PRJ-8866" }
  ]
}`

func cmux(f *fakeCmux) Cmux {
	return NewCmux(f, "/opt/homebrew/bin/cmux", "/usr/local/bin/handoffd", "/cfg/config.json")
}

func TestCmuxWindowsCannotTellWhetherAWorkspaceHoldsAnAgent(t *testing.T) {
	f := &fakeCmux{out: map[string]string{"workspace list": cmuxListJSON}}

	windows, err := cmux(f).Windows(context.Background())

	if err != nil {
		t.Fatalf("Windows: %v", err)
	}
	if len(windows) != 2 {
		t.Fatalf("windows = %+v", windows)
	}
	for _, w := range windows {
		if w.Occupancy != OccupancyUnknown {
			t.Errorf("%s: occupancy = %s, cmux reports no liveness", w.ID, w.Occupancy)
		}
		if w.HoldsAgent() || !w.MayHoldAgent() {
			t.Errorf("%s must be neither known-live nor known-free: %+v", w.ID, w)
		}
	}
	if windows[0].Name != "developer@Dev-Mac:~" || windows[1].Name != "rev/PRJ-8866" {
		t.Errorf("names = %q, %q", windows[0].Name, windows[1].Name)
	}
	if windows[1].ID != "22222222-2222-2222-2222-222222222222" {
		t.Errorf("id = %q, a workspace must be addressed by its stable uuid", windows[1].ID)
	}
}

func TestCmuxWindowsUsesTheCanonicalListCommand(t *testing.T) {
	f := &fakeCmux{out: map[string]string{"workspace list": cmuxListJSON}}

	if _, err := cmux(f).Windows(context.Background()); err != nil {
		t.Fatalf("Windows: %v", err)
	}
	got := strings.Join(f.calls[0], " ")
	if !strings.Contains(got, "workspace list --json") {
		t.Errorf("call = %s, the legacy alias prints a notice before the json", got)
	}
	if strings.Contains(got, "list-workspaces") {
		t.Errorf("call = %s, list-workspaces is a deprecated alias", got)
	}
}

func TestCmuxFindMatchesTheWorkspaceUUID(t *testing.T) {
	f := &fakeCmux{out: map[string]string{"workspace list": cmuxListJSON}}

	w, ok, err := cmux(f).Find(context.Background(), "22222222-2222-2222-2222-222222222222")

	if err != nil || !ok {
		t.Fatalf("Find = (%+v, %v, %v)", w, ok, err)
	}
	if w.Name != "rev/PRJ-8866" {
		t.Errorf("window = %+v", w)
	}
}

func TestCmuxFindReportsAClosedWorkspaceAsAbsent(t *testing.T) {
	f := &fakeCmux{out: map[string]string{"workspace list": cmuxListJSON}}

	w, ok, err := cmux(f).Find(context.Background(), "00000000-0000-0000-0000-000000000000")

	if err != nil {
		t.Fatalf("a closed workspace is not an error: %v", err)
	}
	if ok || w.ID != "" {
		t.Errorf("window = %+v ok = %v", w, ok)
	}
}

func TestCmuxNewWindowResolvesTheCreatedRefToItsUUID(t *testing.T) {
	f := &fakeCmux{out: map[string]string{
		"workspace create": "OK workspace:4",
		"workspace list":   cmuxListJSON,
	}}
	c := NewCmux(f, "/opt/homebrew/bin/cmux", "/usr/local/bin/handoffd", "/cfg/Application Support/config.json")

	id, err := c.NewWindow(context.Background(), "rev/PRJ-8866", "/repo/wt", "/state/prompt-1.txt")

	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	if id != "22222222-2222-2222-2222-222222222222" {
		t.Fatalf("id = %q, want the uuid behind workspace:4", id)
	}
	create := f.calls[0]
	joined := strings.Join(create, " ")
	for _, want := range []string{"workspace create", "--name rev/PRJ-8866", "--cwd /repo/wt"} {
		if !strings.Contains(joined, want) {
			t.Errorf("create missing %q: %s", want, joined)
		}
	}
	var command string
	for i, arg := range create {
		if arg == "--command" && i+1 < len(create) {
			command = create[i+1]
		}
	}
	want := `'/usr/local/bin/handoffd' -config '/cfg/Application Support/config.json' run-agent '/state/prompt-1.txt'`
	if command != want {
		t.Errorf("command = %q, want %q", command, want)
	}
}

func TestCmuxNewWindowFailsWhenTheRefNeverAppears(t *testing.T) {
	f := &fakeCmux{out: map[string]string{
		"workspace create": "OK workspace:9",
		"workspace list":   cmuxListJSON,
	}}

	if _, err := cmux(f).NewWindow(context.Background(), "rev/PRJ-8866", "", "/state/p.txt"); err == nil {
		t.Fatal("a workspace that is not in the list must fail")
	}
}

func TestCmuxKillWindowTreatsAClosedWorkspaceAsDone(t *testing.T) {
	f := &fakeCmux{out: map[string]string{"workspace close": "Error: not_found: Workspace not found"}}

	if err := cmux(f).KillWindow(context.Background(), "22222222"); err != nil {
		t.Errorf("closing an already closed workspace must succeed: %v", err)
	}
}

func TestCmuxReportsATextErrorEvenOnAZeroExit(t *testing.T) {
	f := &fakeCmux{out: map[string]string{"workspace list": "Error: forbidden: socket control is cmuxOnly"}}

	_, err := cmux(f).Windows(context.Background())

	if err == nil {
		t.Fatal("cmux prints Error: on stdout and still exits zero; that must not read as success")
	}
	if !strings.Contains(err.Error(), "socket control is cmuxOnly") {
		t.Errorf("error = %v, it must carry the reason", err)
	}
}

func TestCmuxSendPromptTypesAndSubmits(t *testing.T) {
	f := &fakeCmux{}

	if err := cmux(f).SendPrompt(context.Background(), "22222222", "new head, reread"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("calls = %v", f.calls)
	}
	if got := strings.Join(f.calls[0], " "); !strings.Contains(got, "send --workspace 22222222 new head, reread") {
		t.Errorf("text call = %s", got)
	}
	if got := strings.Join(f.calls[1], " "); !strings.Contains(got, "send-key --workspace 22222222 enter") {
		t.Errorf("submit call = %s", got)
	}
}

func TestCmuxSendPromptRefusesMultilineText(t *testing.T) {
	f := &fakeCmux{}

	if err := cmux(f).SendPrompt(context.Background(), "22222222", "first\nsecond"); err == nil {
		t.Fatal("multi-line prompts must be refused")
	}
	if len(f.calls) != 0 {
		t.Errorf("nothing must be sent: %v", f.calls)
	}
}
