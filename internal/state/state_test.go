package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Kriso1337/handoffd/internal/mrref"
)

func open(t *testing.T, capacity int) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(path, capacity)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s, path
}

func markSeen(t *testing.T, s *Store, id string) {
	t.Helper()
	if err := s.CommitSeen(id); err != nil {
		t.Fatalf("CommitSeen(%s): %v", id, err)
	}
}

func TestSeenSurvivesReopen(t *testing.T) {
	s, path := open(t, 10)
	markSeen(t, s, "T100_1789049440.051109")
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reopened, err := Open(path, 10)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if !reopened.Seen("T100_1789049440.051109") {
		t.Error("seen id lost after restart")
	}
}

func TestSeenEvictsOldestBeyondCapacity(t *testing.T) {
	s, _ := open(t, 2)

	markSeen(t, s, "a")
	markSeen(t, s, "b")
	markSeen(t, s, "c")

	if s.Seen("a") {
		t.Error("oldest id must be evicted")
	}
	if !s.Seen("b") || !s.Seen("c") {
		t.Error("recent ids must be kept")
	}
}

func TestCommitSeenIsIdempotent(t *testing.T) {
	s, _ := open(t, 2)

	markSeen(t, s, "a")
	markSeen(t, s, "a")
	markSeen(t, s, "b")

	if !s.Seen("a") || !s.Seen("b") {
		t.Error("duplicate mark must not evict live ids")
	}
}

func TestThreadRoundTrip(t *testing.T) {
	s, path := open(t, 10)
	s.SetThread("1789047527.174689", Thread{
		WindowID:   "@42",
		WindowName: "rev/PRJ-8866",
		Worktree:   "/repo/.claude/worktrees/review-899",
		Kind:       "review",
		UpdatedAt:  time.Now().UTC(),
	})
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reopened, _ := Open(path, 10)
	got, ok := reopened.Thread("1789047527.174689")

	if !ok {
		t.Fatal("thread lost after restart")
	}
	if got.WindowID != "@42" || got.Kind != "review" {
		t.Errorf("thread = %+v", got)
	}
}

func TestLaunchLimitBlocksAfterQuota(t *testing.T) {
	s, _ := open(t, 10)
	now := time.Now()
	window := 10 * time.Minute

	for i := 0; i < 2; i++ {
		if !s.AllowLaunch(now, 2, window) {
			t.Fatalf("launch %d must be allowed", i)
		}
		s.RecordLaunch(now, window)
	}

	if s.AllowLaunch(now, 2, window) {
		t.Error("third launch must be blocked")
	}
}

func TestLaunchLimitForgetsOldLaunches(t *testing.T) {
	s, _ := open(t, 10)
	window := 10 * time.Minute
	past := time.Now().Add(-time.Hour)
	s.RecordLaunch(past, window)
	s.RecordLaunch(past, window)

	if !s.AllowLaunch(time.Now(), 2, window) {
		t.Error("launches outside the window must not count")
	}
}

func TestMissingStateFileStartsEmpty(t *testing.T) {
	s, _ := open(t, 10)

	if s.Seen("anything") {
		t.Error("fresh store must be empty")
	}
	if _, ok := s.Thread("1789047527.174689"); ok {
		t.Error("fresh store must have no threads")
	}
}

func TestSweepKeepsARecentThreadWhoseWindowStoppedHoldingAnAgent(t *testing.T) {
	s, _ := open(t, 10)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	s.SetThread("live", Thread{WindowID: "@1", UpdatedAt: now.Add(-time.Hour)})
	s.SetThread("dead", Thread{WindowID: "@2", UpdatedAt: now.Add(-time.Hour)})
	s.SetThread("expired", Thread{WindowID: "@3", UpdatedAt: now.Add(-40 * 24 * time.Hour)})
	alive := func(th Thread) bool { return th.WindowID != "@2" }

	dropped := s.Sweep(now, 30*24*time.Hour, 7*24*time.Hour, alive)

	if len(dropped) != 1 || dropped[0] != "expired" {
		t.Fatalf("dropped = %v", dropped)
	}
	if _, ok := s.Thread("live"); !ok {
		t.Error("live thread must survive")
	}
	if _, ok := s.Thread("dead"); !ok {
		t.Error("a window that stopped holding an agent must not take the round with it at once")
	}
	if _, ok := s.Thread("expired"); ok {
		t.Error("thread older than ttl must be dropped even when its window lives")
	}
}

func TestThreadKeepsMergeRequestIdentityAndSession(t *testing.T) {
	s, path := open(t, 10)
	s.SetThread("root", Thread{
		WindowID:  "@1",
		MR:        &mrref.MR{Host: "gitlab.example.com", Namespace: "team-a", Project: "service-a", IID: 899},
		HeadSHA:   "08c9fc23a726a07692b7f9cf28d4ef6f57e4fec6",
		BaseSHA:   "da85e99d6e589924215fb821669ac10e1172ecdd",
		SessionID: "33333333-3333-3333-3333-333333333333",
	})
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reopened, err := Open(path, 10)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	got, _ := reopened.Thread("root")
	if got.MR == nil || got.MR.Key() != "gitlab.example.com/team-a/service-a!899" {
		t.Errorf("mr = %+v", got.MR)
	}
	if got.HeadSHA == "" || got.BaseSHA == "" || got.SessionID == "" {
		t.Errorf("thread = %+v", got)
	}
}

func TestSweepKeepsRecordsOfClosedWindowsUntilTheirOwnTTL(t *testing.T) {
	s, _ := open(t, 10)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	s.SetThread("fresh-gone", Thread{Kind: "review", Worktree: "/wt", UpdatedAt: now.Add(-time.Hour)})
	s.SetThread("stale-gone", Thread{Kind: "review", UpdatedAt: now.Add(-8 * 24 * time.Hour)})
	alive := func(Thread) bool { return false }

	dropped := s.Sweep(now, 30*24*time.Hour, 7*24*time.Hour, alive)

	if len(dropped) != 1 || dropped[0] != "stale-gone" {
		t.Errorf("dropped = %v", dropped)
	}
	if th, ok := s.Thread("fresh-gone"); !ok || !th.WindowGone() {
		t.Error("record of a recently closed window must survive as thread memory")
	}
}

func TestSilentVerdictsCountPerThreadWithinWindow(t *testing.T) {
	s, path := open(t, 10)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	window := time.Hour

	if got := s.RecordSilent("root-a", now.Add(-90*time.Minute), window); got != 1 {
		t.Errorf("first streak = %d", got)
	}
	s.RecordSilent("root-a", now.Add(-10*time.Minute), window)
	s.RecordSilent("root-b", now.Add(-5*time.Minute), window)
	if got := s.RecordSilent("root-a", now, window); got != 2 {
		t.Errorf("streak after an old verdict fell out = %d, want 2", got)
	}
	if got := s.SilentCount("root-b", now, window); got != 1 {
		t.Errorf("root-b streak = %d", got)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.SilentCount("root-a", now, window); got != 2 {
		t.Errorf("streak lost after restart: %d", got)
	}

	reopened.ClearSilent("root-a")
	if got := reopened.SilentCount("root-a", now, window); got != 0 {
		t.Errorf("streak after clear = %d", got)
	}
	if got := reopened.SilentCount("root-b", now.Add(2*time.Hour), window); got != 0 {
		t.Errorf("streak outside the window = %d", got)
	}
}

func TestOpenRoundArchivesThePreviousRun(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	th := Thread{SessionID: "sess-1", Agent: "claude", Members: []Member{{Agent: "codex", SessionID: "sess-2"}}}

	first := th.OpenRound("aaa1111", "bbb2222", RunOpenedByHandoff, now)
	th.Run.Results["codex"] = Result{SHA: "aaa1111", Blockers: 1, At: now}
	second := th.OpenRound("ccc3333", "bbb2222", RunOpenedByHeadChanged, now.Add(time.Hour))

	if first.Round != 1 || second.Round != 2 || th.Run.Round != 2 || len(th.Run.Results) != 0 {
		t.Errorf("rounds = %+v %+v current=%+v", first, second, th.Run)
	}
	if len(th.RunsHistory) != 1 || th.RunsHistory[0].Results["codex"].Blockers != 1 || th.RunsHistory[0].HeadSHA != "aaa1111" {
		t.Errorf("history = %+v", th.RunsHistory)
	}
	if agent, driver, ok := th.Participant("sess-1"); !ok || !driver || agent != "claude" {
		t.Errorf("driver lookup = %q %v %v", agent, driver, ok)
	}
	if agent, driver, ok := th.Participant("sess-2"); !ok || driver || agent != "codex" {
		t.Errorf("member lookup = %q %v %v", agent, driver, ok)
	}
	if _, _, ok := th.Participant("sess-9"); ok {
		t.Error("unknown session must not be a participant")
	}
	if key := (Thread{}).DriverKey(); key != DriverKey {
		t.Errorf("driver key without an agent = %q", key)
	}
}

func TestSaveFailsWhenTheTempPathIsADirectory(t *testing.T) {
	s, path := open(t, 10)
	if err := os.Mkdir(path+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}

	err := s.Save()

	if err == nil {
		t.Fatal("Save writes through a temp file; a directory in its place must fail")
	}
	if !strings.Contains(err.Error(), "write state") {
		t.Errorf("err = %v, want the write step named", err)
	}
	if !errors.Is(err, syscall.EISDIR) {
		t.Errorf("err = %v, want EISDIR: a type refusal holds for root, a permission one does not", err)
	}
}

func TestCommitSeenRollsBackWhenTheStateCannotBeWritten(t *testing.T) {
	s, path := open(t, 10)
	if err := os.Mkdir(path+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}

	if err := s.CommitSeen("T100_1789049440.051109"); err == nil {
		t.Fatal("CommitSeen must report a failed write")
	}
	if s.Seen("T100_1789049440.051109") {
		t.Error("a seen mark that was not persisted must not stay in memory")
	}
}

func TestAnOpenRoundSurvivesAReloadWithResultsItCanRecordInto(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(path, 10)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	thread := Thread{WindowID: "@1", SessionID: "sess-1"}
	thread.OpenRound("head", "base", RunOpenedByHandoff, time.Now().UTC())
	thread.OpenRound("head", "base", RunOpenedByHeadChanged, time.Now().UTC())
	s.SetThread("1789049440.051109", thread)
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reopened, err := Open(path, 10)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	got, ok := reopened.Thread("1789049440.051109")
	if !ok {
		t.Fatal("thread not reloaded")
	}
	if got.Run.Results == nil {
		t.Error("a reloaded round must be able to take a result without panicking")
	}
	if len(got.RunsHistory) != 1 || got.RunsHistory[0].Results == nil {
		t.Errorf("history = %+v, a finished round must reload the same way", got.RunsHistory)
	}
}

func TestARoundRecordsAResultIntoAMapItNeverGot(t *testing.T) {
	run := Run{Round: 1}

	run.Record("codex", Result{Blockers: 1})

	if got, ok := run.Results["codex"]; !ok || got.Blockers != 1 {
		t.Errorf("results = %+v", run.Results)
	}
}

func TestACorruptStateIsQuarantinedInsteadOfKeepingTheWatcherDown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{\"threads\": {broken"), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path, 10)

	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	moved, ok := s.Recovered()
	if !ok {
		t.Fatal("a state that cannot be parsed must be quarantined, not lost and not fatal")
	}
	raw, readErr := os.ReadFile(moved)
	if readErr != nil || !strings.Contains(string(raw), "broken") {
		t.Errorf("quarantined file = %q %v, the unreadable state must survive for a human", raw, readErr)
	}
	if len(s.Threads()) != 0 {
		t.Errorf("threads = %+v, want an empty state to work from", s.Threads())
	}
	s.SetThread("root", Thread{WindowID: "@1"})
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

func TestAReadableStateReportsNothingQuarantined(t *testing.T) {
	s, _ := open(t, 10)

	if moved, ok := s.Recovered(); ok {
		t.Errorf("recovered = %q, nothing was wrong with this state", moved)
	}
}

func liveRound() Thread {
	thread := Thread{WindowID: "@1", SessionID: "sess-1", Members: []Member{{Agent: "codex", WindowID: "@2"}}}
	thread.OpenRound("head", "base", RunOpenedByHandoff, time.Date(2026, 9, 15, 18, 0, 0, 0, time.UTC))
	thread.Run.Record("codex", Result{Blockers: 1})
	thread.Run.Notes = []Note{{Agent: "codex", Text: "svc.go:40"}}
	return thread
}

func TestAThreadHandedOutCannotBeChangedBehindTheStoresBack(t *testing.T) {
	s, _ := open(t, 10)
	s.SetThread("root", liveRound())

	got, _ := s.Thread("root")
	got.Run.Record("codex", Result{Blockers: 99})
	got.Run.HeadSHA = "moved"
	got.Run.Notes[0].Text = "rewritten"
	got.Members[0].Agent = "stranger"

	kept, _ := s.Thread("root")
	if kept.Run.Results["codex"].Blockers != 1 || kept.Run.HeadSHA != "head" {
		t.Errorf("run = %+v, a copy handed out must not reach the stored record", kept.Run)
	}
	if kept.Run.Notes[0].Text != "svc.go:40" || kept.Members[0].Agent != "codex" {
		t.Errorf("thread = %+v", kept)
	}
}

func TestAStoredThreadKeepsWhatTheCallerStillHolds(t *testing.T) {
	s, _ := open(t, 10)
	thread := liveRound()
	s.SetThread("root", thread)

	thread.Run.HeadSHA = "moved"
	thread.Members[0].Agent = "stranger"

	kept, _ := s.Thread("root")
	if kept.Run.HeadSHA != "head" || kept.Members[0].Agent != "codex" {
		t.Errorf("thread = %+v, the store must not share memory with the caller", kept)
	}
}

func TestATransitionAppliesToTheRecordAsItIsNow(t *testing.T) {
	s, path := open(t, 10)
	s.SetThread("root", liveRound())
	stale, _ := s.Thread("root")

	if err := s.UpdateThread("root", func(th *Thread) error {
		th.Progress = "review_start"
		return nil
	}); err != nil {
		t.Fatalf("UpdateThread: %v", err)
	}
	stale.LastSeenTS = "1789490000.000100"
	if err := s.UpdateThread("root", func(th *Thread) error {
		th.LastSeenTS = stale.LastSeenTS
		return nil
	}); err != nil {
		t.Fatalf("UpdateThread: %v", err)
	}

	got, _ := s.Thread("root")
	if got.Progress != "review_start" || got.LastSeenTS != "1789490000.000100" {
		t.Errorf("thread = %+v, a late writer must not undo the transition before it", got)
	}
	reopened, err := Open(path, 10)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if kept, _ := reopened.Thread("root"); kept.Progress != "review_start" || kept.LastSeenTS != "1789490000.000100" {
		t.Errorf("reloaded = %+v, a transition must be persisted with the change", kept)
	}
}

func TestAFailedTransitionLeavesTheRecordAlone(t *testing.T) {
	s, _ := open(t, 10)
	s.SetThread("root", liveRound())

	err := s.UpdateThread("root", func(th *Thread) error {
		th.Progress = "review_done"
		return errors.New("no")
	})

	if err == nil {
		t.Fatal("UpdateThread must report what the transition said")
	}
	if got, _ := s.Thread("root"); got.Progress != "" {
		t.Errorf("thread = %+v, a refused transition must not be applied", got)
	}
}

func TestATransitionOnAThreadThatIsGoneSaysSo(t *testing.T) {
	s, _ := open(t, 10)

	err := s.UpdateThread("missing", func(*Thread) error { return nil })

	if err == nil {
		t.Fatal("UpdateThread must say the thread is gone instead of creating one")
	}
}

func TestSweepDropsAThreadWhoseWindowIsGoneOnceItsOwnTTLPasses(t *testing.T) {
	s, _ := open(t, 10)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	s.SetThread("stale-dead", Thread{WindowID: "@2", UpdatedAt: now.Add(-8 * 24 * time.Hour)})
	alive := func(Thread) bool { return false }

	dropped := s.Sweep(now, 30*24*time.Hour, 7*24*time.Hour, alive)

	if len(dropped) != 1 || dropped[0] != "stale-dead" {
		t.Errorf("dropped = %v", dropped)
	}
}
