package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kriso1337/handoffd/internal/config"
	"github.com/Kriso1337/handoffd/internal/deadletter"
	"github.com/Kriso1337/handoffd/internal/delivery"
	"github.com/Kriso1337/handoffd/internal/journal"
	"github.com/Kriso1337/handoffd/internal/label"
	"github.com/Kriso1337/handoffd/internal/launcher"
	"github.com/Kriso1337/handoffd/internal/linkq"
	"github.com/Kriso1337/handoffd/internal/mention"
	"github.com/Kriso1337/handoffd/internal/mrref"
	"github.com/Kriso1337/handoffd/internal/notifylog"
	"github.com/Kriso1337/handoffd/internal/outbox"
	"github.com/Kriso1337/handoffd/internal/prompt"
	"github.com/Kriso1337/handoffd/internal/promptfile"
	"github.com/Kriso1337/handoffd/internal/session"
	"github.com/Kriso1337/handoffd/internal/slackfetch"
	"github.com/Kriso1337/handoffd/internal/state"
	"github.com/Kriso1337/handoffd/internal/triage"
	"github.com/Kriso1337/handoffd/internal/waitq"
	"github.com/Kriso1337/handoffd/internal/worktree"
)

const (
	channel = "C100HOME"
	selfID  = "U100SELF"
	peerID  = "U200PEER"
	handoff = "[HANDOFF] <@U100SELF>, take this review [PRJ-8866](https://jira.example.com/browse/PRJ-8866) " +
		"https://gitlab.example.com/team-a/service-a/-/merge_requests/899\nHEAD: `08c9fc23a726a07692b7f9cf28d4ef6f57e4fec6`"
	worktreePath = "/repo/.claude/worktrees/review-899"
	oldSHA       = "3bd416e4346c8de2f21af16dc0dd8728c0d0f1d5"
	newSHA       = "4a8259ce0aa1b2c3d4e5f60718293a4b5c6d7e8f"
	baseSHA      = "da85e99d6e589924215fb821669ac10e1172ecdd"
	hookSettings = `{"hooks":{"Stop":[]}}`
)

type fakeFetcher struct {
	msg slackfetch.Message
	err error
}

func (f fakeFetcher) Fetch(context.Context, string, string, string) (slackfetch.Message, error) {
	return f.msg, f.err
}

type fakeThread struct {
	mu      sync.Mutex
	replies []slackfetch.Reply
	err     error
	calls   int
}

func (f *fakeThread) Replies(_ context.Context, _, _, _ string) ([]slackfetch.Reply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return append([]slackfetch.Reply(nil), f.replies...), f.err
}

func (f *fakeThread) holds(reply slackfetch.Reply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = append(f.replies, reply)
}

type flakyFetcher struct {
	failures int
	msg      slackfetch.Message
	calls    int
}

func (f *flakyFetcher) Fetch(context.Context, string, string, string) (slackfetch.Message, error) {
	f.calls++
	if f.calls <= f.failures {
		return slackfetch.Message{}, errors.New("slack hiccup")
	}
	return f.msg, nil
}

type fakeWorktrees struct {
	result      worktree.Result
	revision    worktree.Result
	revisionErr error
	err         error
	errOnCall   int
	synced      worktree.Result
	inspectErr  error
	calls       []mrref.MR
	busy        []map[string]bool
	inspected   []string
	refreshed   []string
	revisions   []mrref.MR
	inTransit   func()
}

func (f *fakeWorktrees) Prepare(_ context.Context, mr mrref.MR, busy map[string]bool) (worktree.Result, error) {
	f.calls = append(f.calls, mr)
	f.busy = append(f.busy, busy)
	if f.errOnCall > 0 && len(f.calls) != f.errOnCall {
		return f.result, nil
	}
	return f.result, f.err
}

func (f *fakeWorktrees) Revision(_ context.Context, mr mrref.MR) (worktree.Result, error) {
	f.revisions = append(f.revisions, mr)
	return f.revision, f.revisionErr
}

func (f *fakeWorktrees) Inspect(_ context.Context, path string, _ mrref.MR) (worktree.Result, error) {
	if f.inTransit != nil {
		during := f.inTransit
		f.inTransit = nil
		during()
	}
	f.inspected = append(f.inspected, path)
	return f.synced, f.inspectErr
}

func (f *fakeWorktrees) Refresh(_ context.Context, path string, _ mrref.MR) (worktree.Result, error) {
	f.refreshed = append(f.refreshed, path)
	return f.synced, nil
}

type fakeSessions struct {
	mu     sync.Mutex
	states map[string]session.State
}

func (f *fakeSessions) Read(id string) (session.Record, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.states[id]
	if !ok {
		return session.Record{}, false, nil
	}
	return session.Record{SessionID: id, State: st}, true, nil
}

func (f *fakeSessions) set(id string, st session.State) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[id] = st
}

type newWindowCall struct {
	Name       string
	CWD        string
	PromptFile string
}

type fakeWindows struct {
	mu        sync.Mutex
	existing  []launcher.Window
	created   []newWindowCall
	sent      []string
	killed    []string
	killErr   error
	captured  []string
	held      []string
	pane      string
	nextID    string
	nextIDs   []string
	renamed   map[string]string
	failNames map[string]error
	failAfter int
	failSend  error
	dieAfter  int
	finds     int
}

func (f *fakeWindows) HoldOnExit(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held = append(f.held, id)
	return nil
}

func (f *fakeWindows) KillWindow(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = append(f.killed, id)
	return f.killErr
}

func (f *fakeWindows) CapturePane(_ context.Context, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.captured = append(f.captured, id)
	if f.pane == "" {
		return "", errors.New("no pane")
	}
	return f.pane, nil
}

func (f *fakeWindows) Rename(_ context.Context, id, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.renamed == nil {
		f.renamed = map[string]string{}
	}
	f.renamed[id] = name
	for i, w := range f.existing {
		if w.ID == id {
			f.existing[i].Name = name
		}
	}
	return nil
}

func (f *fakeWindows) Windows(context.Context) ([]launcher.Window, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]launcher.Window(nil), f.existing...), nil
}

func (f *fakeWindows) Find(_ context.Context, id string) (launcher.Window, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finds++
	if f.dieAfter > 0 && f.finds > f.dieAfter {
		return launcher.Window{ID: id, Dead: true, Occupancy: launcher.OccupancyFree}, true, nil
	}
	for _, w := range f.existing {
		if w.ID == id {
			return w, true, nil
		}
	}
	if f.dieAfter > 0 {
		return launcher.Window{ID: id, Command: "2.1.267", Occupancy: launcher.OccupancyAgent}, true, nil
	}
	return launcher.Window{}, false, nil
}

func (f *fakeWindows) NewWindow(_ context.Context, name, cwd, promptFile string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failNames[name]; ok {
		return "", err
	}
	if f.failAfter > 0 && len(f.created) >= f.failAfter {
		return "", errors.New("tmux: no server running")
	}
	f.created = append(f.created, newWindowCall{Name: name, CWD: cwd, PromptFile: promptFile})
	if len(f.nextIDs) > 0 {
		id := f.nextIDs[0]
		f.nextIDs = f.nextIDs[1:]
		return id, nil
	}
	if f.nextID == "" {
		f.nextID = "@1"
	}
	return f.nextID, nil
}

func (f *fakeWindows) SendPrompt(_ context.Context, _, text string) error {
	if f.failSend != nil {
		return f.failSend
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, text)
	return nil
}

func (f *fakeWindows) sentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

type fakeVerdicts struct {
	mu      sync.Mutex
	verdict triage.Verdict
	err     error
	waited  []string
	block   bool
	taken   bool
	release chan struct{}
	peek    *triage.Verdict
}

func (f *fakeVerdicts) Path(ts string) string {
	return "/state/verdicts/verdict-" + ts + ".json"
}

func (f *fakeVerdicts) Wait(ctx context.Context, path string) (triage.Verdict, error) {
	f.mu.Lock()
	f.waited = append(f.waited, path)
	f.mu.Unlock()
	if f.block {
		select {
		case <-ctx.Done():
			if f.taken {
				return f.verdict, nil
			}
			return triage.Verdict{}, ctx.Err()
		case <-f.release:
			return f.verdict, f.err
		}
	}
	return f.verdict, f.err
}

func (f *fakeVerdicts) Peek(string) (triage.Verdict, bool) {
	if f.peek == nil {
		return triage.Verdict{}, false
	}
	return *f.peek, true
}

type fakeJournal struct {
	mu      sync.Mutex
	entries []journal.Entry
}

func (f *fakeJournal) Append(e journal.Entry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, e)
	return nil
}

func (f *fakeJournal) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.entries))
	for _, e := range f.entries {
		out = append(out, e.Action)
	}
	return out
}

func (f *fakeJournal) last() journal.Entry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.entries[len(f.entries)-1]
}

type fakeDeadLetters struct {
	mu      sync.Mutex
	records []deadletter.Record
	addErr  error
	adds    int
	onAdd   func()
}

func (f *fakeDeadLetters) added() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.adds
}

func (f *fakeDeadLetters) Add(n notifylog.Notification, reason string, at time.Time) error {
	f.mu.Lock()
	if f.addErr != nil {
		f.mu.Unlock()
		return f.addErr
	}
	kept := make([]deadletter.Record, 0, len(f.records)+1)
	for _, r := range f.records {
		if r.Notification.ID != n.ID {
			kept = append(kept, r)
		}
	}
	kept = append(kept, deadletter.Record{At: at, Reason: reason, Notification: n})
	f.records = kept
	f.adds++
	during := f.onAdd
	f.onAdd = nil
	f.mu.Unlock()
	if during != nil {
		during()
	}
	return nil
}

func (f *fakeDeadLetters) Peek() ([]deadletter.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]deadletter.Record(nil), f.records...), nil
}

func (f *fakeDeadLetters) Remove(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.records[:0]
	for _, r := range f.records {
		if r.Notification.ID != id {
			kept = append(kept, r)
		}
	}
	f.records = kept
	return nil
}

func (f *fakeDeadLetters) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.records)
}

type fakeOutbox struct {
	mu          sync.Mutex
	records     []outbox.Record
	malformed   []outbox.Malformed
	receipts    map[string]outbox.Receipt
	quarantined []string
}

func (f *fakeOutbox) Pending() ([]outbox.Record, []outbox.Malformed, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []outbox.Record
	for _, r := range f.records {
		if _, done := f.receipts[r.ID]; !done {
			out = append(out, r)
		}
	}
	return out, f.malformed, nil
}

func (f *fakeOutbox) WriteReceipt(rc outbox.Receipt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.receipts[rc.ID] = rc
	return nil
}

func (f *fakeOutbox) retry() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, rc := range f.receipts {
		if rc.Status == outbox.StatusPending {
			delete(f.receipts, id)
		}
	}
}

func (f *fakeOutbox) Quarantine(path, reason string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quarantined = append(f.quarantined, path+" "+reason)
	f.malformed = nil
	return nil
}

func (f *fakeOutbox) add(r outbox.Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, r)
}

func (f *fakeOutbox) receipt(id string) outbox.Receipt {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.receipts[id]
}

type slackPost struct {
	Channel  string
	ThreadTS string
	Text     string
}

type fakePoster struct {
	mu        sync.Mutex
	posts     []slackPost
	err       error
	fails     int
	attempts  int
	inTransit func()
}

func (f *fakePoster) Post(_ context.Context, channel, threadTS, text string) (string, error) {
	if f.inTransit != nil {
		during := f.inTransit
		f.inTransit = nil
		during()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.fails > 0 {
		f.fails--
		return "", fmt.Errorf("%w: slack said invalid_thread_ts", delivery.ErrNotAccepted)
	}
	if f.err != nil {
		return "", f.err
	}
	f.posts = append(f.posts, slackPost{Channel: channel, ThreadTS: threadTS, Text: text})
	return fmt.Sprintf("1789060000.%06d", len(f.posts)), nil
}

func (f *fakePoster) tries() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

func (f *fakePoster) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.posts))
	for _, p := range f.posts {
		out = append(out, p.Text)
	}
	return out
}

type fakeNotes struct {
	mu    sync.Mutex
	notes []string
	err   error
}

func (f *fakeNotes) Note(_ context.Context, mr mrref.MR, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.notes = append(f.notes, mr.Slug()+": "+text)
	return nil
}

type recordingPrompts struct {
	mu       sync.Mutex
	payloads []promptfile.Payload
}

func (r *recordingPrompts) Write(p promptfile.Payload) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.payloads = append(r.payloads, p)
	return "/state/prompts/prompt-1.json", nil
}

type harness struct {
	pipe      *Pipeline
	cfg       config.Config
	fetcher   fakeFetcher
	worktrees *fakeWorktrees
	windows   *fakeWindows
	prompts   *recordingPrompts
	verdicts  *fakeVerdicts
	sessions  *fakeSessions
	journal   *fakeJournal
	dead      *fakeDeadLetters
	inbox     *fakeDeadLetters
	outbox    *fakeOutbox
	links     linkq.Dir
	waiting   waitq.Dir
	poster    *fakePoster
	thread    *fakeThread
	notes     *fakeNotes
	store     *state.Store
	ids       int
}

func newHarness(t *testing.T, msg slackfetch.Message) *harness {
	t.Helper()
	cfg := config.Defaults()
	cfg.ReviewEnabled = true
	cfg.Channel = channel
	cfg.ChannelName = "agent-handoffs"
	cfg.SelfUserID = selfID
	cfg.SlackWorkspace = "acme"
	cfg.MentionWords = []string{"atlas"}
	cfg.TriageWords = []string{"atlas", "alex", "sam"}
	cfg.MacroPhrases = []string{"atlas, help", "atlas help", "atlas, help", "atlas help"}
	cfg.StopPhrases = []string{"atlas, stop", "atlas stop", "atlas, stop", "atlas stop"}
	cfg.Namesake = "Alex Twin (U300TWIN)"
	cfg.PeerAgents = []string{"Ada", "Orbit"}
	cfg.Persona = prompt.Persona{
		Owner: "Alex", OwnerGenitive: "Alex", OwnerDative: "Alex", OwnerFull: "Alex Smith", OwnerFullGenitive: "Alex Smith",
		Agent: "Atlas", Approvers: "Alex or Sam", MainModel: "Opus", TriageModel: "Haiku", ThreadTool: "mcp__slack__slack_thread",
	}
	cfg.FetchHelper = []string{"python3", "helper.py"}
	cfg.ProjectsDir = "/projects"
	cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
	cfg.OtherLimit = 2
	cfg.DeliverTimeoutSec = 1

	store, err := state.Open(cfg.StatePath, cfg.SeenCapacity)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	render, err := prompt.New(cfg.Persona, "")
	if err != nil {
		t.Fatalf("prompt.New: %v", err)
	}

	h := &harness{
		cfg:       cfg,
		fetcher:   fakeFetcher{msg: msg},
		worktrees: &fakeWorktrees{result: worktree.Result{Path: worktreePath, SHA: oldSHA, HeadSHA: oldSHA}},
		windows:   &fakeWindows{renamed: map[string]string{}},
		prompts:   &recordingPrompts{},
		verdicts:  &fakeVerdicts{},
		sessions:  &fakeSessions{states: map[string]session.State{}},
		journal:   &fakeJournal{},
		dead:      &fakeDeadLetters{},
		inbox:     &fakeDeadLetters{},
		outbox:    &fakeOutbox{receipts: map[string]outbox.Receipt{}},
		poster:    &fakePoster{},
		thread:    &fakeThread{},
		notes:     &fakeNotes{},
		store:     store,
		links:     linkq.New(filepath.Join(filepath.Dir(cfg.StatePath), "links")),
		waiting:   waitq.New(filepath.Join(filepath.Dir(cfg.StatePath), "waiting")),
	}
	h.pipe = h.build(cfg, render)
	return h
}

func (h *harness) build(cfg config.Config, render prompt.Renderer) *Pipeline {
	return New(cfg, Deps{
		Fetcher:      h.fetcher,
		Classifier:   mention.New(selfID, channel, cfg.Rules()),
		Worktrees:    h.worktrees,
		Windows:      h.windows,
		Prompts:      h.prompts,
		Verdicts:     h.verdicts,
		Sessions:     h.sessions,
		Journal:      h.journal,
		Inbox:        h.inbox,
		DeadLetters:  h.dead,
		Render:       render,
		Store:        h.store,
		Now:          func() time.Time { return time.Date(2026, 9, 10, 13, 38, 0, 0, time.UTC) },
		NewID:        func() string { h.ids++; return fmt.Sprintf("sess-%d", h.ids) },
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		HookSettings: hookSettings,
		SelfBin:      "handoffd",
		PollInterval: 2 * time.Millisecond,
		Outbox:       h.outbox,
		Links:        h.links,
		Waiting:      h.waiting,
		Poster:       h.poster,
		Readback:     h.thread,
		Notes:        h.notes,
	})
}

func (h *harness) handle(t *testing.T, n notifylog.Notification) {
	t.Helper()
	if err := h.pipe.Handle(context.Background(), n); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	h.pipe.Wait()
}

func (h *harness) liveWindows(windows ...launcher.Window) {
	h.windows.mu.Lock()
	defer h.windows.mu.Unlock()
	h.windows.existing = windows
}

func (h *harness) liveWindow(id, name string) {
	h.windows.mu.Lock()
	defer h.windows.mu.Unlock()
	h.windows.existing = []launcher.Window{{ID: id, Name: name, Command: "2.1.267", Occupancy: launcher.OccupancyAgent}}
}

func blockStateWrites(t *testing.T, statePath string) func() {
	t.Helper()
	tmp := statePath + ".tmp"
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatalf("block state writes: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(tmp) })
	return func() {
		t.Helper()
		if err := os.Remove(tmp); err != nil {
			t.Fatalf("release state writes: %v", err)
		}
	}
}

func notification(ts, threadTS string) notifylog.Notification {
	return notifylog.Notification{
		ID:       "T100_" + ts,
		TeamID:   "T100TEAM",
		Channel:  channel,
		MsgTS:    ts,
		ThreadTS: threadTS,
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestReviewHandoffOpensWindowInWorktree(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, AuthorName: "teammate-b", Text: handoff})

	h.handle(t, notification("1789049440.051109", ""))

	if len(h.windows.created) != 1 {
		t.Fatalf("windows created = %v", h.windows.created)
	}
	got := h.windows.created[0]
	if got.Name != "rev/PRJ-8866" {
		t.Errorf("window name = %q", got.Name)
	}
	if got.CWD != worktreePath {
		t.Errorf("cwd = %q, want the worktree", got.CWD)
	}
	if len(h.worktrees.calls) != 1 || h.worktrees.calls[0].IID != 899 {
		t.Errorf("worktree calls = %+v", h.worktrees.calls)
	}
	if !strings.Contains(h.prompts.payloads[0].Prompt, "review unconditionally") {
		t.Error("prompt must carry the unconditional-review instruction")
	}
}

func TestReviewLookingMessageUsesGenericResponseFlowWhenReviewIsDisabled(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	h.cfg.ReviewEnabled = false
	render, _ := prompt.New(h.cfg.Persona, "")
	h.pipe = h.build(h.cfg, render)

	h.handle(t, notification("1789049440.051109", ""))

	if len(h.windows.created) != 1 {
		t.Fatalf("windows created = %v", h.windows.created)
	}
	if len(h.worktrees.calls) != 0 {
		t.Errorf("disabled review must not prepare a worktree: %+v", h.worktrees.calls)
	}
	payload := h.prompts.payloads[0]
	if !strings.Contains(payload.Prompt, "help Alex respond promptly") || strings.Contains(payload.Prompt, "review unconditionally") {
		t.Errorf("disabled review must use the generic response prompt:\n%s", payload.Prompt)
	}
	if len(payload.DeniedTools) != 0 {
		t.Errorf("generic response window received reviewer restrictions: %v", payload.DeniedTools)
	}
	thread, ok := h.store.Thread("1789049440.051109")
	if !ok || thread.Kind != mention.KindOther.String() {
		t.Errorf("thread = %+v, want an ordinary response thread", thread)
	}
}

func TestReviewWindowPinsSessionHooksAndToolPolicy(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	h.cfg.MainModel = "claude-opus-5"
	render, _ := prompt.New(h.cfg.Persona, "")
	h.pipe = h.build(h.cfg, render)

	h.handle(t, notification("1789049440.051109", ""))

	payload := h.prompts.payloads[0]
	if payload.SessionID != "sess-1" {
		t.Errorf("session id = %q", payload.SessionID)
	}
	if !strings.HasPrefix(payload.Settings, `{"hooks":{"Stop":[]}`) {
		t.Errorf("settings = %q, hook settings must reach the launcher", payload.Settings)
	}
	if payload.Model != "claude-opus-5" {
		t.Errorf("model = %q, main_model must pin the review window", payload.Model)
	}
	if len(payload.DeniedTools) == 0 || !strings.Contains(strings.Join(payload.DeniedTools, " "), "git push") {
		t.Errorf("denied tools = %v, reviewer must not be able to push", payload.DeniedTools)
	}
	thread, _ := h.store.Thread("1789049440.051109")
	if thread.SessionID != "sess-1" || thread.HeadSHA != oldSHA || thread.MR == nil || thread.MR.IID != 899 {
		t.Errorf("thread = %+v", thread)
	}
}

func TestReviewWindowPinsBaseFromGitLab(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	h.worktrees.result = worktree.Result{Path: worktreePath, SHA: oldSHA, HeadSHA: oldSHA, BaseSHA: baseSHA, TargetBranch: "master"}

	h.handle(t, notification("1789049440.051109", ""))

	if !strings.Contains(h.prompts.payloads[0].Prompt, "base "+baseSHA+" = master") {
		t.Errorf("prompt must name the base:\n%s", h.prompts.payloads[0].Prompt)
	}
	thread, _ := h.store.Thread("1789049440.051109")
	if thread.BaseSHA != baseSHA {
		t.Errorf("thread base = %q", thread.BaseSHA)
	}
}

func TestTriageModelKeyOverridesLegacyHaikuKey(t *testing.T) {
	h := newHarness(t, triageMessage())
	h.cfg.TriageModel = "gpt-5.6-mini"
	h.verdicts.verdict = triage.Verdict{React: false, Reason: "no"}
	render, _ := prompt.New(h.cfg.Persona, "")
	h.pipe = h.build(h.cfg, render)

	h.handle(t, foreignNotification("1789054475.628329", ""))

	if got := h.prompts.payloads[0].Model; got != "gpt-5.6-mini" {
		t.Errorf("triage model = %q", got)
	}
}

func TestHelpWindowKeepsToolsOpen(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: "U300TWIN", Text: "Atlas, help — check logs"})

	h.handle(t, notification("1789049440.051109", ""))

	if got := h.prompts.payloads[0].DeniedTools; len(got) != 0 {
		t.Errorf("help window must not carry review denials: %v", got)
	}
}

func TestOwnMessageOpensNothing(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: selfID, Text: ":robot_face: Atlas: " + handoff})

	h.handle(t, notification("1789049440.051109", ""))

	if len(h.windows.created) != 0 {
		t.Errorf("own message opened a window: %v", h.windows.created)
	}
}

func TestForeignChannelChatterOpensNothing(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: "great", ChannelLabel: "#team-a-dev"})
	n := notification("1789049440.051109", "")
	n.Channel = "C300TEAM"

	h.handle(t, n)

	if len(h.windows.created) != 0 {
		t.Errorf("chatter opened a window: %v", h.windows.created)
	}
}

func TestDuplicateNotificationIsHandledOnce(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	n := notification("1789049440.051109", "")

	h.handle(t, n)
	h.handle(t, n)

	if len(h.windows.created) != 1 {
		t.Errorf("windows created = %d, want 1", len(h.windows.created))
	}
}

func TestTransientFetchFailureIsRetried(t *testing.T) {
	h := newHarness(t, slackfetch.Message{})
	fetcher := &flakyFetcher{failures: 2, msg: slackfetch.Message{AuthorID: peerID, Text: handoff}}
	h.pipe.fetcher = fetcher

	h.handle(t, notification("1789049440.051109", ""))

	if fetcher.calls != 3 {
		t.Errorf("fetch calls = %d, want two retries", fetcher.calls)
	}
	if len(h.windows.created) != 1 {
		t.Errorf("window must open once the fetch succeeds: %v", h.windows.created)
	}
}

func TestFetchFailureIsReported(t *testing.T) {
	h := newHarness(t, slackfetch.Message{})
	h.pipe.fetcher = fakeFetcher{err: errors.New("slack unavailable")}

	err := h.pipe.Handle(context.Background(), notification("1789049440.051109", ""))

	if err == nil {
		t.Fatal("fetch failure must be reported, not swallowed")
	}
	if len(h.windows.created) != 0 {
		t.Errorf("no window may open without the message text: %v", h.windows.created)
	}
}

func TestContinuationGoesToLiveWindow(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	root := "1789047527.174689"
	h.handle(t, notification(root, ""))
	h.liveWindow("@1", "rev/PRJ-8866")
	h.worktrees.synced = worktree.Result{Path: worktreePath, SHA: oldSHA, HeadSHA: oldSHA}
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{
		AuthorID: peerID,
		Text:     "[HEAD CHANGED] 08c9fc23 → 4a8259ce0aa1b2c3d4e5f6a7b8c9d0e1f2a3b4c5",
	}}

	h.handle(t, notification("1789049999.000100", root))

	if len(h.windows.created) != 1 {
		t.Errorf("continuation opened a second window: %v", h.windows.created)
	}
	if len(h.windows.sent) != 1 {
		t.Fatalf("prompts sent = %v", h.windows.sent)
	}
	if strings.Contains(h.windows.sent[0], "\n") {
		t.Error("continuation prompt must be single-line")
	}
	if !strings.Contains(h.windows.sent[0], worktreePath) {
		t.Error("continuation must point at the existing worktree")
	}
}

func TestContinuationRefreshesWorktreeWhileSessionIsIdle(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	root := "1789047527.174689"
	h.handle(t, notification(root, ""))
	h.liveWindow("@1", "rev/PRJ-8866")
	h.sessions.set("sess-1", session.Idle)
	h.worktrees.synced = worktree.Result{Path: worktreePath, SHA: newSHA, HeadSHA: newSHA, Note: "worktree moved 3bd416e4346c → 4a8259ce0aa1"}
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: peerID, Text: "[HEAD CHANGED] → " + newSHA}}

	h.handle(t, notification("1789049999.000100", root))

	if len(h.worktrees.refreshed) != 1 || len(h.worktrees.inspected) != 0 {
		t.Errorf("idle session must get its tree refreshed: refreshed=%v inspected=%v", h.worktrees.refreshed, h.worktrees.inspected)
	}
	if !strings.Contains(h.windows.sent[0], "matches the MR head") {
		t.Errorf("continuation must confirm the tree is current: %s", h.windows.sent[0])
	}
	thread, _ := h.store.Thread(root)
	if thread.HeadSHA != newSHA {
		t.Errorf("thread head = %q, want %q", thread.HeadSHA, newSHA)
	}
}

func TestContinuationOnlyInspectsWorktreeWhileSessionWorks(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	root := "1789047527.174689"
	h.handle(t, notification(root, ""))
	h.liveWindow("@1", "rev/PRJ-8866")
	h.sessions.set("sess-1", session.Working)
	h.worktrees.synced = worktree.Result{Path: worktreePath, SHA: oldSHA, HeadSHA: newSHA}
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: peerID, Text: "[HEAD CHANGED] → " + newSHA}}

	h.handle(t, notification("1789049999.000100", root))

	if len(h.worktrees.refreshed) != 0 || len(h.worktrees.inspected) != 1 {
		t.Errorf("working session must not have its tree moved: refreshed=%v inspected=%v", h.worktrees.refreshed, h.worktrees.inspected)
	}
	if !strings.Contains(h.windows.sent[0], "git checkout --detach "+newSHA) {
		t.Errorf("continuation must hand the agent the exact head: %s", h.windows.sent[0])
	}
}

func TestContinuationWaitsWhileSessionIsBlocked(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	root := "1789047527.174689"
	h.handle(t, notification(root, ""))
	h.liveWindow("@1", "rev/PRJ-8866")
	h.sessions.set("sess-1", session.Blocked)
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: peerID, Text: "replied to the findings"}}

	h.handle(t, notification("1789049999.000100", root))

	if h.windows.sentCount() != 0 {
		t.Fatalf("prompt must not be typed into a permission dialog: %v", h.windows.sent)
	}
	h.sessions.set("sess-1", session.Idle)
	waitFor(t, "queued prompt", func() bool { return h.windows.sentCount() == 1 })
}

func TestQueuedPromptsKeepOrderBehindBlockedSession(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	root := "1789047527.174689"
	h.handle(t, notification(root, ""))
	h.liveWindow("@1", "rev/PRJ-8866")
	h.sessions.set("sess-1", session.Blocked)

	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: peerID, Text: "first"}}
	h.handle(t, notification("1789049999.000100", root))
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: peerID, Text: "second"}}
	h.handle(t, notification("1789049999.000200", root))
	h.sessions.set("sess-1", session.Idle)

	waitFor(t, "both prompts", func() bool { return h.windows.sentCount() == 2 })
	if !strings.Contains(h.windows.sent[0], "first") || !strings.Contains(h.windows.sent[1], "second") {
		t.Errorf("order lost: %v", h.windows.sent)
	}
}

func TestAPromptTheDialogNeverFreedIsKeptForAHuman(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	alerts := &fakeAlerts{}
	h.pipe.alerts = alerts
	root := "1789047527.174689"
	h.handle(t, notification(root, ""))
	h.liveWindow("@1", "rev/PRJ-8866")
	h.sessions.set("sess-1", session.Blocked)
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: peerID, Text: "replied"}}

	h.handle(t, notification("1789049999.000100", root))

	waitFor(t, "the queue to give up", func() bool {
		waiting, err := h.waiting.Peek("@1")
		return err == nil && len(waiting) == 0
	})
	if h.windows.sentCount() != 0 {
		t.Errorf("nothing may be sent into a dialog that never closed: %v", h.windows.sent)
	}
	kept, err := filepath.Glob(filepath.Join(h.waiting.Path(), "undelivered", "*.json"))
	if err != nil || len(kept) != 1 {
		t.Fatalf("kept = %v, err = %v, a prompt nobody could deliver must not just vanish", kept, err)
	}
	raw, err := os.ReadFile(kept[0])
	if err != nil || !strings.Contains(string(raw), "replied") {
		t.Errorf("kept prompt = %q, err = %v", raw, err)
	}
	if alerts.count() == 0 {
		t.Error("a prompt left undelivered must reach a human")
	}
}

func TestABlockedPromptIsOnDiskBeforeItIsAcknowledged(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	root := "1789047527.174689"
	h.handle(t, notification(root, ""))
	h.liveWindow("@1", "rev/PRJ-8866")
	h.sessions.set("sess-1", session.Blocked)
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: peerID, Text: "survive the restart"}}

	h.handle(t, notification("1789049999.000100", root))

	waiting, err := h.waiting.Peek("@1")
	if err != nil || len(waiting) != 1 || !strings.Contains(waiting[0].Text, "survive the restart") {
		t.Fatalf("waiting = %+v, err = %v, a queued prompt must be on disk before it is acknowledged", waiting, err)
	}
	if waiting[0].SessionID != "sess-1" || waiting[0].WindowID != "@1" {
		t.Errorf("waiting = %+v, want the window and session it waits for", waiting[0])
	}
}

func TestPromptsLeftByADeadProcessAreDeliveredOnTheNextStart(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	root := "1789047527.174689"
	h.handle(t, notification(root, ""))
	h.liveWindow("@1", "rev/PRJ-8866")
	h.sessions.set("sess-1", session.Idle)
	left := waitq.Prompt{
		ID: "left-1", At: h.pipe.now().UTC(), WindowID: "@1",
		SessionID: "sess-1", ThreadTS: root, Text: "delivery process died before sending this",
	}
	if err := h.waiting.Append(left); err != nil {
		t.Fatal(err)
	}

	h.pipe.ResumeWaiting(context.Background())

	waitFor(t, "the prompt left behind", func() bool { return h.windows.sentCount() == 1 })
	if !strings.Contains(h.windows.sent[0], "delivery process died") {
		t.Errorf("sent = %v", h.windows.sent)
	}
	if rest, _ := h.waiting.Peek("@1"); len(rest) != 0 {
		t.Errorf("waiting = %+v, a delivered prompt must leave the queue", rest)
	}
}

func TestPromptsLeftForAWindowThatIsGoneAreKeptForAHuman(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	alerts := &fakeAlerts{}
	h.pipe.alerts = alerts
	if err := h.waiting.Append(waitq.Prompt{
		ID: "left-2", At: h.pipe.now().UTC(), WindowID: "@404",
		SessionID: "sess-9", ThreadTS: "1789047527.174689", Text: "window has been gone for a while",
	}); err != nil {
		t.Fatal(err)
	}

	h.pipe.ResumeWaiting(context.Background())

	if rest, _ := h.waiting.Peek("@404"); len(rest) != 0 {
		t.Errorf("waiting = %+v", rest)
	}
	kept, err := filepath.Glob(filepath.Join(h.waiting.Path(), "undelivered", "*.json"))
	if err != nil || len(kept) != 1 {
		t.Fatalf("kept = %v, err = %v", kept, err)
	}
	if alerts.count() == 0 {
		t.Error("prompts for a window that is gone must reach a human")
	}
}

func TestAPromptThatCannotBeQueuedDurablyIsNotAcknowledged(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	root := "1789047527.174689"
	h.handle(t, notification(root, ""))
	h.liveWindow("@1", "rev/PRJ-8866")
	h.sessions.set("sess-1", session.Blocked)
	if err := os.WriteFile(h.waiting.Path(), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: peerID, Text: "no destination available"}}

	err := h.pipe.Handle(context.Background(), notification("1789049999.000100", root))
	h.pipe.Wait()

	if err == nil {
		t.Fatal("a prompt that could not be queued durably must not be reported as handled")
	}
	if h.inbox.count()+h.dead.count() == 0 {
		t.Error("the notification must stay recoverable, in the inbox or as a dead letter")
	}
}

func TestContinuationOpensNewWindowWhenOldOneIsGone(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	root := "1789047527.174689"
	h.handle(t, notification(root, ""))
	h.windows.existing = nil
	h.windows.nextID = "@2"

	h.handle(t, notification("1789049999.000100", root))

	if len(h.windows.created) != 2 {
		t.Errorf("windows created = %d, want a fresh window", len(h.windows.created))
	}
}

func TestAcknowledgementInRememberedThreadIsSkippedButMemoryStays(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	root := "1789047527.174689"
	h.handle(t, notification(root, ""))
	h.windows.existing = nil
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: peerID, Text: "okay, thanks"}}

	h.handle(t, notification("1789049999.000100", root))

	if len(h.windows.created) != 1 {
		t.Errorf("acknowledgement opened a window: %v", h.windows.created)
	}
	th, ok := h.store.Thread(root)
	if !ok || !th.WindowGone() || th.Worktree != worktreePath || th.Kind != "review" {
		t.Errorf("thread memory = %+v ok=%v, want the record kept without a window", th, ok)
	}
	if e := h.journal.last(); e.Action != journal.ActionSkipped || !strings.Contains(e.Reason, "acknowledgement") {
		t.Errorf("entry = %+v", e)
	}
}

func TestReplyInRememberedThreadGoesToTriageAndResumesInSameWorktree(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	root := "1789047527.174689"
	h.handle(t, notification(root, ""))
	h.windows.existing = nil
	h.windows.nextID = "@7"
	h.verdicts.verdict = triage.Verdict{React: true, Reason: "answer to the agent question", Task: "Continue: ticket PRJ-8874 was created"}
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: peerID, AuthorName: "teammate", Text: "well https://jira.example.com/browse/PRJ-8874"}}

	h.handle(t, notification("1789049999.000100", root))

	if len(h.verdicts.waited) != 1 {
		t.Fatalf("reply in a remembered thread must be triaged, waited=%v", h.verdicts.waited)
	}
	if len(h.windows.created) != 3 {
		t.Fatalf("windows = %v, want review, triage and the resumed window", h.windows.created)
	}
	resumed := h.windows.created[2]
	if resumed.CWD != worktreePath {
		t.Errorf("resumed window cwd = %q, want the same worktree", resumed.CWD)
	}
	prompt := h.prompts.payloads[len(h.prompts.payloads)-1].Prompt
	if !strings.Contains(prompt, "The previous window of this thread is closed") || !strings.Contains(prompt, "PRJ-8874") {
		t.Errorf("resumed prompt = %s", prompt)
	}
	th, _ := h.store.Thread(root)
	if th.WindowID != "@7" || th.Kind != "review" || th.Worktree != worktreePath {
		t.Errorf("thread = %+v, want the review thread resumed in place", th)
	}
	if got := h.prompts.payloads[len(h.prompts.payloads)-1].DeniedTools; len(got) == 0 {
		t.Error("resumed review window must keep the reviewer tool policy")
	}
}

func TestChatterInUnknownHomeThreadIsIgnored(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: "well https://jira.example.com/browse/PRJ-8874"})

	h.handle(t, notification("1789049999.000100", "1789040000.000001"))

	if len(h.windows.created) != 0 || len(h.verdicts.waited) != 0 {
		t.Errorf("unknown thread chatter must not open anything: %v %v", h.windows.created, h.verdicts.waited)
	}
	if e := h.journal.last(); e.Action != journal.ActionIgnored {
		t.Errorf("entry = %+v", e)
	}
}

func TestSecondThreadOnSameMergeRequestGetsItsOwnWorktree(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	h.handle(t, notification("1789047527.174689", ""))
	h.liveWindow("@1", "rev/PRJ-8866")
	h.windows.nextID = "@2"

	h.handle(t, notification("1789049440.051109", ""))

	if len(h.worktrees.busy) != 2 {
		t.Fatalf("prepare calls = %d", len(h.worktrees.busy))
	}
	if h.worktrees.busy[0][worktreePath] {
		t.Error("first review must see a free worktree")
	}
	if !h.worktrees.busy[1][worktreePath] {
		t.Error("second review must be told the worktree is held by a live session")
	}
}

func TestDeadWindowDoesNotHoldWorktree(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	h.handle(t, notification("1789047527.174689", ""))
	h.windows.existing = []launcher.Window{{ID: "@1", Name: "rev/PRJ-8866", Command: "zsh", Occupancy: launcher.OccupancyFree}}

	h.handle(t, notification("1789049440.051109", ""))

	if h.worktrees.busy[1][worktreePath] {
		t.Error("worktree of a window that fell back to the shell is free")
	}
}

func TestMergeRequestFromUnknownHostIsNotCheckedOut(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	h.cfg.GitLabHosts = []string{"gitlab.trusted.com"}
	render, _ := prompt.New(h.cfg.Persona, "")
	h.pipe = h.build(h.cfg, render)

	err := h.pipe.Handle(context.Background(), notification("1789049440.051109", ""))

	if len(h.worktrees.calls) != 0 {
		t.Errorf("foreign host must not reach git: %+v", h.worktrees.calls)
	}
	if err == nil || len(h.windows.created) != 0 {
		t.Fatalf("review from an unlisted host must be refused: err=%v windows=%v", err, h.windows.created)
	}
	if h.dead.count() != 1 || !strings.Contains(h.dead.records[0].Reason, "gitlab.example.com is not in gitlab_hosts") {
		t.Errorf("dead letter must explain the allowlist miss: %+v", h.dead.records)
	}
}

func TestSweepDropsThreadsWithoutLiveWindowsOnceTheyAreOld(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	h.handle(t, notification("1789047527.174689", ""))
	h.windows.nextID = "@2"
	h.handle(t, notification("1789049440.051109", ""))
	h.windows.nextID = "@3"
	h.handle(t, notification("1789049440.051200", ""))
	h.liveWindow("@2", "rev/PRJ-8866-2")
	if err := h.store.UpdateThread("1789047527.174689", func(th *state.Thread) error {
		th.UpdatedAt = h.pipe.now().Add(-60 * 24 * time.Hour)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := h.pipe.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if _, ok := h.store.Thread("1789047527.174689"); ok {
		t.Error("thread of a window closed long ago must be gone")
	}
	if _, ok := h.store.Thread("1789049440.051200"); !ok {
		t.Error("a thread whose window just stopped holding an agent keeps its round for its own ttl")
	}
	if _, ok := h.store.Thread("1789049440.051109"); !ok {
		t.Error("thread of the live window must stay")
	}
}

func TestMentionWithoutReviewSkipsWorktree(t *testing.T) {
	h := newHarness(t, slackfetch.Message{
		AuthorID: "U300TWIN",
		Text:     "Peer, Atlas, team, catch up",
	})

	h.handle(t, notification("1789049440.051109", ""))

	if len(h.worktrees.calls) != 0 {
		t.Errorf("non-review message prepared a worktree: %+v", h.worktrees.calls)
	}
	if got := h.windows.created[0]; got.CWD != "/projects" {
		t.Errorf("cwd = %q, want projects dir", got.CWD)
	}
	if !strings.HasPrefix(h.windows.created[0].Name, "sky/") {
		t.Errorf("window name = %q", h.windows.created[0].Name)
	}
}

func TestRateLimitAppliesToNonReviewOnly(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: "U300TWIN", Text: "Atlas, check"})

	for _, ts := range []string{"1789049440.000001", "1789049440.000002", "1789049440.000003"} {
		h.handle(t, notification(ts, ""))
	}

	if len(h.windows.created) != 2 {
		t.Errorf("windows created = %d, want the limit of 2", len(h.windows.created))
	}

	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: peerID, Text: handoff}}
	h.handle(t, notification("1789049440.000004", ""))

	if len(h.windows.created) != 3 {
		t.Error("review must pass the rate limit")
	}
}

func TestReviewWithoutExactWorktreeIsRefusedAndBuried(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	h.worktrees.result = worktree.Result{Note: "repository service-a not found locally (/projects/service-a)"}
	h.worktrees.err = errors.New("stat: no such file")
	n := notification("1789049440.051109", "")

	err := h.pipe.Handle(context.Background(), n)

	if err == nil || !strings.Contains(err.Error(), "review refused without an exact worktree") {
		t.Fatalf("Handle = %v, want a fail-closed refusal", err)
	}
	if len(h.windows.created) != 0 {
		t.Errorf("no review window may open from the shared projects dir: %v", h.windows.created)
	}
	if h.dead.count() != 1 || !strings.Contains(h.dead.records[0].Reason, "service-a!899") {
		t.Errorf("dead letters = %+v", h.dead.records)
	}
	if !h.store.Seen(n.ID) {
		t.Error("a buried notification is settled")
	}
	if e := h.journal.last(); e.Action != journal.ActionError {
		t.Errorf("entry = %+v", e)
	}
}

func TestHelpWithBrokenWorktreeStillOpensWindow(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: "Atlas, help: check https://gitlab.example.com/team-a/service-a/-/merge_requests/899"})
	h.worktrees.result = worktree.Result{Note: "repository service-a not found locally (/projects/service-a)"}
	h.worktrees.err = errors.New("stat: no such file")

	h.handle(t, notification("1789049440.051109", ""))

	if len(h.windows.created) != 1 || h.windows.created[0].CWD != "/projects" {
		t.Errorf("help must stay fail-open: %v", h.windows.created)
	}
}

func TestStaleDirtyWorktreeIsFlaggedInPrompt(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	h.worktrees.result = worktree.Result{
		Path: worktreePath, SHA: oldSHA, HeadSHA: newSHA, Dirty: true,
		Note: "dirty worktree (2 files with local changes), left at 3bd416e4346c, head MR — 4a8259ce0aa1",
	}

	h.handle(t, notification("1789049440.051109", ""))

	got := h.prompts.payloads[0].Prompt
	if !strings.Contains(got, "MR head is already "+newSHA+", the tree was NOT moved to it") || !strings.Contains(got, "dirty") {
		t.Errorf("prompt must warn about the stale dirty tree:\n%s", got)
	}
}

func TestSecondMergeRequestBecomesAddDir(t *testing.T) {
	text := handoff + "\nand https://gitlab.example.com/team-b/service-b/-/merge_requests/2246"
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: text})

	h.handle(t, notification("1789049440.051109", ""))

	if len(h.worktrees.calls) != 2 {
		t.Fatalf("worktree calls = %+v", h.worktrees.calls)
	}
	if len(h.prompts.payloads[0].AddDirs) != 1 {
		t.Errorf("add_dirs = %v, want the second worktree", h.prompts.payloads[0].AddDirs)
	}
}

func TestWindowNameAvoidsCollision(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	h.windows.existing = []launcher.Window{{ID: "@9", Name: "rev/PRJ-8866", Command: "zsh", Occupancy: launcher.OccupancyFree}}

	h.handle(t, notification("1789049440.051109", ""))

	if got := h.windows.created[0].Name; got != "rev/PRJ-8866-2" {
		t.Errorf("window name = %q, want a suffixed name", got)
	}
}

const foreign = "C200TEAM"

func foreignNotification(ts, threadTS string) notifylog.Notification {
	n := notification(ts, threadTS)
	n.Channel = foreign
	return n
}

func triageMessage() slackfetch.Message {
	return slackfetch.Message{
		AuthorID:     "U400PEER",
		AuthorName:   "teammate-a",
		Text:         "Alex, check both, when you have time",
		ChannelLabel: "#team-a-dev",
		SelfInThread: true,
		ThreadSize:   49,
		ThreadTail: []slackfetch.Reply{
			{AuthorName: "teammate-c", Text: "check too please MR 3"},
		},
	}
}

func TestTriageOpensHaikuWindowWithModelAndTools(t *testing.T) {
	h := newHarness(t, triageMessage())
	h.verdicts.verdict = triage.Verdict{React: false, Reason: "asking the other Alex"}

	h.handle(t, foreignNotification("1789054475.628329", "1789034159.622289"))

	if len(h.windows.created) != 1 {
		t.Fatalf("windows created = %v", h.windows.created)
	}
	got := h.windows.created[0]
	if !strings.HasPrefix(got.Name, "tri/") {
		t.Errorf("window name = %q, want tri/ prefix", got.Name)
	}
	payload := h.prompts.payloads[0]
	if payload.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("model = %q", payload.Model)
	}
	if len(payload.AllowedTools) == 0 || payload.AllowedTools[0] != "Write" {
		t.Errorf("allowed tools = %v, Write is required to save the verdict", payload.AllowedTools)
	}
	if !strings.Contains(payload.Prompt, "Alex, check both, when you have time") {
		t.Error("triage prompt must carry the message")
	}
	if got.CWD != "/projects" {
		t.Errorf("cwd = %q: the window must use a trusted directory or Claude Code will ask for trust", got.CWD)
	}
	if len(payload.AddDirs) != 1 || payload.AddDirs[0] != "/state/verdicts" {
		t.Errorf("add_dirs = %v: without it Write sends the verdict to the scratchpad", payload.AddDirs)
	}
	if payload.SessionID != "" || payload.Settings != "" {
		t.Errorf("triage window is short-lived and needs no session hooks: %+v", payload)
	}
}

func TestTriageWithoutReactionClosesWindowAndOpensNothingElse(t *testing.T) {
	h := newHarness(t, triageMessage())
	h.verdicts.verdict = triage.Verdict{React: false, Reason: "chatter"}

	h.handle(t, foreignNotification("1789054475.628329", ""))

	if len(h.windows.killed) != 1 {
		t.Errorf("triage window must be closed: %v", h.windows.killed)
	}
	if len(h.windows.created) != 1 {
		t.Errorf("nothing else must open: %v", h.windows.created)
	}
	if _, ok := h.store.Thread("1789054475.628329"); ok {
		t.Error("declined triage must not remember the thread")
	}
}

func TestTriageReactionEscalatesToMainAgent(t *testing.T) {
	h := newHarness(t, triageMessage())
	h.verdicts.verdict = triage.Verdict{
		React:  true,
		Reason: "asked for review of two MRs",
		Task:   "Review MR 2 and MR 3",
	}

	h.handle(t, foreignNotification("1789054475.628329", ""))

	if len(h.windows.created) != 2 {
		t.Fatalf("want triage window plus main window, got %v", h.windows.created)
	}
	if len(h.windows.killed) != 1 {
		t.Errorf("haiku window must be closed: %v", h.windows.killed)
	}
	main := h.windows.created[1]
	if !strings.HasPrefix(main.Name, "help/") {
		t.Errorf("main window name = %q, want help/ prefix", main.Name)
	}
	payload := h.prompts.payloads[1]
	if payload.Model != "" {
		t.Errorf("without main_model the window must run the CLI default, got %q", payload.Model)
	}
	for _, want := range []string{"Review MR 2 and MR 3", "asked for review of two MRs", "#team-a-dev"} {
		if !strings.Contains(payload.Prompt, want) {
			t.Errorf("main prompt missing %q", want)
		}
	}
	if thread, ok := h.store.Thread("1789054475.628329"); !ok || thread.Kind != "help" || thread.SessionID == "" {
		t.Errorf("escalated thread = %+v, ok = %v", thread, ok)
	}
}

func TestTriageTimeoutClosesWindowAndEscalatesNothing(t *testing.T) {
	h := newHarness(t, triageMessage())
	h.verdicts.err = context.DeadlineExceeded

	h.handle(t, foreignNotification("1789054475.628329", ""))

	if len(h.windows.created) != 1 {
		t.Errorf("timeout must not escalate: %v", h.windows.created)
	}
	if len(h.windows.killed) != 1 {
		t.Errorf("window must be closed on timeout: %v", h.windows.killed)
	}
}

func TestMacroSkipsHaikuEntirely(t *testing.T) {
	msg := triageMessage()
	msg.Text = "Atlas, help — fetch yesterday spend for the ad platform"
	h := newHarness(t, msg)

	h.handle(t, foreignNotification("1789054475.628329", ""))

	if len(h.verdicts.waited) != 0 {
		t.Errorf("macro must not wait for a verdict: %v", h.verdicts.waited)
	}
	if len(h.windows.created) != 1 || !strings.HasPrefix(h.windows.created[0].Name, "help/") {
		t.Errorf("macro must open one main window: %v", h.windows.created)
	}
	if h.prompts.payloads[0].Model != "" {
		t.Error("macro window must run the default model")
	}
}

func TestTriageRateLimitStopsFlood(t *testing.T) {
	h := newHarness(t, triageMessage())
	h.verdicts.verdict = triage.Verdict{React: false, Reason: "no"}
	h.pipe.cfg.TriageLimit = 2

	for _, ts := range []string{"1789054475.000001", "1789054475.000002", "1789054475.000003"} {
		h.handle(t, foreignNotification(ts, ""))
	}

	if len(h.windows.created) != 2 {
		t.Errorf("windows created = %d, want the limit of 2", len(h.windows.created))
	}
}

func TestLiveHelpWindowGetsContinuationWithoutTriage(t *testing.T) {
	h := newHarness(t, triageMessage())
	h.verdicts.verdict = triage.Verdict{React: true, Reason: "asked for review", Task: "Review MR"}
	root := "1789034159.622289"
	h.handle(t, foreignNotification(root, ""))
	h.liveWindow("@1", "help/x")

	h.handle(t, foreignNotification("1789054999.000100", root))

	if len(h.verdicts.waited) != 1 {
		t.Errorf("continuation must not run triage again: %v", h.verdicts.waited)
	}
	if len(h.windows.sent) != 1 {
		t.Fatalf("prompts sent = %v", h.windows.sent)
	}
	if strings.Contains(h.windows.sent[0], "REVIEW DONE") {
		t.Errorf("non-review continuation must not demand review rituals: %s", h.windows.sent[0])
	}
}

func TestEnqueueWaitsForRoomInsteadOfDropping(t *testing.T) {
	h := newHarness(t, triageMessage())
	h.pipe.queue = make(chan notifylog.Notification, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if !h.pipe.Enqueue(ctx, foreignNotification("1.1", "")) {
		t.Fatal("first notification must be admitted")
	}
	admitted := make(chan bool, 1)
	go func() { admitted <- h.pipe.Enqueue(ctx, foreignNotification("1.2", "")) }()
	select {
	case <-admitted:
		t.Fatal("second notification must wait for room, not be dropped")
	case <-time.After(20 * time.Millisecond):
	}
	<-h.pipe.queue
	if !<-admitted {
		t.Error("second notification must be admitted once the worker drains")
	}
	if h.inbox.count() != 2 {
		t.Errorf("inbox = %+v, want both admitted notifications", h.inbox.records)
	}

	cancel()
	if h.pipe.Enqueue(ctx, foreignNotification("1.3", "")) {
		t.Error("a cancelled context must refuse admission")
	}
}

func TestNotificationIsSettledOnlyAfterProcessing(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	n := notification("1789049440.051109", "")
	h.pipe.Enqueue(context.Background(), n)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.pipe.fetcher = fakeFetcher{err: errors.New("slack unavailable")}

	if err := h.pipe.Handle(ctx, n); err == nil {
		t.Fatal("Handle under a cancelled context must fail")
	}
	if h.store.Seen(n.ID) || h.inbox.count() != 1 || h.dead.count() != 0 {
		t.Errorf("interrupted notification must stay admitted: seen=%v inbox=%d dead=%d", h.store.Seen(n.ID), h.inbox.count(), h.dead.count())
	}

	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: peerID, Text: handoff}}
	h.handle(t, n)

	if !h.store.Seen(n.ID) || h.inbox.count() != 0 {
		t.Errorf("handled notification must be settled: seen=%v inbox=%d", h.store.Seen(n.ID), h.inbox.count())
	}
}

func TestRecoverRequeuesUnhandledInboxRecords(t *testing.T) {
	h := newHarness(t, triageMessage())
	pending, done := foreignNotification("1.1", ""), foreignNotification("1.2", "")
	_ = h.inbox.Add(pending, "notification", time.Now())
	_ = h.inbox.Add(done, "notification", time.Now())
	if err := h.store.CommitSeen(done.ID); err != nil {
		t.Fatal(err)
	}

	if got := h.pipe.Recover(context.Background()); got != 1 {
		t.Errorf("recovered = %d", got)
	}
	if len(h.pipe.queue) != 1 || (<-h.pipe.queue).ID != pending.ID {
		t.Error("only the unhandled record goes back to the queue")
	}
	if h.inbox.count() != 1 || h.inbox.records[0].Notification.ID != pending.ID {
		t.Errorf("settled record must leave the inbox: %+v", h.inbox.records)
	}
}

func TestVerdictIsRecoveredFromWindowOutput(t *testing.T) {
	h := newHarness(t, triageMessage())
	h.verdicts.err = context.DeadlineExceeded
	h.windows.pane = `Alex, check both
⏺ {
    "react": true,
    "reason": "asked for review of two MRs",
    "task": "Review MR 2 and MR 3"
  }
`

	h.handle(t, foreignNotification("1789054475.628329", ""))

	if len(h.windows.captured) != 1 {
		t.Errorf("pane must be captured once: %v", h.windows.captured)
	}
	if len(h.windows.created) != 2 {
		t.Fatalf("verdict printed to chat must still escalate: %v", h.windows.created)
	}
	if !strings.Contains(h.prompts.payloads[1].Prompt, "Review MR 2 and MR 3") {
		t.Error("recovered task must reach the main prompt")
	}
}

func TestUnusableWindowOutputEscalatesNothing(t *testing.T) {
	h := newHarness(t, triageMessage())
	h.verdicts.err = context.DeadlineExceeded
	h.windows.pane = "I considered it and decided it should be checked"

	h.handle(t, foreignNotification("1789054475.628329", ""))

	if len(h.windows.created) != 1 {
		t.Errorf("garbage output must not escalate: %v", h.windows.created)
	}
	if len(h.windows.killed) != 1 {
		t.Errorf("window must still be closed: %v", h.windows.killed)
	}
}

func TestReviewFollowUpTakesMergeRequestFromThreadTail(t *testing.T) {
	msg := triageMessage()
	msg.Text = "<@U100SELF> replied to your review"
	msg.ThreadTail = []slackfetch.Reply{
		{AuthorName: "teammate-c", Text: "check this MR too https://gitlab.example.com/team-c/service-c/-/merge_requests/2"},
		{AuthorName: "teammate-a", Text: "replied to the findings"},
	}
	h := newHarness(t, msg)

	h.handle(t, foreignNotification("1789120092.251979", "1789034159.622289"))

	if len(h.verdicts.waited) != 0 {
		t.Errorf("review replies must not go to Haiku: %v", h.verdicts.waited)
	}
	if len(h.worktrees.calls) != 1 || h.worktrees.calls[0].IID != 2 {
		t.Fatalf("worktree calls = %+v, the link must come from the thread", h.worktrees.calls)
	}
	if got := h.windows.created[0]; got.CWD != worktreePath {
		t.Errorf("cwd = %q, want worktree", got.CWD)
	}
}

func TestTriageIgnoresThreadTailLinks(t *testing.T) {
	msg := triageMessage()
	msg.Text = "when is the deployment?"
	msg.ThreadTail = []slackfetch.Reply{
		{AuthorName: "teammate-c", Text: "https://gitlab.example.com/team-c/service-c/-/merge_requests/2"},
	}
	h := newHarness(t, msg)
	h.verdicts.verdict = triage.Verdict{React: false, Reason: "chatter"}

	h.handle(t, foreignNotification("1789120092.251979", "1789034159.622289"))

	if len(h.worktrees.calls) != 0 {
		t.Errorf("triage must not prepare a worktree yet: %+v", h.worktrees.calls)
	}
}

func TestJournalRecordsEveryDecision(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, AuthorName: "teammate-b", Text: handoff})
	root := "1789047527.174689"
	h.handle(t, notification(root, ""))
	h.liveWindow("@1", "rev/PRJ-8866")
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: peerID, Text: "[HEAD CHANGED] → " + newSHA}}
	h.handle(t, notification("1789049999.000100", root))
	h.windows.existing = nil
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: peerID, Text: "okay, thanks"}}
	h.handle(t, notification("1789049999.000200", root))
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: peerID, Text: "just chatting"}}
	h.handle(t, notification("1789049999.000300", ""))

	got := h.journal.actions()
	want := []string{journal.ActionOpened, journal.ActionContinued, journal.ActionSession, journal.ActionSkipped, journal.ActionIgnored}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("actions = %v, want %v", got, want)
	}
	first := h.journal.entries[0]
	if first.WindowID != "@1" || first.SessionID != "sess-1" || first.Worktree != worktreePath || first.Kind != "review" || !first.Mentioned {
		t.Errorf("opened entry = %+v", first)
	}
	if first.AuthorName != "teammate-b" || !strings.Contains(first.Text, "[HANDOFF]") || first.Source != journal.SourceNotification {
		t.Errorf("opened entry context = %+v", first)
	}
}

func TestJournalRecordsTriageVerdictAndLatency(t *testing.T) {
	h := newHarness(t, triageMessage())
	h.verdicts.verdict = triage.Verdict{React: false, Reason: "chatter"}

	h.handle(t, foreignNotification("1789054475.628329", ""))

	e := h.journal.last()
	if e.Action != journal.ActionSilent || e.Kind != "triage" || e.Triage == nil {
		t.Fatalf("entry = %+v", e)
	}
	if e.Triage.React || e.Triage.Reason != "chatter" || e.Triage.Outcome != journal.OutcomeVerdict {
		t.Errorf("triage = %+v", e.Triage)
	}
}

func TestJournalMarksTriageTimeoutAsDropped(t *testing.T) {
	h := newHarness(t, triageMessage())
	h.verdicts.err = context.DeadlineExceeded

	h.handle(t, foreignNotification("1789054475.628329", ""))

	e := h.journal.last()
	if e.Action != journal.ActionDropped || e.Triage == nil || e.Triage.Outcome != journal.OutcomeTimeout {
		t.Errorf("entry = %+v triage = %+v", e, e.Triage)
	}
}

func TestJournalRecordsEscalationWithPaneOutcome(t *testing.T) {
	h := newHarness(t, triageMessage())
	h.verdicts.err = context.DeadlineExceeded
	h.windows.pane = `{"react": true, "reason": "asked", "task": "Review"}`

	h.handle(t, foreignNotification("1789054475.628329", ""))

	e := h.journal.last()
	if e.Action != journal.ActionEscalated || e.Triage.Outcome != journal.OutcomePane || e.WindowID == "" {
		t.Errorf("entry = %+v triage = %+v", e, e.Triage)
	}
}

func TestJournalRecordsErrorAfterRetries(t *testing.T) {
	h := newHarness(t, slackfetch.Message{})
	h.pipe.fetcher = fakeFetcher{err: errors.New("slack unavailable")}

	_ = h.pipe.Handle(context.Background(), notification("1789049440.051109", ""))

	e := h.journal.last()
	if e.Action != journal.ActionError || !strings.Contains(e.Error, "slack unavailable") {
		t.Errorf("entry = %+v", e)
	}
}

func TestJournalRecordsRateLimitDrop(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: "U300TWIN", Text: "Atlas, check"})

	for _, ts := range []string{"1789049440.000001", "1789049440.000002", "1789049440.000003"} {
		h.handle(t, notification(ts, ""))
	}

	e := h.journal.last()
	if e.Action != journal.ActionDropped || !strings.Contains(e.Reason, "rate limit") {
		t.Errorf("entry = %+v", e)
	}
}

func TestJournalRecordsQueuedDelivery(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	root := "1789047527.174689"
	h.handle(t, notification(root, ""))
	h.liveWindow("@1", "rev/PRJ-8866")
	h.sessions.set("sess-1", session.Blocked)
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: peerID, Text: "replied"}}

	h.handle(t, notification("1789049999.000100", root))

	if e := h.journal.last(); e.Action != journal.ActionQueued {
		t.Errorf("entry = %+v", e)
	}
	h.sessions.set("sess-1", session.Idle)
	waitFor(t, "queued prompt", func() bool { return h.windows.sentCount() == 1 })
}

func TestTriageWindowIsHeldOnExitButMainWindowsAreNot(t *testing.T) {
	h := newHarness(t, triageMessage())
	h.verdicts.verdict = triage.Verdict{React: true, Reason: "asked", Task: "do it"}

	h.handle(t, foreignNotification("1789054475.628329", ""))

	if len(h.windows.held) != 1 || h.windows.held[0] != "@1" {
		t.Errorf("held = %v, want only the triage window", h.windows.held)
	}
}

func TestTriageStopsWaitingWhenWindowDies(t *testing.T) {
	h := newHarness(t, triageMessage())
	h.verdicts.block = true
	h.windows.dieAfter = 2
	h.windows.pane = "the model exited without a file"
	started := time.Now()

	h.handle(t, foreignNotification("1789054475.628329", ""))

	if time.Since(started) > time.Second {
		t.Errorf("dead window must end the wait quickly, took %s", time.Since(started))
	}
	e := h.journal.last()
	if e.Action != journal.ActionDropped || e.Triage == nil || e.Triage.Outcome != journal.OutcomeWindowDied {
		t.Errorf("entry = %+v triage = %+v", e, e.Triage)
	}
	if len(h.windows.captured) != 1 || len(h.windows.killed) != 1 {
		t.Errorf("dead pane must be captured then killed: captured=%v killed=%v", h.windows.captured, h.windows.killed)
	}
}

func TestVerdictWrittenRightBeforeDeathIsStillUsed(t *testing.T) {
	h := newHarness(t, triageMessage())
	h.verdicts.block = true
	h.verdicts.peek = &triage.Verdict{React: true, Reason: "completed in time", Task: "Review"}
	h.windows.dieAfter = 1

	h.handle(t, foreignNotification("1789054475.628329", ""))

	if len(h.windows.created) != 2 {
		t.Fatalf("late verdict must still escalate: %v", h.windows.created)
	}
	if e := h.journal.last(); e.Action != journal.ActionEscalated || e.Triage.Outcome != journal.OutcomeVerdict {
		t.Errorf("entry = %+v triage = %+v", e, e.Triage)
	}
}

func TestVerdictPrintedByDyingWindowIsRecoveredFromPane(t *testing.T) {
	h := newHarness(t, triageMessage())
	h.verdicts.block = true
	h.windows.dieAfter = 1
	h.windows.pane = `{"react": true, "reason": "printed", "task": "Review"}`

	h.handle(t, foreignNotification("1789054475.628329", ""))

	if e := h.journal.last(); e.Action != journal.ActionEscalated || e.Triage.Outcome != journal.OutcomePane {
		t.Errorf("entry = %+v triage = %+v", e, e.Triage)
	}
}

func TestBotMessageInWatchedThreadIsIgnoredWithoutTriage(t *testing.T) {
	msg := triageMessage()
	msg.IsBot = true
	msg.AuthorID = "B01GITLAB"
	msg.Text = "Pipeline passed for !899"
	h := newHarness(t, msg)

	h.handle(t, foreignNotification("1789054475.628329", "1789034159.622289"))

	if len(h.windows.created) != 0 {
		t.Errorf("bot must not reach Haiku: %v", h.windows.created)
	}
	if e := h.journal.last(); e.Action != journal.ActionIgnored || !strings.Contains(e.Reason, "bot") {
		t.Errorf("entry = %+v", e)
	}
}

func TestTriagePromptCarriesPeerAgents(t *testing.T) {
	h := newHarness(t, triageMessage())
	h.verdicts.verdict = triage.Verdict{React: false, Reason: "for Ada"}

	h.handle(t, foreignNotification("1789054475.628329", ""))

	if !strings.Contains(h.prompts.payloads[0].Prompt, "Ada, Orbit") {
		t.Error("triage prompt must name peer agents from config")
	}
}

func TestTriageDoesNotBlockTheWorker(t *testing.T) {
	h := newHarness(t, triageMessage())
	h.verdicts.block = true
	ctx, cancel := context.WithCancel(context.Background())
	started := time.Now()

	if err := h.pipe.Handle(ctx, foreignNotification("1789054475.628329", "")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if time.Since(started) > 200*time.Millisecond {
		t.Errorf("worker waited for the verdict: %s", time.Since(started))
	}
	if len(h.windows.created) != 1 {
		t.Fatalf("triage window must already be open: %v", h.windows.created)
	}
	cancel()
	h.pipe.Wait()
	for _, e := range h.journal.entries {
		if e.Action == journal.ActionDropped {
			t.Errorf("a wait cut short by shutdown is not a decision: %+v", e)
		}
	}
}

func TestMessagesArrivingDuringTriageAreDeferredThenReplayed(t *testing.T) {
	h := newHarness(t, triageMessage())
	h.verdicts.block = true
	h.verdicts.release = make(chan struct{})
	h.verdicts.verdict = triage.Verdict{React: true, Reason: "asked", Task: "Do it"}
	root := "1789034159.622289"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := h.pipe.Handle(ctx, foreignNotification("1789054475.628329", root)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if err := h.pipe.Handle(ctx, foreignNotification("1789054480.000001", root)); err != nil {
		t.Fatalf("second Handle: %v", err)
	}

	if got := h.journal.last(); got.Action != journal.ActionQueued || !strings.Contains(got.Reason, "in flight") {
		t.Fatalf("second message must be deferred, entry = %+v", got)
	}
	h.windows.mu.Lock()
	created := len(h.windows.created)
	h.windows.mu.Unlock()
	if created != 1 {
		t.Fatalf("no second triage window while the first is running: %d", created)
	}
	h.liveWindow("@1", "help/team-a-dev")
	close(h.verdicts.release)
	h.pipe.Wait()

	h.windows.mu.Lock()
	sent := len(h.windows.sent)
	created = len(h.windows.created)
	h.windows.mu.Unlock()
	if created != 2 {
		t.Errorf("windows = %d, want triage plus escalated window", created)
	}
	if sent != 1 {
		t.Errorf("deferred message must reach the new window as a continuation, sent = %d", sent)
	}
	if e := h.journal.last(); e.Action != journal.ActionContinued {
		t.Errorf("replayed message entry = %+v", e)
	}
	if !h.store.Seen("T100_1789054480.000001") {
		t.Error("deferred message must be settled once it is really handled")
	}
}

func TestTriageEscalationFailureIsJournaled(t *testing.T) {
	h := newHarness(t, triageMessage())
	h.verdicts.verdict = triage.Verdict{React: true, Reason: "asked", Task: "Do it"}
	h.pipe.render = brokenRenderer(t)

	_ = h.pipe.Handle(context.Background(), foreignNotification("1789054475.628329", ""))
	h.pipe.Wait()

	if e := h.journal.last(); e.Action != journal.ActionError || e.Error == "" {
		t.Errorf("entry = %+v", e)
	}
}

func brokenRenderer(t *testing.T) prompt.Renderer {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ask.tmpl"), []byte("{{.NoSuchField}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := prompt.New(config.Defaults().Persona, dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDroppedNotificationsBecomeDeadLetters(t *testing.T) {
	h := newHarness(t, triageMessage())
	h.verdicts.err = context.DeadlineExceeded

	h.handle(t, foreignNotification("1789054475.628329", ""))

	if h.dead.count() != 1 || !strings.Contains(h.dead.records[0].Reason, "no triage verdict") {
		t.Errorf("dead letters = %+v", h.dead.records)
	}
	if h.dead.records[0].Notification.MsgTS != "1789054475.628329" {
		t.Errorf("dead letter must carry the original notification: %+v", h.dead.records[0].Notification)
	}
}

func TestFailedNotificationsBecomeDeadLetters(t *testing.T) {
	h := newHarness(t, slackfetch.Message{})
	h.pipe.fetcher = fakeFetcher{err: errors.New("slack unavailable")}

	_ = h.pipe.Handle(context.Background(), notification("1789049440.051109", ""))

	if h.dead.count() != 1 || !strings.Contains(h.dead.records[0].Reason, "slack unavailable") {
		t.Errorf("dead letters = %+v", h.dead.records)
	}
}

func TestSilentAndHandledNotificationsAreNotDeadLetters(t *testing.T) {
	h := newHarness(t, triageMessage())
	h.verdicts.verdict = triage.Verdict{React: false, Reason: "chatter"}

	h.handle(t, foreignNotification("1789054475.628329", ""))
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: peerID, Text: handoff}}
	h.handle(t, notification("1789049440.051109", ""))

	if h.dead.count() != 0 {
		t.Errorf("dead letters = %+v", h.dead.records)
	}
}

func TestDirectTagWithoutVerdictStillEscalates(t *testing.T) {
	msg := triageMessage()
	msg.Text = "<@U100SELF> check please both MR"
	h := newHarness(t, msg)
	h.verdicts.err = context.DeadlineExceeded

	h.handle(t, foreignNotification("1789054475.628329", ""))

	if len(h.windows.created) != 2 {
		t.Fatalf("tagged message must escalate even without a verdict: %v", h.windows.created)
	}
	if e := h.journal.last(); e.Action != journal.ActionEscalated || !strings.Contains(e.Reason, "direct tag") {
		t.Errorf("entry = %+v", e)
	}
	if h.dead.count() != 0 {
		t.Error("escalated message is not a dead letter")
	}
}

func TestReplayFeedsDeadLettersBackThroughTheWorker(t *testing.T) {
	h := newHarness(t, triageMessage())
	h.verdicts.err = context.DeadlineExceeded
	h.handle(t, foreignNotification("1789054475.628329", ""))
	if h.dead.count() != 1 {
		t.Fatalf("precondition: one dead letter, got %d", h.dead.count())
	}
	h.verdicts.err = nil
	h.verdicts.verdict = triage.Verdict{React: true, Reason: "second attempt", Task: "Do it"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.pipe.Work(ctx)

	n, err := h.pipe.Replay(ctx)

	if err != nil || n != 1 {
		t.Fatalf("Replay = %d, %v", n, err)
	}
	waitFor(t, "replayed escalation", func() bool {
		h.windows.mu.Lock()
		defer h.windows.mu.Unlock()
		return len(h.windows.created) == 3
	})
	h.pipe.Wait()
	e := h.journal.last()
	if e.Action != journal.ActionEscalated || e.Source != journal.SourceReplay {
		t.Errorf("entry = %+v", e)
	}
	waitFor(t, "dead letter removal", func() bool { return h.dead.count() == 0 })
}

func TestFailedReplayKeepsTheDeadLetterUntilItSucceeds(t *testing.T) {
	h := newHarness(t, triageMessage())
	h.verdicts.err = context.DeadlineExceeded
	h.handle(t, foreignNotification("1789054475.628329", ""))
	h.pipe.fetcher = fakeFetcher{err: errors.New("slack unavailable")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.pipe.Work(ctx)

	if n, err := h.pipe.Replay(ctx); err != nil || n != 1 {
		t.Fatalf("Replay = %d, %v", n, err)
	}
	waitFor(t, "replay failure recorded", func() bool {
		h.dead.mu.Lock()
		defer h.dead.mu.Unlock()
		return len(h.dead.records) == 1 && strings.Contains(h.dead.records[0].Reason, "slack unavailable")
	})
	if n, _ := h.pipe.Replay(ctx); n != 1 {
		t.Errorf("a failed replay must be replayable again, queued = %d", n)
	}
}

func TestStopPhraseClosesTheThreadWindowAndRecordsFeedback(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	root := "1789047527.174689"
	h.handle(t, notification(root, ""))
	h.liveWindow("@1", "rev/PRJ-8866")
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: selfID, Text: "Atlas, stop — I will finish checking"}}

	h.handle(t, notification("1789049999.000100", root))

	if len(h.windows.killed) != 1 || h.windows.killed[0] != "@1" {
		t.Errorf("killed = %v", h.windows.killed)
	}
	th, ok := h.store.Thread(root)
	if !ok || !th.WindowGone() {
		t.Errorf("thread = %+v ok=%v, want the record kept without a window", th, ok)
	}
	e := h.journal.last()
	if e.Action != journal.ActionFeedback || e.WindowID != "@1" || !strings.Contains(e.Reason, "closed") {
		t.Errorf("entry = %+v", e)
	}
	if len(h.windows.sent) != 0 {
		t.Error("stop phrase must not be typed into the window")
	}
}

func TestStopPhraseWithoutWindowIsJustFeedback(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: "U300TWIN", Text: "Atlas, stop, not for you", ChannelLabel: "#team-a-dev", SelfInThread: true})

	h.handle(t, foreignNotification("1789054475.628329", "1789034159.622289"))

	if len(h.windows.created) != 0 || len(h.verdicts.waited) != 0 {
		t.Errorf("stop must not open anything: %v %v", h.windows.created, h.verdicts.waited)
	}
	if e := h.journal.last(); e.Action != journal.ActionFeedback {
		t.Errorf("entry = %+v", e)
	}
}

func TestOwnReviewDoneInRememberedThreadRecordsProgress(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	root := "1789047527.174689"
	h.handle(t, notification(root, ""))
	h.liveWindow("@1", "rev/PRJ-8866")
	h.worktrees.synced = worktree.Result{Path: worktreePath, SHA: newSHA, HeadSHA: newSHA}
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: selfID, AuthorName: "Atlas", Text: ":robot_face: [REVIEW DONE] 4a8259ce0aa1 — 2 findings"}}

	h.handle(t, notification("1789049999.000100", root))

	if len(h.worktrees.inspected) != 1 {
		t.Errorf("completion must re-read the provider head, inspected = %v", h.worktrees.inspected)
	}
	if len(h.windows.created) != 1 || len(h.windows.sent) != 0 {
		t.Errorf("own marker must neither open nor continue a window: created=%v sent=%v", h.windows.created, h.windows.sent)
	}
	e := h.journal.last()
	if e.Action != journal.ActionProgress || e.Phase != "review_done" || e.HeadSHA != "4a8259ce0aa1" || e.ThreadTS != root || e.SessionID != "sess-1" {
		t.Errorf("entry = %+v", e)
	}
	th, _ := h.store.Thread(root)
	if th.Progress != "review_done" || th.ProgressSHA != "4a8259ce0aa1" || th.LastSeenTS != "1789049999.000100" {
		t.Errorf("thread = %+v", th)
	}
}

func TestLateReviewDoneForOldHeadDoesNotCloseTheRound(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	root := "1789047527.174689"
	h.handle(t, notification(root, ""))
	h.liveWindow("@1", "rev/PRJ-8866")
	h.worktrees.synced = worktree.Result{Path: worktreePath, SHA: newSHA, HeadSHA: newSHA}
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: selfID, Text: "[REVIEW DONE] " + oldSHA[:12] + " — 2 findings"}}

	h.handle(t, notification("1789049999.000100", root))

	e := h.journal.last()
	if e.Action != journal.ActionStale || e.Phase != "review_done" || e.HeadSHA != oldSHA[:12] || !strings.Contains(e.Reason, newSHA) {
		t.Errorf("entry = %+v", e)
	}
	th, _ := h.store.Thread(root)
	if th.Progress != "" || th.HeadSHA != newSHA || th.LastSeenTS != "1789049999.000100" {
		t.Errorf("thread = %+v", th)
	}
	if outs := journal.Outcomes(h.journal.entries, h.pipe.now(), time.Hour); len(outs) != 1 || outs[0].Status != journal.StatusOpen {
		t.Errorf("outcomes = %+v, want the round still open", outs)
	}

	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: selfID, Text: "[REVIEW DONE] " + newSHA + " — 1 finding"}}
	h.handle(t, notification("1789049999.000200", root))

	th, _ = h.store.Thread(root)
	if th.Progress != "review_done" || th.ProgressSHA != newSHA {
		t.Errorf("thread after a fresh verdict = %+v", th)
	}
	if outs := journal.Outcomes(h.journal.entries, h.pipe.now(), time.Hour); outs[0].Status != journal.StatusDone {
		t.Errorf("outcomes = %+v", outs)
	}
}

func TestCompletionIsPendingWhenProviderHeadIsUnknown(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	root := "1789047527.174689"
	h.handle(t, notification(root, ""))
	h.liveWindow("@1", "rev/PRJ-8866")
	h.worktrees.inspectErr = errors.New("fetch refs/merge-requests/899/head failed")
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: selfID, Text: "[DONE] " + newSHA[:10]}}

	h.handle(t, notification("1789049999.000100", root))

	if e := h.journal.last(); e.Action != journal.ActionPending || !strings.Contains(e.Reason, "provider head unknown") {
		t.Errorf("entry = %+v, want the completion held as pending", e)
	}

	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: selfID, Text: "[DONE] " + oldSHA[:10]}}
	h.handle(t, notification("1789049999.000200", root))

	if th, _ := h.store.Thread(root); th.Progress != "" {
		t.Errorf("thread = %+v, a completion must not close the round on the remembered head", th)
	}
	if outs := journal.Outcomes(h.journal.entries, h.pipe.now(), time.Hour); len(outs) != 1 || outs[0].Status != journal.StatusOpen {
		t.Errorf("outcomes = %+v, want the round still open", outs)
	}

	h.worktrees.inspectErr = nil
	h.worktrees.synced = worktree.Result{Path: worktreePath, SHA: newSHA, HeadSHA: newSHA}
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: selfID, Text: "[DONE] " + newSHA}}
	h.handle(t, notification("1789049999.000300", root))

	if th, _ := h.store.Thread(root); th.Progress != "done" || th.ProgressSHA != newSHA {
		t.Errorf("thread = %+v, want the completion accepted once the provider head is readable", th)
	}
}

func TestOwnMarkerAfterWindowClosedStillClosesTheOutcome(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	root := "1789047527.174689"
	h.handle(t, notification(root, ""))
	h.windows.existing = nil
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: selfID, Text: "[TAKEN]"}}

	h.handle(t, notification("1789049999.000100", root))

	if e := h.journal.last(); e.Action != journal.ActionProgress || e.Phase != "taken" {
		t.Errorf("entry = %+v", e)
	}
	if th, _ := h.store.Thread(root); th.Progress != "taken" || !th.WindowGone() {
		t.Errorf("thread = %+v", th)
	}
}

func TestOwnMarkerInUnknownThreadIsIgnored(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: selfID, Text: "[REVIEW DONE] 4a8259ce0aa1"})

	h.handle(t, notification("1789049999.000100", "1789040000.000001"))

	if e := h.journal.last(); e.Action != journal.ActionIgnored {
		t.Errorf("entry = %+v", e)
	}
}

func TestReplyReadInOpenChatIsRerootedByHelper(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	root := "1789047527.174689"
	h.handle(t, notification(root, ""))
	h.liveWindow("@1", "rev/PRJ-8866")
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: peerID, Text: "[HEAD CHANGED] → " + newSHA, ThreadTS: root}}
	reply := notification("1789049999.000100", "")
	reply.Source = notifylog.SourceLastRead

	h.handle(t, reply)

	if len(h.windows.sent) != 1 {
		t.Fatalf("reply must reach the thread window once the helper names its root: sent=%v created=%v", h.windows.sent, h.windows.created)
	}
	if e := h.journal.last(); e.Action != journal.ActionContinued || e.ThreadTS != root {
		t.Errorf("entry = %+v", e)
	}
}

func TestOpenedThreadRemembersTeamAndTimestamps(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	root := "1789047527.174689"
	h.handle(t, notification(root, ""))

	th, _ := h.store.Thread(root)
	if th.TeamID != "T100TEAM" || th.OpenedTS != root || th.LastSeenTS != root {
		t.Fatalf("thread = %+v", th)
	}

	h.liveWindow("@1", "rev/PRJ-8866")
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: peerID, Text: "[HEAD CHANGED] → " + newSHA}}
	h.handle(t, notification("1789049999.000100", root))

	if th, _ := h.store.Thread(root); th.LastSeenTS != "1789049999.000100" || th.OpenedTS != root {
		t.Errorf("thread after continuation = %+v", th)
	}
}

func TestThreadCooldownSkipsChatterAfterSilentVerdictsUntilSomethingPointed(t *testing.T) {
	chatter := slackfetch.Message{AuthorID: "U400PEER", AuthorName: "teammate", Text: "weather chatter", ChannelLabel: "direct message with teammate", IsDM: true}
	h := newHarness(t, chatter)
	h.verdicts.verdict = triage.Verdict{React: false, Reason: "chatter"}
	root := "1789054475.000001"
	dm := func(ts string) notifylog.Notification {
		n := notification(ts, root)
		n.Channel = "D100PEER"
		return n
	}

	for _, ts := range []string{"1789054475.000001", "1789054475.000002", "1789054475.000003"} {
		h.handle(t, dm(ts))
	}
	if len(h.verdicts.waited) != 3 {
		t.Fatalf("first three chatter messages must be triaged, waited=%v", h.verdicts.waited)
	}

	h.handle(t, dm("1789054475.000004"))
	if len(h.verdicts.waited) != 3 {
		t.Errorf("fourth chatter message must hit the cooldown, waited=%v", h.verdicts.waited)
	}
	if e := h.journal.last(); e.Action != journal.ActionSkipped || !strings.Contains(e.Reason, "cooldown after 3") {
		t.Errorf("entry = %+v", e)
	}

	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: "U400PEER", Text: "can you check?", IsDM: true}}
	h.handle(t, dm("1789054475.000005"))
	if len(h.verdicts.waited) != 4 {
		t.Errorf("a question must break through the cooldown, waited=%v", h.verdicts.waited)
	}

	h.verdicts.verdict = triage.Verdict{React: true, Reason: "asked", Task: "Take a look"}
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: "U400PEER", Text: "Sam, check please", IsDM: true}}
	h.handle(t, dm("1789054475.000006"))
	if len(h.verdicts.waited) != 5 || h.journal.last().Action != journal.ActionEscalated {
		t.Fatalf("a mention must be triaged and escalated: waited=%v last=%+v", h.verdicts.waited, h.journal.last())
	}

	h.verdicts.verdict = triage.Verdict{React: false, Reason: "chatter"}
	h.pipe.fetcher = fakeFetcher{msg: chatter}
	h.handle(t, dm("1789054475.000007"))
	if len(h.verdicts.waited) != 6 {
		t.Errorf("a reaction resets the streak, chatter must be triaged again: waited=%v", h.verdicts.waited)
	}
}

func TestThreadCooldownCanBeDisabled(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: "U400PEER", Text: "weather chatter", IsDM: true})
	h.cfg.TriageCooldownN = 0
	render, _ := prompt.New(h.cfg.Persona, "")
	h.pipe = h.build(h.cfg, render)
	h.verdicts.verdict = triage.Verdict{React: false, Reason: "chatter"}
	root := "1789054475.000001"

	for i := 1; i <= 5; i++ {
		n := notification(fmt.Sprintf("1789054475.00000%d", i), root)
		n.Channel = "D100PEER"
		h.handle(t, n)
	}

	if len(h.verdicts.waited) != 5 {
		t.Errorf("without a cooldown every message is triaged, waited=%v", h.verdicts.waited)
	}
}

func TestReviewWindowCarriesReviewerPermissionProfile(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})

	h.handle(t, notification("1789049440.051109", ""))

	settings := h.prompts.payloads[0].Settings
	for _, want := range []string{`"hooks"`, `"allow":[`, `"Bash(git log:*)"`, `"Bash(handoffd post:*)"`, `"deny":[`, `"Bash(git push:*)"`, `"Bash(glab mr approve:*)"`, `"Bash(glab mr note:*)"`, `"mcp__slack-agent-bridge__post_to_agent_channel"`} {
		if !strings.Contains(settings, want) {
			t.Errorf("review settings lack %s: %s", want, settings)
		}
	}
}

func TestHelpWindowKeepsPermissionsInteractive(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: "Atlas, help: deployment failed"})

	h.handle(t, notification("1789049440.051109", ""))

	if got := h.prompts.payloads[0].Settings; got != hookSettings {
		t.Errorf("help window settings = %s, want hooks only", got)
	}
}

func TestResumedReviewWindowKeepsPermissionProfile(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	root := "1789047527.174689"
	h.handle(t, notification(root, ""))
	h.windows.existing = nil
	h.windows.nextID = "@7"
	h.verdicts.verdict = triage.Verdict{React: true, Reason: "answer", Task: "Continue"}
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: peerID, Text: "well https://jira.example.com/browse/PRJ-8874"}}

	h.handle(t, notification("1789049999.000100", root))

	resumed := h.prompts.payloads[len(h.prompts.payloads)-1]
	if !strings.Contains(resumed.Settings, `"deny":[`) || !strings.Contains(resumed.Settings, `"allow":[`) {
		t.Errorf("resumed review window settings = %s", resumed.Settings)
	}
}

type fakeAlerts struct {
	mu   sync.Mutex
	sent []string
}

func (f *fakeAlerts) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func (f *fakeAlerts) Alert(_ context.Context, key, _, text string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, key+" "+text)
	return true
}

func TestDeadLetterRaisesAnAlert(t *testing.T) {
	h := newHarness(t, triageMessage())
	alerts := &fakeAlerts{}
	h.pipe.alerts = alerts
	h.verdicts.err = errors.New("no verdict")
	h.windows.pane = ""

	h.handle(t, foreignNotification("1789054475.628329", ""))

	if h.dead.count() != 1 {
		t.Fatalf("dead letters = %d", h.dead.count())
	}
	alerts.mu.Lock()
	defer alerts.mu.Unlock()
	if len(alerts.sent) != 1 || !strings.HasPrefix(alerts.sent[0], "deadletter:T100_1789054475.628329 ") || !strings.Contains(alerts.sent[0], "handoffd replay") {
		t.Errorf("alerts = %v", alerts.sent)
	}
}

type fakeLabels struct {
	mu     sync.Mutex
	recent []label.Label
	added  []label.Label
}

func (f *fakeLabels) Append(l label.Label) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added = append(f.added, l)
	return nil
}

func (f *fakeLabels) Recent(n int) ([]label.Label, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.recent) > n {
		return f.recent[len(f.recent)-n:], nil
	}
	return f.recent, nil
}

func TestTriagePromptLearnsFromRecentLabels(t *testing.T) {
	h := newHarness(t, triageMessage())
	h.pipe.labels = &fakeLabels{recent: []label.Label{{ID: "T1_1", Text: "weather chatter", React: false}, {ID: "T1_2", Text: "check MR", React: true}}}
	h.verdicts.verdict = triage.Verdict{React: false, Reason: "no"}

	h.handle(t, foreignNotification("1789054475.628329", ""))

	prompt := h.prompts.payloads[0].Prompt
	if !strings.Contains(prompt, "\"weather chatter\" → react: false") || !strings.Contains(prompt, "\"check MR\" → react: true") {
		t.Errorf("triage prompt lacks labelled examples:\n%s", prompt)
	}
}

func TestStopPhraseLabelsTheOpeningDecisionAsBad(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	labels := &fakeLabels{}
	h.pipe.labels = labels
	root := "1789047527.174689"
	h.handle(t, notification(root, ""))
	h.liveWindow("@1", "rev/PRJ-8866")
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: peerID, Text: "Atlas, stop"}}

	h.handle(t, notification("1789049999.000100", root))

	if len(labels.added) != 1 {
		t.Fatalf("labels = %+v, want the opening decision labelled", labels.added)
	}
	got := labels.added[0]
	if got.ID != "T100TEAM_"+root || got.Verdict != label.Bad || got.React || got.Source != label.SourceStopPhrase || !strings.Contains(got.Text, "[HANDOFF]") {
		t.Errorf("label = %+v", got)
	}
}

const committeeHandoff = "[COMMITTEE] " + handoff

func committeeHarness(t *testing.T, agents ...string) *harness {
	t.Helper()
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, AuthorName: "teammate-b", Text: committeeHandoff})
	h.cfg.Agent = "claude"
	h.cfg.MainModel = "claude-opus-5"
	h.cfg.CommitteeAgents = []string{"claude", "codex"}
	render, _ := prompt.New(h.cfg.Persona, "")
	h.pipe = h.build(h.cfg, render)
	h.pipe.agents = installed(agents)
	return h
}

func TestCommitteeMarkerOpensOneReviewWindowPerInstalledAgent(t *testing.T) {
	h := committeeHarness(t, "claude", "codex")
	h.windows.nextIDs = []string{"@1", "@2"}
	root := "1789049440.051109"

	h.handle(t, notification(root, ""))

	if len(h.windows.created) != 2 {
		t.Fatalf("windows = %+v, want member then driver", h.windows.created)
	}
	if h.windows.created[0].Name != "rev/PRJ-8866~codex" || h.windows.created[1].Name != "rev/PRJ-8866" {
		t.Errorf("window names = %q %q, members must open before the driver", h.windows.created[0].Name, h.windows.created[1].Name)
	}
	member, driver := h.prompts.payloads[0], h.prompts.payloads[1]
	if driver.Agent != "claude" || driver.Model != "claude-opus-5" || member.Agent != "codex" || member.Model != "" {
		t.Errorf("payload agents/models = %s/%s %s/%s", driver.Agent, driver.Model, member.Agent, member.Model)
	}
	if !strings.Contains(driver.Prompt, "You drive") || !strings.Contains(driver.Prompt, "joint --review-done") || !strings.Contains(driver.Prompt, "parallel by codex") {
		t.Errorf("driver prompt lacks the committee role:\n%s", driver.Prompt)
	}
	if !strings.Contains(member.Prompt, "claude writes the consolidated verdict") || !strings.Contains(member.Prompt, "tagged (codex)") || !strings.Contains(member.Prompt, "you are codex") {
		t.Errorf("member prompt lacks its role:\n%s", member.Prompt)
	}
	if !strings.Contains(member.Settings, `"deny":[`) || len(member.DeniedTools) == 0 {
		t.Error("committee member must get the reviewer policy too")
	}
	if len(h.worktrees.calls) != 2 || !h.worktrees.busy[1][worktreePath] {
		t.Errorf("member worktree must avoid the driver's tree: calls=%d busy=%v", len(h.worktrees.calls), h.worktrees.busy)
	}
	th, _ := h.store.Thread(root)
	if th.WindowID != "@2" || len(th.Members) != 1 || th.Members[0].Agent != "codex" || th.Members[0].WindowID != "@1" || th.Members[0].SessionID != "sess-1" {
		t.Errorf("thread = %+v", th)
	}
	if e := h.journal.last(); e.Action != journal.ActionOpened || e.Reason != "committee: claude, codex" {
		t.Errorf("entry = %+v", e)
	}
}

func TestCommitteeFallsBackToSingleReviewWhenMemberWindowFails(t *testing.T) {
	h := committeeHarness(t, "claude", "codex")
	h.windows.failNames = map[string]error{"rev/PRJ-8866~codex": errors.New("tmux: no server")}
	root := "1789049440.051109"

	h.handle(t, notification(root, ""))

	if len(h.windows.created) != 1 || h.windows.created[0].Name != "rev/PRJ-8866" {
		t.Fatalf("windows = %+v, want the driver alone", h.windows.created)
	}
	driver := h.prompts.payloads[len(h.prompts.payloads)-1]
	if driver.Agent != "" || strings.Contains(driver.Prompt, "codex is reviewing") {
		t.Errorf("driver must not wait for a peer that never started: agent=%q\n%s", driver.Agent, driver.Prompt)
	}
	th, _ := h.store.Thread(root)
	if len(th.Members) != 0 {
		t.Errorf("thread = %+v", th)
	}
	if e := h.journal.last(); e.Action != journal.ActionOpened || e.Reason != "" {
		t.Errorf("entry = %+v", e)
	}
}

func TestCommitteeMemberWithoutExactWorktreeIsLeftOut(t *testing.T) {
	h := committeeHarness(t, "claude", "codex")
	h.worktrees.errOnCall = 2
	h.worktrees.err = errors.New("fetch refs/merge-requests/899/head failed")

	h.handle(t, notification("1789049440.051109", ""))

	if len(h.windows.created) != 1 || len(h.prompts.payloads) != 1 || h.prompts.payloads[0].Agent != "" {
		t.Errorf("windows = %+v payloads = %d, want a single review", h.windows.created, len(h.prompts.payloads))
	}
}

func TestCommitteeFallsBackToSingleReviewWithoutSecondAgent(t *testing.T) {
	h := committeeHarness(t, "claude")

	h.handle(t, notification("1789049440.051109", ""))

	if len(h.windows.created) != 1 || h.prompts.payloads[0].Agent != "" {
		t.Errorf("windows = %+v agent=%q, want a plain single review", h.windows.created, h.prompts.payloads[0].Agent)
	}
	if strings.Contains(h.prompts.payloads[0].Prompt, "Committee") {
		t.Error("single review must not mention a committee")
	}
	if th, _ := h.store.Thread("1789049440.051109"); len(th.Members) != 0 {
		t.Errorf("thread = %+v", th)
	}
}

func TestContinuationReachesEveryLiveCommitteeWindow(t *testing.T) {
	h := committeeHarness(t, "claude", "codex")
	h.windows.nextIDs = []string{"@1", "@2"}
	root := "1789049440.051109"
	h.handle(t, notification(root, ""))
	h.windows.mu.Lock()
	h.windows.existing = []launcher.Window{{ID: "@1", Name: "rev/PRJ-8866~codex", Command: "2.1.267", Occupancy: launcher.OccupancyAgent}, {ID: "@2", Name: "rev/PRJ-8866", Command: "2.1.267", Occupancy: launcher.OccupancyAgent}}
	h.windows.mu.Unlock()
	h.worktrees.synced = worktree.Result{Path: worktreePath, SHA: oldSHA, HeadSHA: newSHA}
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: peerID, Text: "[HEAD CHANGED] → " + newSHA}}

	h.handle(t, notification("1789049999.000100", root))

	if len(h.windows.sent) != 2 {
		t.Errorf("continuation must reach both windows, sent=%d", len(h.windows.sent))
	}
	busy := h.pipe.busyWorktrees(context.Background())
	if !busy[worktreePath] {
		t.Errorf("live member worktrees must count as busy: %v", busy)
	}
}

const githubHandoff = "[HANDOFF] <@U100SELF>, take this review https://github.com/acme/service-a/pull/42\nHEAD: `08c9fc23a726a07692b7f9cf28d4ef6f57e4fec6`"

func TestGitHubPullRequestGetsPullRefAndGhHint(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: githubHandoff})
	h.worktrees.result = worktree.Result{Path: "/repo/.claude/worktrees/review-42", SHA: oldSHA, HeadSHA: newSHA, BaseSHA: baseSHA, TargetBranch: "main"}

	h.handle(t, notification("1789049440.051109", ""))

	if len(h.worktrees.calls) != 1 || !h.worktrees.calls[0].IsGitHub() || h.worktrees.calls[0].IID != 42 {
		t.Fatalf("worktree calls = %+v", h.worktrees.calls)
	}
	prompt := h.prompts.payloads[0].Prompt
	for _, want := range []string{"This is a GitHub pull request", "gh pr view/diff/checks", "base " + baseSHA + " = main"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if h.windows.created[0].Name != "rev/service-a-42" {
		t.Errorf("window name = %q", h.windows.created[0].Name)
	}
	th, _ := h.store.Thread("1789049440.051109")
	if th.MergeRef != "refs/pull/42/head" || th.MR == nil || !th.MR.IsGitHub() {
		t.Errorf("thread = %+v", th)
	}
}

func TestGitHubHostAllowlistIsSeparateFromGitLab(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: githubHandoff})
	h.cfg.GitLabHosts = []string{"gitlab.trusted.com"}
	h.cfg.GitHubHosts = []string{"github.corp.example"}
	render, _ := prompt.New(h.cfg.Persona, "")
	h.pipe = h.build(h.cfg, render)

	_ = h.pipe.Handle(context.Background(), notification("1789049440.051109", ""))

	if len(h.worktrees.calls) != 0 {
		t.Errorf("foreign GitHub host must not reach git: %+v", h.worktrees.calls)
	}
	if h.dead.count() != 1 || !strings.Contains(h.dead.records[0].Reason, "GitHub github.com is not in github_hosts") {
		t.Errorf("dead letter must name the GitHub allowlist: %+v", h.dead.records)
	}
}

func TestMacroPromptNamesTheMacroNotTriage(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: "Atlas, help: deployment failed"})

	h.handle(t, notification("1789049440.051109", ""))

	prompt := h.prompts.payloads[0].Prompt
	if !strings.Contains(prompt, "woken by the macro phrase") || strings.Contains(prompt, "decided to wake you") {
		t.Errorf("macro prompt = %s", prompt)
	}
	if e := h.journal.last(); e.Reason != "macro phrase in the message" {
		t.Errorf("journal keeps the machine reason, got %q", e.Reason)
	}
}

func TestTriagePromptShowsThreadRootAndOwnLastReply(t *testing.T) {
	root := "1789034159.622289"
	msg := slackfetch.Message{
		AuthorID: "U400PEER", AuthorName: "teammate-a", Text: "yes, both were pushed again", ChannelLabel: "#team-a-dev",
		SelfInThread: true, ThreadSize: 40, ThreadTS: root, RootAuthor: "teammate", RootText: "Alex, check both MR when possible",
		ThreadTail: []slackfetch.Reply{
			{TS: "1789034200.000001", AuthorID: "U400PEER", AuthorName: "teammate-a", Text: "the first is ready"},
			{TS: "1789034300.000002", AuthorID: selfID, AuthorName: "Atlas", Text: "were both pushed again after my findings?"},
		},
	}
	h := newHarness(t, msg)
	h.verdicts.verdict = triage.Verdict{React: false, Reason: "no"}

	h.handle(t, foreignNotification("1789054475.628329", root))

	prompt := h.prompts.payloads[0].Prompt
	if !strings.Contains(prompt, "Thread root — teammate: Alex, check both MR") || !strings.Contains(prompt, "The previous reply in the thread is from Alex/Atlas") {
		t.Errorf("triage prompt lacks root or last-reply facts:\n%s", prompt)
	}

	msg.ThreadTail = append([]slackfetch.Reply{{TS: root, AuthorID: "U400PEER", AuthorName: "teammate", Text: "Alex, check both MR when possible"}}, msg.ThreadTail[:1]...)
	h.pipe.fetcher = fakeFetcher{msg: msg}
	h.handle(t, foreignNotification("1789054475.628330", root))
	prompt = h.prompts.payloads[1].Prompt
	if strings.Contains(prompt, "Thread root") || strings.Contains(prompt, "The previous reply") {
		t.Errorf("root already in the tail and a peer's last reply must add no lines:\n%s", prompt)
	}
}

func (f *fakeJournal) sessions() []journal.Entry {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []journal.Entry
	for _, e := range f.entries {
		if e.Action == journal.ActionSession {
			out = append(out, e)
		}
	}
	return out
}

func TestVanishedWindowClosesTheSessionInTheJournal(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	root := "1789047527.174689"
	h.handle(t, notification(root, ""))
	h.sessions.set("sess-1", session.Blocked)
	h.windows.existing = nil
	h.windows.nextID = "@2"
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: peerID, Text: "[HEAD CHANGED] → " + newSHA}}

	h.handle(t, notification("1789049999.000100", root))

	closed := h.journal.sessions()
	if len(closed) != 1 || closed[0].SessionID != "sess-1" || closed[0].Reason != "window_gone" || closed[0].ThreadTS != root || closed[0].Session == nil {
		t.Fatalf("session entries = %+v", closed)
	}
	outs := journal.Outcomes(h.journal.entries, h.pipe.now(), time.Hour)
	if len(outs) != 2 || outs[0].Status != journal.StatusAbandoned || outs[1].Status != journal.StatusOpen {
		t.Errorf("outcomes = %+v", outs)
	}
}

func TestSessionAlreadyEndedByHookIsNotClosedTwice(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	root := "1789047527.174689"
	h.handle(t, notification(root, ""))
	h.sessions.set("sess-1", session.Ended)
	h.windows.existing = nil
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: peerID, Text: "okay, thanks"}}

	h.handle(t, notification("1789049999.000100", root))

	if closed := h.journal.sessions(); len(closed) != 0 {
		t.Errorf("hook already recorded the end, got %+v", closed)
	}
}

func TestStopPhraseAndSweepCloseSessions(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	root := "1789047527.174689"
	h.handle(t, notification(root, ""))
	h.liveWindow("@1", "rev/PRJ-8866")
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: peerID, Text: "Atlas, stop"}}
	h.handle(t, notification("1789049999.000100", root))
	if closed := h.journal.sessions(); len(closed) != 1 || closed[0].Reason != "stop_phrase" {
		t.Fatalf("stop phrase must close the session: %+v", closed)
	}

	h.windows.nextID = "@5"
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: peerID, Text: handoff}}
	h.handle(t, notification("1789050000.000001", ""))
	h.windows.existing = nil
	if err := h.store.UpdateThread("1789050000.000001", func(th *state.Thread) error {
		th.UpdatedAt = h.pipe.now().Add(-60 * 24 * time.Hour)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.pipe.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	closed := h.journal.sessions()
	if len(closed) != 2 || closed[1].Reason != "swept" || closed[1].SessionID != "sess-2" {
		t.Errorf("sweep must close the second session: %+v", closed)
	}
}

func TestCommitteeMemberMarkersAreTaggedAndJointVerdictClosesTheThread(t *testing.T) {
	h := committeeHarness(t, "claude", "codex")
	h.windows.nextIDs = []string{"@1", "@2"}
	root := "1789049440.051109"
	h.handle(t, notification(root, ""))
	h.windows.mu.Lock()
	h.windows.existing = []launcher.Window{{ID: "@1", Command: "2.1.267", Occupancy: launcher.OccupancyAgent}, {ID: "@2", Command: "2.1.267", Occupancy: launcher.OccupancyAgent}}
	h.windows.mu.Unlock()
	h.worktrees.synced = worktree.Result{Path: worktreePath, SHA: newSHA, HeadSHA: newSHA}

	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: selfID, Text: "🤖 Atlas: [REVIEW DONE] " + newSHA + " (codex) — blockers 1, other 2, findings in the MR: https://gitlab.example.com/x"}}
	h.handle(t, notification("1789049999.000100", root))

	th, _ := h.store.Thread(root)
	if th.Progress != "" || len(th.Members) != 1 || th.Members[0].Progress != "review_done" || th.Members[0].ProgressSHA != newSHA {
		t.Errorf("thread after member verdict = %+v", th)
	}
	if e := h.journal.last(); e.Action != journal.ActionProgress || e.Agent != "codex" {
		t.Errorf("entry = %+v", e)
	}

	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: selfID, Text: "🤖 Atlas: [REVIEW DONE] " + newSHA + " — joint: overlap 1, disagreements 1"}}
	h.handle(t, notification("1789049999.000200", root))

	th, _ = h.store.Thread(root)
	if th.Progress != "review_done" || th.ProgressSHA != newSHA {
		t.Errorf("thread after joint verdict = %+v", th)
	}
	if e := h.journal.last(); e.Agent != "" {
		t.Errorf("joint verdict must not be tagged: %+v", e)
	}
	outs := journal.Outcomes(h.journal.entries, h.pipe.now(), time.Hour)
	if len(outs) != 1 || outs[0].Status != journal.StatusDone {
		t.Errorf("outcomes = %+v", outs)
	}
}

func TestPostBinCarriesTheConfigPath(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})

	if got := h.pipe.postBin(); got != "handoffd" {
		t.Errorf("postBin = %q, want the bare binary when the daemon runs on the default config", got)
	}

	h.pipe.configPath = "/Users/developer/Library/Application Support/handoffd/config.toml"

	want := `handoffd -config '/Users/developer/Library/Application Support/handoffd/config.toml'`
	if got := h.pipe.postBin(); got != want {
		t.Errorf("postBin = %q, want %q", got, want)
	}
}

func TestEnqueueRefusesWhenTheInboxIsNotWritten(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	h.inbox.addErr = errors.New("no space left on device")

	if h.pipe.Enqueue(context.Background(), notification("1789049999.000100", "")) {
		t.Fatal("a notification with no durable copy must not be admitted")
	}
	if len(h.pipe.queue) != 0 {
		t.Errorf("queue holds %d notifications, want none", len(h.pipe.queue))
	}
	if h.inbox.count() != 0 {
		t.Errorf("inbox holds %d records, want none", h.inbox.count())
	}
}

func TestEnqueueAdmitsOnceTheInboxIsWritten(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})

	if !h.pipe.Enqueue(context.Background(), notification("1789049999.000100", "")) {
		t.Fatal("a durably recorded notification must be admitted")
	}
	if len(h.pipe.queue) != 1 || h.inbox.count() != 1 {
		t.Errorf("queue = %d inbox = %d, want one each", len(h.pipe.queue), h.inbox.count())
	}
}

func TestFailedNotificationKeepsItsInboxRecordWhenTheDeadLetterFails(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	n := notification("1789049999.000100", "")
	if !h.pipe.Enqueue(context.Background(), n) {
		t.Fatal("Enqueue")
	}
	h.pipe.fetcher = fakeFetcher{err: errors.New("slack is down")}
	h.dead.addErr = errors.New("no space left on device")

	err := h.pipe.Handle(context.Background(), n)

	if err == nil || !strings.Contains(err.Error(), "no space left on device") {
		t.Fatalf("Handle err = %v, want the dead letter failure reported", err)
	}
	if h.inbox.count() != 1 {
		t.Errorf("inbox holds %d records, want the only recoverable copy kept", h.inbox.count())
	}
	if h.store.Seen(n.ID) {
		t.Error("a notification with no durable copy must not be acked as seen")
	}
}

func TestFailedNotificationIsBuriedBeforeItIsAcked(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	n := notification("1789049999.000100", "")
	if !h.pipe.Enqueue(context.Background(), n) {
		t.Fatal("Enqueue")
	}
	h.pipe.fetcher = fakeFetcher{err: errors.New("slack is down")}

	if err := h.pipe.Handle(context.Background(), n); err == nil {
		t.Fatal("Handle must report the exhausted retries")
	}
	if h.dead.count() != 1 {
		t.Errorf("dead letters = %d, want the notification recoverable", h.dead.count())
	}
	if h.inbox.count() != 0 || !h.store.Seen(n.ID) {
		t.Errorf("inbox = %d seen = %v, want the record acked after it was buried", h.inbox.count(), h.store.Seen(n.ID))
	}
}

func TestReviewWithoutAnMRLinkIsRefused(t *testing.T) {
	h := newHarness(t, slackfetch.Message{
		AuthorID: peerID,
		Text:     "[HEAD CHANGED] <@U100SELF> → 4a8259ce0aa1b2c3d4e5f60718293a4b5c6d7e8f",
	})
	root := "1789047527.174689"

	h.handle(t, notification(root, ""))

	e := h.journal.last()
	if e.Action != journal.ActionDropped || !strings.Contains(e.Reason, "no MR link") {
		t.Fatalf("entry = %+v, want the review refused", e)
	}
	if len(h.windows.created) != 0 {
		t.Errorf("windows = %+v, a review with no exact revision must not start", h.windows.created)
	}
	if _, ok := h.store.Thread(root); ok {
		t.Error("a refused review must not leave a thread with an empty head")
	}
	if h.dead.count() != 1 {
		t.Errorf("dead letters = %d, want the refusal recoverable", h.dead.count())
	}
}

func TestSeenThatCouldNotBePersistedKeepsTheInboxRecord(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	n := notification("1789049999.000100", "")
	if !h.pipe.Enqueue(context.Background(), n) {
		t.Fatal("Enqueue")
	}
	blockStateWrites(t, h.cfg.StatePath)

	_ = h.pipe.Handle(context.Background(), n)

	if h.store.Seen(n.ID) {
		t.Error("a seen mark that could not be persisted must not be visible in memory")
	}
	if h.inbox.count() != 1 {
		t.Fatalf("inbox = %d, the only durable copy must survive", h.inbox.count())
	}

	_ = h.pipe.Handle(context.Background(), n)

	if h.inbox.count() != 1 {
		t.Errorf("inbox = %d, a redelivery must not drop the only durable copy", h.inbox.count())
	}
}

func TestReplayReleasesTheIDWhenAdmissionIsRefused(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	n := notification("1789049999.000100", "")
	if err := h.dead.Add(n, "slack is down", h.pipe.now()); err != nil {
		t.Fatal(err)
	}
	h.inbox.addErr = errors.New("no space left on device")

	queued, err := h.pipe.Replay(context.Background())

	if queued != 0 || err == nil {
		t.Fatalf("Replay = (%d, %v), want the refused admission reported", queued, err)
	}

	h.inbox.addErr = nil
	if queued, err := h.pipe.Replay(context.Background()); queued != 1 || err != nil {
		t.Errorf("second Replay = (%d, %v), want the dead letter replayable again", queued, err)
	}
}

func TestDroppedTriageKeepsItsInboxRecordWhenTheDeadLetterFails(t *testing.T) {
	h := newHarness(t, triageMessage())
	h.verdicts.err = context.DeadlineExceeded
	n := foreignNotification("1789054475.628329", "")
	if !h.pipe.Enqueue(context.Background(), n) {
		t.Fatal("Enqueue")
	}
	h.dead.addErr = errors.New("no space left on device")

	h.handle(t, n)

	if h.inbox.count() != 1 {
		t.Errorf("inbox = %d, a dropped triage with no dead letter must keep its only durable copy", h.inbox.count())
	}
	if h.store.Seen(n.ID) {
		t.Error("a notification that could not be buried must not be acked as seen")
	}
}

func TestDroppedTriageIsAckedOnceItIsBuried(t *testing.T) {
	h := newHarness(t, triageMessage())
	h.verdicts.err = context.DeadlineExceeded
	n := foreignNotification("1789054475.628329", "")
	if !h.pipe.Enqueue(context.Background(), n) {
		t.Fatal("Enqueue")
	}

	h.handle(t, n)

	if h.dead.count() != 1 {
		t.Fatalf("dead letters = %d, want the dropped triage recoverable", h.dead.count())
	}
	if h.inbox.count() != 0 || !h.store.Seen(n.ID) {
		t.Errorf("inbox = %d seen = %v, want the record acked after it was buried", h.inbox.count(), h.store.Seen(n.ID))
	}
}

func TestAStopThatCannotCloseTheWindowKeepsTheThreadUnderControl(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, Text: handoff})
	root := "1789047527.174689"
	h.handle(t, notification(root, ""))
	h.liveWindow("@1", "rev/PRJ-8866")
	h.windows.mu.Lock()
	h.windows.killErr = errors.New("tmux: can't find window @1")
	h.windows.mu.Unlock()
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: selfID, Text: "Atlas, stop — I will finish checking"}}

	err := h.pipe.Handle(context.Background(), notification("1789049999.000100", root))
	h.pipe.Wait()

	if err == nil {
		t.Fatal("a stop that left the agent running must not report success")
	}
	th, ok := h.store.Thread(root)
	if !ok || th.WindowGone() || th.SessionID == "" {
		t.Errorf("thread = %+v ok=%v, want the live window still owned", th, ok)
	}
	for _, e := range h.journal.entries {
		if e.Action == journal.ActionFeedback && strings.Contains(e.Reason, "closed") {
			t.Errorf("entry = %+v, the window was not closed", e)
		}
	}
}

func TestAVerdictTakenAsTheWindowDiedIsNotThrownAway(t *testing.T) {
	h := newHarness(t, slackfetch.Message{})
	h.verdicts.block = true
	h.verdicts.taken = true
	h.verdicts.verdict = triage.Verdict{React: true, Task: "review MR"}

	got, err := h.pipe.awaitVerdict(context.Background(), "@9", "/state/verdicts/v.json")

	if err != nil {
		t.Fatalf("awaitVerdict: %v", err)
	}
	if !got.React || got.Task != "review MR" {
		t.Errorf("verdict = %+v, want the one the waiter had already taken off disk", got)
	}
}

func TestADeadLetterIsReplayableAsSoonAsItIsWritten(t *testing.T) {
	h := newHarness(t, triageMessage())
	h.verdicts.err = context.DeadlineExceeded
	h.handle(t, foreignNotification("1789054475.628329", ""))
	h.pipe.fetcher = fakeFetcher{err: errors.New("slack unavailable")}
	written := h.dead.added()
	var queuedAgain int
	var replayErr error
	h.dead.onAdd = func() {
		queuedAgain, replayErr = h.pipe.Replay(context.Background())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.pipe.Work(ctx)

	if n, err := h.pipe.Replay(ctx); err != nil || n != 1 {
		t.Fatalf("Replay = %d, %v", n, err)
	}
	waitFor(t, "the failed replay written back", func() bool { return h.dead.added() > written })

	if queuedAgain != 1 {
		t.Errorf("replay while the dead letter was being written queued %d (%v), a written dead letter must be replayable",
			queuedAgain, replayErr)
	}
}

func TestAnEscalationThatCouldNotOpenAWindowKeepsTheMessageRecoverable(t *testing.T) {
	h := newHarness(t, triageMessage())
	alerts := &fakeAlerts{}
	h.pipe.alerts = alerts
	h.verdicts.verdict = triage.Verdict{React: true, Reason: "asked to check", Task: "review"}
	h.windows.failAfter = 1

	if err := h.pipe.Handle(context.Background(), foreignNotification("1789054475.628329", "")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	h.pipe.Wait()

	if h.dead.count() != 1 {
		t.Fatalf("dead letters = %d, a message nobody escalated must stay recoverable", h.dead.count())
	}
	if !strings.Contains(h.dead.records[0].Reason, "escalation") {
		t.Errorf("dead letter = %+v", h.dead.records[0])
	}
	if alerts.count() == 0 {
		t.Error("a failed escalation must reach a human")
	}
	var errored bool
	for _, e := range h.journal.entries {
		if e.Action == journal.ActionError {
			errored = true
		}
	}
	if !errored {
		t.Error("the journal must show the escalation failed")
	}
}

func TestAShutdownDuringTriageDoesNotFakeADrop(t *testing.T) {
	h := newHarness(t, triageMessage())
	alerts := &fakeAlerts{}
	h.pipe.alerts = alerts
	h.verdicts.block = true
	ctx, cancel := context.WithCancel(context.Background())

	if err := h.pipe.Handle(ctx, foreignNotification("1789054475.628329", "")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	cancel()
	h.pipe.Wait()

	if h.dead.count() != 0 {
		t.Errorf("dead letters = %+v, a shutdown is not a verdict", h.dead.records)
	}
	if alerts.count() != 0 {
		t.Errorf("alerts = %d, nothing went wrong, the watcher is stopping", alerts.count())
	}
	for _, e := range h.journal.entries {
		if e.Action == journal.ActionDropped {
			t.Errorf("entry = %+v, a message the watcher never judged must not be recorded as dropped", e)
		}
	}
	if len(h.windows.killed) == 0 {
		t.Error("the triage window must still be closed while the watcher stops")
	}
}
