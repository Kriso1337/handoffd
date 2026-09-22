package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func persona() Persona {
	return Persona{
		Owner:             "Alex",
		OwnerGenitive:     "Alex",
		OwnerDative:       "Alex",
		OwnerFull:         "Alex Smith",
		OwnerFullGenitive: "Alex Smith",
		Agent:             "Atlas",
		Approvers:         "Alex",
		MainModel:         "Opus",
		TriageModel:       "Haiku",
		ThreadTool:        "Slack MCP",
	}
}

type strict struct {
	t *testing.T
	r Renderer
}

func wrap(t *testing.T, r Renderer) strict {
	return strict{t: t, r: r}
}

func renderer(t *testing.T) strict {
	t.Helper()
	r, err := New(persona(), "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return wrap(t, r)
}

func (s strict) must(out string, err error) string {
	s.t.Helper()
	if err != nil {
		s.t.Fatalf("render: %v", err)
	}
	return out
}

func (s strict) Initial(c Context) string { return s.must(s.r.Initial(c)) }

func (s strict) Continuation(c Context, review bool) string {
	return s.must(s.r.Continuation(c, review))
}

func (s strict) Triage(c TriageContext) string { return s.must(s.r.Triage(c)) }

func (s strict) Ask(c Context, task, reason string) string {
	return s.must(s.r.Ask(c, task, reason))
}

func (s strict) Results(c ResultsContext) string { return s.must(s.r.Results(c)) }

func (s strict) ReviewDone(d ReviewDone) string { return s.must(s.r.ReviewDone(d)) }

func reviewContext() Context {
	return Context{
		Workspace:     "acme",
		Channel:       "C100HOME",
		ChannelName:   "#agent-handoffs",
		AuthorID:      "U200PEER",
		AuthorName:    "teammate-b",
		MsgTS:         "1789049440.051109",
		ThreadTS:      "1789047527.174689",
		Text:          "[HANDOFF] review MR !899",
		Worktree:      "/Users/developer/projects/team-a/.claude/worktrees/review-899",
		MergeRef:      "refs/merge-requests/899/head",
		WorktreeSHA:   "08c9fc23a726a07692b7f9cf28d4ef6f57e4fec6",
		PostBin:       "/usr/local/bin/handoffd",
		Round:         1,
		ReviewEnabled: true,
		ReviewMode:    true,
	}
}

func TestPermalinkDropsDotFromTimestamp(t *testing.T) {
	got := Permalink("acme", "C100HOME", "1789049440.051109")
	want := "https://acme.slack.com/archives/C100HOME/p1789049440051109"
	if got != want {
		t.Errorf("permalink = %q, want %q", got, want)
	}
}

func TestDefaultRendererUsesEnglishReviewerProtocol(t *testing.T) {
	got := renderer(t).Initial(reviewContext())

	for _, want := range []string{
		"New mention in #agent-handoffs",
		"Author: teammate-b (U200PEER)",
		"Worktree: /Users/developer/projects/team-a/.claude/worktrees/review-899",
		"review unconditionally",
		"read-only",
		"post --thread 1789047527.174689 --review-done",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
}

func TestGenericSessionFocusesOnTimelyResponseWithoutReviewProtocol(t *testing.T) {
	c := reviewContext()
	c.ReviewEnabled = false
	c.ReviewMode = false
	c.Worktree = ""
	c.WorktreeSHA = ""

	got := renderer(t).Initial(c)

	for _, want := range []string{"help Alex respond promptly", "prepare a concise response", "Automatic MR/PR review is disabled"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{"review unconditionally", "--review-done", "--mr-note"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("generic prompt contains review protocol %q:\n%s", forbidden, got)
		}
	}
}

func TestInitialReportsRevisionMismatchAndDirtyWorktree(t *testing.T) {
	c := reviewContext()
	c.SHA = "08c9fc23a726a07692b7f9cf28d4ef6f57e4fec6"
	c.WorktreeSHA = "3bd416e4346c8de2f21af16dc0dd8728c0d0f1d5"
	c.HeadSHA = "4a8259ce0aa1b2c3d4e5f60718293a4b5c6d7e8f"
	c.WorktreeDirty = true
	c.WorktreeNote = "dirty worktree with two local changes"

	got := renderer(t).Initial(c)

	for _, want := range []string{"tree was NOT moved", "message names 08c9fc23", "dirty worktree with two local changes"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
}

func TestInitialNamesGitHubCommandsBaseAndTarget(t *testing.T) {
	c := reviewContext()
	c.Forge = "github"
	c.BaseSHA = "da85e99d6e589924215fb821669ac10e1172ecdd"
	c.TargetBranch = "main"

	got := renderer(t).Initial(c)

	for _, want := range []string{"base da85e99d6e589924215fb821669ac10e1172ecdd = main", "gh pr view/diff/checks"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
}

func TestContinuationIsSingleLineAndAnnouncesNewRound(t *testing.T) {
	c := reviewContext()
	c.Text = "[HEAD CHANGED]\nold -> new"
	c.HeadSHA = "4a8259ce0aa1b2c3d4e5f60718293a4b5c6d7e8f"
	c.Round = 2
	c.NewRound = true

	got := renderer(t).Continuation(c, true)

	for _, want := range []string{"round 2", "previous results are void", "git checkout --detach 4a8259ce", "--review-done"} {
		if !strings.Contains(got, want) {
			t.Errorf("continuation missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "\n") {
		t.Errorf("continuation must be one line: %s", got)
	}
}

func TestGenericContinuationContainsNoReviewInstructions(t *testing.T) {
	got := renderer(t).Continuation(reviewContext(), false)
	if !strings.Contains(got, "same request continues") || strings.Contains(got, "--review-done") {
		t.Errorf("continuation = %s", got)
	}
}

func TestTriagePromptCarriesThreadContextAndLabelledExamples(t *testing.T) {
	c := TriageContext{
		Context: Context{
			Workspace: "acme", SelfUserID: "U100SELF", Channel: "C200TEAM", AuthorID: "U400PEER",
			AuthorName: "teammate-a", MsgTS: "1789054475.628329", Text: "Alex, check both when you can",
			ReviewEnabled: false,
		},
		ChannelLabel: "#team-a-dev", SelfInThread: true, ThreadSize: 49,
		ThreadTail:  []TailReply{{AuthorName: "teammate-c", Text: "check MR 3 too"}},
		VerdictPath: "/state/verdicts/verdict.json",
		Namesake:    "Alex Twin (U300TWIN)",
		PeerAgents:  []string{"Ada", "Orbit"},
		Examples:    []Example{{Text: "weather chatter", React: false}, {Text: "check the MR", React: true}},
	}

	got := renderer(t).Triage(c)

	for _, want := range []string{
		"Atlas — a Opus agent working for Alex Smith",
		"surface a review request and prepare its context",
		"another Alex — Alex Twin",
		"Colleagues' agents also work in these chats: Ada, Orbit",
		"weather chatter\" → react: false",
		"/state/verdicts/verdict.json",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("triage prompt missing %q:\n%s", want, got)
		}
	}
}

func TestAskCarriesTaskReasonAndSafetyBoundary(t *testing.T) {
	got := renderer(t).Ask(reviewContext(), "Investigate the failed deployment", "direct request")

	for _, want := range []string{"Haiku decided to wake you: direct request", "Investigate the failed deployment", "Write nothing to Slack", "until Alex says so"} {
		if !strings.Contains(got, want) {
			t.Errorf("ask prompt missing %q:\n%s", want, got)
		}
	}
}

func TestOverrideDirectoryReplacesOnlyProvidedTemplates(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ask.tmpl"), []byte("CUSTOM {{.Persona.Agent}}: {{.Task}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := New(persona(), dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := wrap(t, r).Ask(reviewContext(), "investigate", ""); got != "CUSTOM Atlas: investigate" {
		t.Errorf("override = %q", got)
	}
	if !strings.Contains(wrap(t, r).Initial(reviewContext()), "New mention") {
		t.Error("templates without an override must use the embedded default")
	}
}

func TestUnknownLanguageIsRejected(t *testing.T) {
	if _, err := NewLocalized(persona(), "", "de"); err == nil {
		t.Error("unsupported language must fail at startup")
	}
}

func TestResultsAndReviewDoneUseEnglishProtocol(t *testing.T) {
	r := renderer(t)
	results := r.Results(ResultsContext{
		Root: "1.1", Round: 2, HeadSHA: "4a8259ce0aa1b2c3d4e5f60718293a4b5c6d7e8f", PostBin: "handoffd",
		Rows: []ResultRow{{Agent: "codex", Submitted: true, Blockers: 1, Others: 2, Decision: "rollback"}, {Agent: "pi"}},
	})
	for _, want := range []string{"Round 2 results collected", "codex: blockers 1, other 2", "pi: did not submit", "joint verdict"} {
		if !strings.Contains(results, want) {
			t.Errorf("results missing %q: %s", want, results)
		}
	}

	done := r.ReviewDone(ReviewDone{SHA: "4a8259ce0aa1", Agent: "codex", Blockers: 1, Others: 2, Decision: "do not merge", Stale: true, StaleRound: 2, StaleHead: "3bd416e4346c"})
	for _, want := range []string{"blockers 1, other 2", "decision needed: do not merge", "stale: current round 2"} {
		if !strings.Contains(done, want) {
			t.Errorf("review done missing %q: %s", want, done)
		}
	}
}
