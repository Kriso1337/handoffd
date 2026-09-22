package linkq

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kriso1337/handoffd/internal/launcher"
	"github.com/Kriso1337/handoffd/internal/mrref"
	"github.com/Kriso1337/handoffd/internal/state"
	"github.com/Kriso1337/handoffd/internal/worktree"
)

type fakeWindows struct {
	windows []launcher.Window
	renamed map[string]string
	err     error
}

func newFakeWindows(windows ...launcher.Window) *fakeWindows {
	return &fakeWindows{windows: windows, renamed: map[string]string{}}
}

func (f *fakeWindows) Windows(context.Context) ([]launcher.Window, error) {
	return f.windows, f.err
}

func (f *fakeWindows) Find(_ context.Context, id string) (launcher.Window, bool, error) {
	if f.err != nil {
		return launcher.Window{}, false, f.err
	}
	for _, w := range f.windows {
		if w.ID == id {
			return w, true, nil
		}
	}
	return launcher.Window{}, false, nil
}

func (f *fakeWindows) Rename(_ context.Context, id, name string) error {
	if f.err != nil {
		return f.err
	}
	f.renamed[id] = name
	for i, w := range f.windows {
		if w.ID == id {
			f.windows[i].Name = name
		}
	}
	return nil
}

func agentWindow(id, name string) launcher.Window {
	return launcher.Window{ID: id, Name: name, Command: "claude", Occupancy: launcher.OccupancyAgent}
}

func store(t *testing.T) *state.Store {
	t.Helper()
	s, err := state.Open(filepath.Join(t.TempDir(), "state.json"), 10)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	return s
}

func deps(windows Windows, s *state.Store) Deps {
	return Deps{
		Windows:       windows,
		ReviewEnabled: true,
		Store:         s,
		Now:           func() time.Time { return now },
		NewID:         func() string { return "session-new" },
	}
}

func TestApplyRenamesTheWindowAndRecordsTheThread(t *testing.T) {
	windows := newFakeWindows(agentWindow("@115", "help/Alex-Pro-1448"))
	s := store(t)

	rc, err := Apply(context.Background(), request("a"), deps(windows, s))

	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rc.Status != StatusLinked || rc.WindowName != "sky/OPS-1718" || rc.SessionID != "session-new" {
		t.Fatalf("receipt = %+v", rc)
	}
	if windows.renamed["@115"] != "sky/OPS-1718" {
		t.Errorf("renamed = %v", windows.renamed)
	}
	thread, ok := s.Thread("1789480920.729569")
	if !ok {
		t.Fatal("thread not recorded")
	}
	if thread.WindowID != "@115" || thread.WindowName != "sky/OPS-1718" || thread.Kind != "other" {
		t.Errorf("thread = %+v", thread)
	}
	if thread.Channel != "C0000000002" || thread.OpenedTS != "1789480920.729569" || thread.LastSeenTS != "1789480920.729569" {
		t.Errorf("thread = %+v", thread)
	}
}

func TestApplyRejectsARequestFromAWindowThatIsGone(t *testing.T) {
	windows := newFakeWindows()
	s := store(t)

	rc, err := Apply(context.Background(), request("a"), deps(windows, s))

	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rc.Status != StatusRejected || !strings.Contains(rc.Reason, "@115") {
		t.Fatalf("receipt = %+v", rc)
	}
	if _, ok := s.Thread("1789480920.729569"); ok {
		t.Error("a rejected request must not record a thread")
	}
}

func TestApplyRejectsATakeoverOfAThreadHeldByAnotherLiveWindow(t *testing.T) {
	windows := newFakeWindows(agentWindow("@115", "help/Alex-Pro-1448"), agentWindow("@116", "sky/teammate-1716"))
	s := store(t)
	s.SetThread("1789480920.729569", state.Thread{WindowID: "@116", WindowName: "sky/teammate-1716", SessionID: "session-held"})

	rc, err := Apply(context.Background(), request("a"), deps(windows, s))

	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rc.Status != StatusRejected || !strings.Contains(rc.Reason, "@116") || !strings.Contains(rc.Reason, "--force") {
		t.Fatalf("receipt = %+v", rc)
	}
	if thread, _ := s.Thread("1789480920.729569"); thread.WindowID != "@116" {
		t.Errorf("thread = %+v, want the holder untouched", thread)
	}
	if len(windows.renamed) != 0 {
		t.Errorf("a rejected request must not rename anything: %v", windows.renamed)
	}
}

func TestApplyTakesOverWithForceAndReleasesTheHeldWindow(t *testing.T) {
	windows := newFakeWindows(agentWindow("@115", "help/Alex-Pro-1448"), agentWindow("@116", "sky/teammate-1716"))
	s := store(t)
	held := state.Thread{WindowID: "@116", WindowName: "sky/teammate-1716", SessionID: "session-held"}
	s.SetThread("1789480920.729569", held)
	var released []string
	d := deps(windows, s)
	d.Released = func(root string, thread state.Thread) {
		released = append(released, root+" "+thread.WindowID+" "+thread.SessionID)
	}
	req := request("a")
	req.Force = true

	rc, err := Apply(context.Background(), req, d)

	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rc.Status != StatusLinked || rc.WindowID != "@115" {
		t.Fatalf("receipt = %+v", rc)
	}
	if len(released) != 1 || released[0] != "1789480920.729569 @116 session-held" {
		t.Errorf("released = %v, want the previous holder handed back once", released)
	}
	if thread, _ := s.Thread("1789480920.729569"); thread.WindowID != "@115" || thread.SessionID != "session-new" {
		t.Errorf("thread = %+v", thread)
	}
}

func TestApplyLinksAThreadWhoseWindowDiedWithoutForce(t *testing.T) {
	windows := newFakeWindows(agentWindow("@115", "help/Alex-Pro-1448"))
	s := store(t)
	s.SetThread("1789480920.729569", state.Thread{WindowID: "@116", WindowName: "sky/teammate-1716", SessionID: "session-dead"})

	rc, err := Apply(context.Background(), request("a"), deps(windows, s))

	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rc.Status != StatusLinked {
		t.Fatalf("receipt = %+v", rc)
	}
	if thread, _ := s.Thread("1789480920.729569"); thread.WindowID != "@115" {
		t.Errorf("thread = %+v", thread)
	}
}

func TestApplyKeepsTheSessionAlreadyRunningInTheWindow(t *testing.T) {
	windows := newFakeWindows(agentWindow("@115", "help/Alex-Pro-1448"))
	s := store(t)
	s.SetThread("1789472867.446279", state.Thread{WindowID: "@115", WindowName: "help/Alex-Pro-1448", SessionID: "session-live"})

	rc, err := Apply(context.Background(), request("a"), deps(windows, s))

	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rc.SessionID != "session-live" {
		t.Fatalf("receipt = %+v, want the session of the window it links", rc)
	}
	if thread, _ := s.Thread("1789480920.729569"); thread.SessionID != "session-live" {
		t.Errorf("thread = %+v", thread)
	}
}

func TestApplyNamesAroundAWindowThatAlreadyCarriesTheName(t *testing.T) {
	windows := newFakeWindows(agentWindow("@115", "help/Alex-Pro-1448"), agentWindow("@120", "sky/OPS-1718"))
	s := store(t)

	rc, err := Apply(context.Background(), request("a"), deps(windows, s))

	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rc.WindowName != "sky/OPS-1718-2" {
		t.Errorf("name = %q, want a name that does not collide", rc.WindowName)
	}
}

func TestApplyReportsATerminalThatCannotBeReached(t *testing.T) {
	windows := newFakeWindows(agentWindow("@115", "help/Alex-Pro-1448"))
	windows.err = errors.New("no server running")
	s := store(t)

	if _, err := Apply(context.Background(), request("a"), deps(windows, s)); err == nil {
		t.Fatal("Apply must report a terminal failure instead of rejecting the request")
	}
}

func TestApplyStoresTheThreadOnDisk(t *testing.T) {
	windows := newFakeWindows(agentWindow("@115", "help/Alex-Pro-1448"))
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := state.Open(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	d := deps(windows, s)

	if _, err := Apply(context.Background(), request("a"), d); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	reopened, err := state.Open(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Thread("1789480920.729569"); !ok {
		t.Error("the thread must survive in the state file, not only in memory")
	}
}

type fakeRevisions struct {
	result worktree.Result
	err    error
	mrs    []mrref.MR
}

func (f *fakeRevisions) Revision(_ context.Context, mr mrref.MR) (worktree.Result, error) {
	f.mrs = append(f.mrs, mr)
	return f.result, f.err
}

const (
	linkedHeadSHA = "3bd416e4346c8de2f21af16dc0dd8728c0d0f1d5"
	linkedBaseSHA = "da85e99d6e589924215fb821669ac10e1172ecdd"
	linkedMRURL   = "https://gitlab.example.com/team-a/service-a/-/merge_requests/899"
)

func reviewRequest(id string) Request {
	req := request(id)
	req.Kind = "review"
	req.Refs = mrref.Extract(linkedMRURL)
	req.Worktree = "/repo/.claude/worktrees/review-899"
	return req
}

func reviewDeps(windows Windows, s *state.Store, revisions Revisions) Deps {
	d := deps(windows, s)
	d.Revisions = revisions
	return d
}

func TestApplyPinsTheRevisionOfALinkedReview(t *testing.T) {
	windows := newFakeWindows(agentWindow("@115", "help/Alex-Pro-1448"))
	s := store(t)
	revisions := &fakeRevisions{result: worktree.Result{HeadSHA: linkedHeadSHA, BaseSHA: linkedBaseSHA}}

	rc, err := Apply(context.Background(), reviewRequest("a"), reviewDeps(windows, s, revisions))

	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rc.Status != StatusLinked {
		t.Fatalf("receipt = %+v", rc)
	}
	thread, ok := s.Thread("1789480920.729569")
	if !ok {
		t.Fatal("thread not recorded")
	}
	if thread.HeadSHA != linkedHeadSHA || thread.BaseSHA != linkedBaseSHA {
		t.Errorf("thread = %+v, want the review pinned to the provider head and base", thread)
	}
	if len(revisions.mrs) != 1 || revisions.mrs[0].IID != 899 {
		t.Errorf("revision read for %+v", revisions.mrs)
	}
}

func TestApplyRejectsALinkedReviewWhoseRevisionIsUnreadable(t *testing.T) {
	windows := newFakeWindows(agentWindow("@115", "help/Alex-Pro-1448"))
	s := store(t)
	revisions := &fakeRevisions{err: errors.New("fetch refs/merge-requests/899/head failed")}

	rc, err := Apply(context.Background(), reviewRequest("a"), reviewDeps(windows, s, revisions))

	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rc.Status != StatusRejected || !strings.Contains(rc.Reason, "899") {
		t.Fatalf("receipt = %+v", rc)
	}
	if _, ok := s.Thread("1789480920.729569"); ok {
		t.Error("a review that cannot be pinned must not become a thread")
	}
	if len(windows.renamed) != 0 {
		t.Errorf("a rejected request must not rename anything: %v", windows.renamed)
	}
}

func TestApplyRejectsALinkedReviewWhoseBaseIsUnknown(t *testing.T) {
	windows := newFakeWindows(agentWindow("@115", "help/Alex-Pro-1448"))
	s := store(t)
	revisions := &fakeRevisions{result: worktree.Result{HeadSHA: linkedHeadSHA}}

	rc, err := Apply(context.Background(), reviewRequest("a"), reviewDeps(windows, s, revisions))

	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rc.Status != StatusRejected || !strings.Contains(rc.Reason, "base") {
		t.Fatalf("receipt = %+v", rc)
	}
	if _, ok := s.Thread("1789480920.729569"); ok {
		t.Error("a review without a confirmed base must not become a thread")
	}
}

func TestApplyRejectsALinkedReviewWithoutAnMRLink(t *testing.T) {
	windows := newFakeWindows(agentWindow("@115", "help/Alex-Pro-1448"))
	s := store(t)
	revisions := &fakeRevisions{result: worktree.Result{HeadSHA: linkedHeadSHA, BaseSHA: linkedBaseSHA}}
	req := reviewRequest("a")
	req.Refs = mrref.Refs{}

	rc, err := Apply(context.Background(), req, reviewDeps(windows, s, revisions))

	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rc.Status != StatusRejected || !strings.Contains(rc.Reason, "MR") {
		t.Fatalf("receipt = %+v", rc)
	}
	if len(revisions.mrs) != 0 {
		t.Errorf("nothing to read a revision from: %+v", revisions.mrs)
	}
}

func TestApplyRejectsReviewWhenReviewWorkflowsAreDisabled(t *testing.T) {
	windows := newFakeWindows(agentWindow("@115", "help/Alex-Pro-1448"))
	s := store(t)
	d := reviewDeps(windows, s, &fakeRevisions{result: worktree.Result{HeadSHA: linkedHeadSHA, BaseSHA: linkedBaseSHA}})
	d.ReviewEnabled = false

	rc, err := Apply(context.Background(), reviewRequest("a"), d)

	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rc.Status != StatusRejected || !strings.Contains(rc.Reason, "review_enabled") {
		t.Fatalf("receipt = %+v", rc)
	}
	if len(windows.renamed) != 0 {
		t.Errorf("disabled review must not rename a window: %v", windows.renamed)
	}
	if _, ok := s.Thread("1789480920.729569"); ok {
		t.Error("disabled review must not record a thread")
	}
}

func TestApplyLinksANonReviewWindowWithoutARevision(t *testing.T) {
	windows := newFakeWindows(agentWindow("@115", "help/Alex-Pro-1448"))
	s := store(t)
	revisions := &fakeRevisions{err: errors.New("provider must not be asked")}

	rc, err := Apply(context.Background(), request("a"), reviewDeps(windows, s, revisions))

	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rc.Status != StatusLinked {
		t.Fatalf("receipt = %+v", rc)
	}
	if len(revisions.mrs) != 0 {
		t.Errorf("a thread that is not a review has no revision to pin: %+v", revisions.mrs)
	}
}

func liveReviewThread() state.Thread {
	thread := state.Thread{
		WindowID: "@115", WindowName: "rev/service-a!899", SessionID: "session-live",
		Worktree: "/repo/.claude/worktrees/review-899", Kind: "review",
		HeadSHA: linkedHeadSHA, BaseSHA: linkedBaseSHA,
		Channel: "C0000000002", Subject: "review " + linkedMRURL,
		OpenedTS: "1789480920.729569", LastSeenTS: "1789481999.000100",
		Progress: "review_start", ProgressSHA: linkedHeadSHA,
		Members: []state.Member{{Agent: "codex", WindowID: "@116", SessionID: "session-member"}},
	}
	for range 3 {
		thread.OpenRound(linkedHeadSHA, linkedBaseSHA, state.RunOpenedByHandoff, now)
	}
	thread.Run.ReviewStarted = true
	thread.Run.Results = map[string]state.Result{"codex": {SHA: linkedHeadSHA, Blockers: 1, At: now}}
	return thread
}

func TestRelinkInTheSameWindowPreservesTheActiveReviewRound(t *testing.T) {
	windows := newFakeWindows(agentWindow("@115", "rev/service-a!899"))
	s := store(t)
	s.SetThread("1789480920.729569", liveReviewThread())
	revisions := &fakeRevisions{result: worktree.Result{HeadSHA: linkedHeadSHA, BaseSHA: linkedBaseSHA}}

	rc, err := Apply(context.Background(), reviewRequest("a"), reviewDeps(windows, s, revisions))

	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rc.Status != StatusLinked {
		t.Fatalf("receipt = %+v", rc)
	}
	thread, _ := s.Thread("1789480920.729569")
	if thread.Run == nil {
		t.Fatal("relink dropped the active round")
	}
	if thread.Run.Round != 3 || !thread.Run.ReviewStarted || len(thread.RunsHistory) != 2 {
		t.Errorf("run = %+v history = %+v", thread.Run, thread.RunsHistory)
	}
	if got, ok := thread.Run.Results["codex"]; !ok || got.Blockers != 1 {
		t.Errorf("results = %+v, want the collected result kept", thread.Run.Results)
	}
	if thread.Progress != "review_start" || thread.LastSeenTS != "1789481999.000100" || len(thread.Members) != 1 {
		t.Errorf("thread = %+v", thread)
	}
}

func TestATakeoverMovesOwnershipAndKeepsTheRound(t *testing.T) {
	windows := newFakeWindows(agentWindow("@115", "help/Alex-Pro-1448"), agentWindow("@116", "rev/service-a!899"))
	s := store(t)
	held := liveReviewThread()
	held.WindowID, held.WindowName = "@116", "rev/service-a!899"
	s.SetThread("1789480920.729569", held)
	revisions := &fakeRevisions{result: worktree.Result{HeadSHA: linkedHeadSHA, BaseSHA: linkedBaseSHA}}
	req := reviewRequest("a")
	req.Force = true

	rc, err := Apply(context.Background(), req, reviewDeps(windows, s, revisions))

	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rc.Status != StatusLinked || rc.WindowID != "@115" || rc.SessionID != "session-new" {
		t.Fatalf("receipt = %+v", rc)
	}
	thread, _ := s.Thread("1789480920.729569")
	if thread.WindowID != "@115" || thread.SessionID != "session-new" {
		t.Errorf("thread = %+v, want the new window owning it", thread)
	}
	if thread.Run == nil || thread.Run.Round != 3 || len(thread.Run.Results) != 1 {
		t.Errorf("run = %+v, want the round handed over intact", thread.Run)
	}
}

func TestARelinkAgainstAMovedHeadOpensANewRound(t *testing.T) {
	windows := newFakeWindows(agentWindow("@115", "rev/service-a!899"))
	s := store(t)
	s.SetThread("1789480920.729569", liveReviewThread())
	moved := "4a8259ce0aa1b2c3d4e5f60718293a4b5c6d7e8f"
	revisions := &fakeRevisions{result: worktree.Result{HeadSHA: moved, BaseSHA: linkedBaseSHA}}

	if _, err := Apply(context.Background(), reviewRequest("a"), reviewDeps(windows, s, revisions)); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	thread, _ := s.Thread("1789480920.729569")
	if thread.HeadSHA != moved {
		t.Errorf("thread = %+v, want the moved head pinned", thread)
	}
	if thread.Run == nil || thread.Run.Round != 4 || thread.Run.HeadSHA != moved || thread.Run.OpenedBy != state.RunOpenedByHeadChanged {
		t.Fatalf("run = %+v, want a new round for the moved head", thread.Run)
	}
	if len(thread.RunsHistory) != 3 || len(thread.RunsHistory[2].Results) != 1 {
		t.Errorf("history = %+v, want the finished round kept as evidence", thread.RunsHistory)
	}
}

func TestARelinkPinsARoundThatStartedWithoutARevision(t *testing.T) {
	windows := newFakeWindows(agentWindow("@115", "rev/service-a!899"))
	s := store(t)
	thread := liveReviewThread()
	thread.HeadSHA, thread.BaseSHA = "", ""
	thread.Run.HeadSHA, thread.Run.BaseSHA = "", ""
	s.SetThread("1789480920.729569", thread)
	revisions := &fakeRevisions{result: worktree.Result{HeadSHA: linkedHeadSHA, BaseSHA: linkedBaseSHA}}

	if _, err := Apply(context.Background(), reviewRequest("a"), reviewDeps(windows, s, revisions)); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got, _ := s.Thread("1789480920.729569")
	if got.Run == nil || got.Run.Round != 3 || len(got.RunsHistory) != 2 {
		t.Fatalf("run = %+v history = %+v, want the open round pinned in place", got.Run, got.RunsHistory)
	}
	if got.Run.HeadSHA != linkedHeadSHA || got.Run.BaseSHA != linkedBaseSHA {
		t.Errorf("run = %+v, want the revision pinned to the round that is already open", got.Run)
	}
}

func TestApplyRefusesAReviewOnAForgeTheConfigDoesNotAllow(t *testing.T) {
	windows := newFakeWindows(agentWindow("@115", "help/Alex-Pro-1448"))
	s := store(t)
	revisions := &fakeRevisions{result: worktree.Result{HeadSHA: linkedHeadSHA, BaseSHA: linkedBaseSHA}}
	d := reviewDeps(windows, s, revisions)
	d.Allows = func(mrref.MR) bool { return false }

	rc, err := Apply(context.Background(), reviewRequest("a"), d)

	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rc.Status != StatusRejected || !strings.Contains(rc.Reason, "gitlab.example.com") {
		t.Fatalf("receipt = %+v", rc)
	}
	if len(revisions.mrs) != 0 {
		t.Errorf("a forge outside the allowlist must not be contacted: %+v", revisions.mrs)
	}
	if _, ok := s.Thread("1789480920.729569"); ok {
		t.Error("a refused review must not become a thread")
	}
}

func TestLinkingAWindowReleasesTheThreadThatHeldIt(t *testing.T) {
	windows := newFakeWindows(agentWindow("@115", "rev/service-a!899"))
	s := store(t)
	previous := liveReviewThread()
	s.SetThread("1789400000.000100", previous)
	revisions := &fakeRevisions{result: worktree.Result{HeadSHA: linkedHeadSHA, BaseSHA: linkedBaseSHA}}

	rc, err := Apply(context.Background(), reviewRequest("a"), reviewDeps(windows, s, revisions))

	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rc.Status != StatusLinked {
		t.Fatalf("receipt = %+v", rc)
	}
	left, ok := s.Thread("1789400000.000100")
	if !ok {
		t.Fatal("the thread that held the window must stay as memory")
	}
	if left.WindowID != "" || left.SessionID != "" {
		t.Errorf("thread = %+v, a window can only belong to one thread", left)
	}
	if left.Run == nil || len(left.Run.Results) != 1 {
		t.Errorf("run = %+v, releasing a window must not erase what the round collected", left.Run)
	}
	linked, _ := s.Thread("1789480920.729569")
	if linked.WindowID != "@115" {
		t.Errorf("thread = %+v, the window must belong to the thread that linked it", linked)
	}
}
