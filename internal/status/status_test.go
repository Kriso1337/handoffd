package status

import (
	"strings"
	"testing"
	"time"
)

func TestRenderShowsDaemonSignalThreadsAndLosses(t *testing.T) {
	r := Report{
		DaemonPID:       20192,
		DaemonAlive:     true,
		Agent:           "claude",
		SlackLogAge:     42 * time.Second,
		LastDecisionAge: 5 * time.Minute,
		Decisions24h:    "total 12, opened 2, silent 7\ntriage runs 8, react 1",
		DeadLetters:     2,
		Threads: []Thread{
			{Root: "1789156662.321049", Kind: "help", Window: "@261", WindowName: "help/teammate-2258", Live: true, Session: "working", Worktree: "", Members: []string{"codex:review_done"}},
			{Root: "1789047527.174689", Kind: "review", Window: "", Live: false, Worktree: "/repo/.claude/worktrees/review-899", Head: "08c9fc23a726", Progress: "review_done", ProgressSHA: "08c9fc23a726"},
		},
		Work:       "work 7d: work items 3: done 1 (handoff→done median 41m0s, p90 41m0s), open 1, stale 1, ended without DONE 0",
		Unfinished: []Unfinished{{Root: "1789000000.000001", Kind: "review", WindowName: "rev/PRJ-1", Status: "stale", Age: 7 * time.Hour}},
	}

	out := Render(r)

	for _, want := range []string{
		"watcher: running (pid 20192), agent claude",
		"slack log: last write 42s ago",
		"last decision: 5m0s ago",
		"threads: 2 (1 live)",
		"@261 help/teammate-2258 help session=working committee=codex:review_done",
		"review window closed worktree=/repo/.claude/worktrees/review-899 head=08c9fc23a726 progress=review_done@08c9fc23a726",
		"work 7d: work items 3: done 1",
		"1789000000.000001 rev/PRJ-1 review stale for 7h0m0s",
		"dead letters: 2 (handoffd replay)",
		"total 12, opened 2, silent 7",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status missing %q:\n%s", want, out)
		}
	}
}

func TestRenderWarnsWhenDaemonIsDownAndLogIsStale(t *testing.T) {
	r := Report{DaemonAlive: false, SlackLogAge: 5 * time.Hour, SlackLogStale: true}

	out := Render(r)

	if !strings.Contains(out, "watcher: NOT running") || !strings.Contains(out, "STALE") {
		t.Errorf("status = %s", out)
	}
}

func TestRenderWithoutJournalOrThreads(t *testing.T) {
	out := Render(Report{DaemonAlive: true, DaemonPID: 1, Agent: "codex"})

	if !strings.Contains(out, "threads: 0") || !strings.Contains(out, "dead letters: 0") {
		t.Errorf("status = %s", out)
	}
}

func TestShortLineForStatusBars(t *testing.T) {
	r := Report{DaemonAlive: true, DeadLetters: 1, SlackLogStale: true,
		Threads: []Thread{
			{Root: "1", Live: true, BlockedFor: 20 * time.Minute},
			{Root: "2", Live: true, BlockedFor: 2 * time.Minute},
			{Root: "3", Live: false},
		},
		Unfinished: []Unfinished{{Root: "9", Status: "stale"}},
	}

	if got := r.Short(15 * time.Minute); got != "2 live, 1 waiting, 1 unfinished, 1 dead, log stale" {
		t.Errorf("short = %q", got)
	}
	if got := (Report{DaemonAlive: true}).Short(15 * time.Minute); got != "0 live" {
		t.Errorf("quiet short = %q", got)
	}
	if got := (Report{}).Short(15 * time.Minute); got != "watcher down" {
		t.Errorf("down short = %q", got)
	}
}

func TestRenderShowsHowLongASessionWaits(t *testing.T) {
	out := Render(Report{DaemonAlive: true, Threads: []Thread{{Root: "1", Live: true, Window: "@1", WindowName: "rev/x", Kind: "review", Session: "blocked", BlockedFor: 16 * time.Minute}}})

	if !strings.Contains(out, "session=blocked waiting 16m0s") {
		t.Errorf("render = %s", out)
	}
}
