package launcher

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeHerdr struct {
	calls  [][]string
	out    map[string]string
	failOn string
	exitOn string
}

func (f *fakeHerdr) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	key := strings.Join(args, " ")
	for _, arg := range args {
		if f.failOn != "" && arg == f.failOn {
			return []byte("herdr said no"), errors.New("exit 1")
		}
	}
	body := ""
	for prefix, out := range f.out {
		if strings.HasPrefix(key, prefix) {
			body = out
			break
		}
	}
	for _, arg := range args {
		if f.exitOn != "" && arg == f.exitOn {
			return []byte(body), errors.New("exit status 1")
		}
	}
	if body == "" {
		return nil, nil
	}
	return []byte(body), nil
}

const (
	herdrPaneListJSON = `{"id":"cli:pane:list","result":{"panes":[{"agent_status":"working","cwd":"/repo/wt",` +
		`"focused":true,"label":"rev/PRJ-8866","pane_id":"w1:p1","revision":1,"tab_id":"w1:t1",` +
		`"terminal_id":"term_65b6e3","terminal_title":"claude","workspace_id":"w1"}],"type":"pane_list"}}`
	herdrProcessJSON = `{"id":"cli:pane:process_info","result":{"process_info":{"foreground_process_group_id":11883,` +
		`"foreground_processes":[{"argv":["claude"],"argv0":"claude","cmdline":"claude","cwd":"/repo/wt",` +
		`"name":"claude","pid":11883}],"pane_id":"w1:p1","shell_pid":11845},"type":"pane_process_info"}}`
	herdrShellProcessJSON = `{"id":"cli:pane:process_info","result":{"process_info":{"foreground_process_group_id":90,` +
		`"foreground_processes":[{"argv":["-zsh"],"argv0":"-zsh","cmdline":"-zsh","name":"zsh","pid":90}],` +
		`"pane_id":"w1:p1","shell_pid":90},"type":"pane_process_info"}}`
	herdrNotFoundJSON = `{"error":{"code":"pane_not_found","message":"pane w9:p9 not found"},"id":"cli:pane:get"}`
	herdrGetJSON      = `{"id":"cli:pane:get","result":{"pane":{"agent_status":"idle","label":"rev/PRJ-8866",` +
		`"pane_id":"w1:p1","revision":1,"tab_id":"w1:t1","terminal_id":"t","workspace_id":"w1"},"type":"pane_info"}}`
	herdrCreateJSON = `{"id":"cli:workspace:create","result":{"root_pane":{"agent_status":"unknown","cwd":"/repo/wt",` +
		`"focused":true,"pane_id":"w3:p1","revision":0,"tab_id":"w3:t1","terminal_id":"t","workspace_id":"w3"},` +
		`"type":"workspace_created","workspace":{"label":"rev/PRJ-8866","workspace_id":"w3"}}}`
)

func herdr(f *fakeHerdr) Herdr {
	return NewHerdr(f, "/opt/homebrew/bin/herdr", "", "/usr/local/bin/handoffd", "/cfg/config.json")
}

func TestHerdrWindowsReportTheForegroundCommand(t *testing.T) {
	f := &fakeHerdr{out: map[string]string{
		"pane list":         herdrPaneListJSON,
		"pane process-info": herdrProcessJSON,
	}}

	windows, err := herdr(f).Windows(context.Background())

	if err != nil {
		t.Fatalf("Windows: %v", err)
	}
	if len(windows) != 1 {
		t.Fatalf("windows = %+v", windows)
	}
	w := windows[0]
	if w.ID != "w1:p1" || w.Name != "rev/PRJ-8866" || w.Command != "claude" {
		t.Errorf("window = %+v", w)
	}
	if !w.HoldsAgent() {
		t.Error("a pane running an agent must hold it")
	}
}

func TestHerdrPaneBackAtTheShellHoldsNoAgent(t *testing.T) {
	f := &fakeHerdr{out: map[string]string{
		"pane list":         herdrPaneListJSON,
		"pane process-info": herdrShellProcessJSON,
	}}

	windows, err := herdr(f).Windows(context.Background())

	if err != nil {
		t.Fatalf("Windows: %v", err)
	}
	if windows[0].Command != "zsh" || windows[0].HoldsAgent() {
		t.Errorf("a pane back at the shell must not hold an agent: %+v", windows[0])
	}
}

func TestHerdrWindowsFailWhenAPaneProcessCannotBeRead(t *testing.T) {
	f := &fakeHerdr{out: map[string]string{"pane list": herdrPaneListJSON}, failOn: "process-info"}

	if _, err := herdr(f).Windows(context.Background()); err == nil {
		t.Fatal("an unreadable pane process must not be reported as a live agent")
	}
}

func TestHerdrFindReportsAMissingPaneAsAbsent(t *testing.T) {
	f := &fakeHerdr{out: map[string]string{"pane get": herdrNotFoundJSON}, exitOn: "get"}

	w, ok, err := herdr(f).Find(context.Background(), "w9:p9")

	if err != nil {
		t.Fatalf("a missing pane is not an error: %v", err)
	}
	if ok || w.ID != "" {
		t.Errorf("window = %+v ok = %v", w, ok)
	}
}

func TestHerdrFindReportsOtherFailuresAsErrors(t *testing.T) {
	f := &fakeHerdr{out: map[string]string{
		"pane get": `{"error":{"code":"server_unavailable","message":"no server"},"id":"cli:pane:get"}`,
	}, exitOn: "get"}

	if _, _, err := herdr(f).Find(context.Background(), "w1:p1"); err == nil {
		t.Fatal("a server failure must not read as a missing pane")
	}
}

func TestHerdrFindReturnsTheLivePane(t *testing.T) {
	f := &fakeHerdr{out: map[string]string{
		"pane get":          herdrGetJSON,
		"pane process-info": herdrProcessJSON,
	}}

	w, ok, err := herdr(f).Find(context.Background(), "w1:p1")

	if err != nil || !ok {
		t.Fatalf("Find = (%+v, %v, %v)", w, ok, err)
	}
	if w.ID != "w1:p1" || w.Name != "rev/PRJ-8866" || w.Command != "claude" {
		t.Errorf("window = %+v", w)
	}
}

func TestHerdrNewWindowLabelsThePaneAndRunsTheAgent(t *testing.T) {
	f := &fakeHerdr{out: map[string]string{"workspace create": herdrCreateJSON}}
	h := NewHerdr(f, "/opt/homebrew/bin/herdr", "", "/usr/local/bin/handoffd", "/cfg/Application Support/config.json")

	id, err := h.NewWindow(context.Background(), "rev/PRJ-8866", "/repo/wt", "/state/prompt-1.txt")

	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	if id != "w3:p1" {
		t.Fatalf("id = %q", id)
	}
	if len(f.calls) != 3 {
		t.Fatalf("calls = %v", f.calls)
	}
	create := strings.Join(f.calls[0], " ")
	for _, want := range []string{"workspace create", "--label rev/PRJ-8866", "--cwd /repo/wt"} {
		if !strings.Contains(create, want) {
			t.Errorf("create missing %q: %s", want, create)
		}
	}
	if rename := strings.Join(f.calls[1], " "); !strings.Contains(rename, "pane rename w3:p1 rev/PRJ-8866") {
		t.Errorf("pane not labelled: %s", rename)
	}
	run := f.calls[2]
	if len(run) != 5 || run[1] != "pane" || run[2] != "run" || run[3] != "w3:p1" {
		t.Fatalf("run call = %v", run)
	}
	want := `'/usr/local/bin/handoffd' -config '/cfg/Application Support/config.json' run-agent '/state/prompt-1.txt'`
	if run[4] != want {
		t.Errorf("run command = %q, want %q", run[4], want)
	}
}

func TestHerdrNewWindowFailsOnAnEmptyPaneID(t *testing.T) {
	f := &fakeHerdr{out: map[string]string{
		"workspace create": `{"id":"cli:workspace:create","result":{"type":"workspace_created"}}`,
	}}

	if _, err := herdr(f).NewWindow(context.Background(), "rev/PRJ-8866", "", "/state/p.txt"); err == nil {
		t.Fatal("a workspace with no root pane must fail")
	}
}

func TestHerdrKillWindowTreatsAMissingPaneAsDone(t *testing.T) {
	f := &fakeHerdr{out: map[string]string{"pane close": herdrNotFoundJSON}, exitOn: "close"}

	if err := herdr(f).KillWindow(context.Background(), "w9:p9"); err != nil {
		t.Errorf("closing an already gone pane must succeed: %v", err)
	}
}

func TestHerdrSendPromptTypesAndSubmits(t *testing.T) {
	f := &fakeHerdr{}

	if err := herdr(f).SendPrompt(context.Background(), "w1:p1", "new head, reread"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("calls = %v", f.calls)
	}
	if got := strings.Join(f.calls[0], " "); !strings.Contains(got, "pane send-text w1:p1 new head, reread") {
		t.Errorf("text call = %s", got)
	}
	if got := strings.Join(f.calls[1], " "); !strings.Contains(got, "pane send-keys w1:p1 enter") {
		t.Errorf("submit call = %s", got)
	}
}

func TestHerdrSendPromptRefusesMultilineText(t *testing.T) {
	f := &fakeHerdr{}

	if err := herdr(f).SendPrompt(context.Background(), "w1:p1", "first\nsecond"); err == nil {
		t.Fatal("multi-line prompts must be refused")
	}
	if len(f.calls) != 0 {
		t.Errorf("nothing must be sent: %v", f.calls)
	}
}

func TestHerdrRoutesEveryCallThroughItsNamedSession(t *testing.T) {
	f := &fakeHerdr{out: map[string]string{"--session review pane close": `{"id":"c","result":{"type":"ok"}}`}}
	h := NewHerdr(f, "/opt/homebrew/bin/herdr", "review", "/usr/local/bin/handoffd", "/cfg/config.json")

	if err := h.KillWindow(context.Background(), "w1:p1"); err != nil {
		t.Fatalf("KillWindow: %v", err)
	}
	if got := strings.Join(f.calls[0], " "); !strings.Contains(got, "--session review pane close w1:p1") {
		t.Errorf("session not routed: %s", got)
	}
}

func TestHerdrAcceptsTheSilentSuccessOfARunCommand(t *testing.T) {
	f := &fakeHerdr{}

	if err := herdr(f).KillWindow(context.Background(), "w1:p1"); err != nil {
		t.Errorf("herdr prints nothing on a plain success: %v", err)
	}
}

func TestHerdrReportsAFailureThatPrintsNothing(t *testing.T) {
	f := &fakeHerdr{failOn: "close"}

	if err := herdr(f).KillWindow(context.Background(), "w1:p1"); err == nil {
		t.Error("a failing herdr call must be reported even with no output")
	}
}
