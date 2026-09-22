package threadpoll

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Kriso1337/handoffd/internal/notifylog"
	"github.com/Kriso1337/handoffd/internal/slackfetch"
	"github.com/Kriso1337/handoffd/internal/state"
)

var now = time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

type fakeLister struct {
	mu      sync.Mutex
	replies map[string][]slackfetch.Reply
	calls   []string
	err     error
}

func (f *fakeLister) Replies(_ context.Context, channel, threadTS, since string) ([]slackfetch.Reply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, channel+"/"+threadTS+"@"+since)
	if f.err != nil {
		return nil, f.err
	}
	return f.replies[threadTS], nil
}

type fakeSink struct {
	mu    sync.Mutex
	items []notifylog.Notification
	limit int
}

func (f *fakeSink) Enqueue(_ context.Context, n notifylog.Notification) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.limit > 0 && len(f.items) >= f.limit {
		return false
	}
	f.items = append(f.items, n)
	return true
}

func openStore(t *testing.T) *state.Store {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"), 100)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func poller(store *state.Store, lister *fakeLister, sink *fakeSink) *Poller {
	return New(store, lister, sink, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return now }, 30*time.Second, 72*time.Hour, 10)
}

func TestPollQueuesNewRepliesOfRememberedThreadsOnly(t *testing.T) {
	store := openStore(t)
	store.SetThread("1789000000.000001", state.Thread{WindowID: "@1", Channel: "C1", TeamID: "T1", LastSeenTS: "1789000100.000000", UpdatedAt: now.Add(-time.Hour)})
	store.SetThread("1789000000.000002", state.Thread{Channel: "C1", UpdatedAt: now.Add(-2 * time.Hour)})
	store.SetThread("1789000000.000003", state.Thread{WindowID: "@3", Channel: "C1", TeamID: "T1", UpdatedAt: now.Add(-100 * time.Hour)})
	store.SetThread("1789000000.000004", state.Thread{WindowID: "@4", TeamID: "T1", UpdatedAt: now})
	lister := &fakeLister{replies: map[string][]slackfetch.Reply{
		"1789000000.000001": {{TS: "1789000050.000000"}, {TS: "1789000200.000000", AuthorID: "U2"}, {TS: "1789000300.000000", AuthorID: "U3"}},
		"1789000000.000002": {{TS: "1789200000.000000", AuthorID: "U2"}},
	}}
	sink := &fakeSink{}
	if err := store.CommitSeen("T1_1789000200.000000"); err != nil {
		t.Fatal(err)
	}

	queued := poller(store, lister, sink).Poll(context.Background())

	if queued != 1 || len(sink.items) != 1 {
		t.Fatalf("queued = %d items = %+v", queued, sink.items)
	}
	got := sink.items[0]
	if got.ID != "T1_1789000300.000000" || got.ThreadTS != "1789000000.000001" || got.Channel != "C1" || got.Source != notifylog.SourceThreadPoll {
		t.Errorf("notification = %+v", got)
	}
	if len(lister.calls) != 2 {
		t.Errorf("calls = %v, want the two recent threads with a channel", lister.calls)
	}
	th, _ := store.Thread("1789000000.000001")
	if th.LastSeenTS != "1789000300.000000" {
		t.Errorf("last seen = %q, want the newest reply", th.LastSeenTS)
	}
	legacy, _ := store.Thread("1789000000.000002")
	if legacy.LastSeenTS != "1789214400.000000" {
		t.Errorf("thread without a cursor must start from now, got %q", legacy.LastSeenTS)
	}
}

func TestSecondPollStartsFromTheAdvancedCursor(t *testing.T) {
	store := openStore(t)
	store.SetThread("1789000000.000001", state.Thread{WindowID: "@1", Channel: "C1", TeamID: "T1", LastSeenTS: "1789000100.000000", UpdatedAt: now})
	lister := &fakeLister{replies: map[string][]slackfetch.Reply{"1789000000.000001": {{TS: "1789000200.000000"}}}}
	sink := &fakeSink{}
	p := poller(store, lister, sink)

	p.Poll(context.Background())
	p.Poll(context.Background())

	if len(sink.items) != 1 {
		t.Errorf("items = %+v, want the reply queued once", sink.items)
	}
	if lister.calls[1] != "C1/1789000000.000001@1789000200.000000" {
		t.Errorf("second call = %q", lister.calls[1])
	}
}

func TestCursorAdvancesOnlyPastAdmittedReplies(t *testing.T) {
	store := openStore(t)
	store.SetThread("1789000000.000001", state.Thread{WindowID: "@1", Channel: "C1", TeamID: "T1", LastSeenTS: "1789000100.000000", UpdatedAt: now})
	lister := &fakeLister{replies: map[string][]slackfetch.Reply{"1789000000.000001": {{TS: "1789000300.000000"}, {TS: "1789000200.000000"}}}}
	sink := &fakeSink{limit: 1}

	queued := poller(store, lister, sink).Poll(context.Background())

	if queued != 1 || len(sink.items) != 1 || sink.items[0].MsgTS != "1789000200.000000" {
		t.Fatalf("queued = %d items = %+v, want the oldest reply first", queued, sink.items)
	}
	if th, _ := store.Thread("1789000000.000001"); th.LastSeenTS != "1789000200.000000" {
		t.Errorf("last seen = %q, the refused reply must stay ahead of the cursor", th.LastSeenTS)
	}
}

func TestPollSurvivesHelperErrorsAndUnknownTeam(t *testing.T) {
	store := openStore(t)
	store.SetThread("1789000000.000001", state.Thread{WindowID: "@1", Channel: "C1", UpdatedAt: now})
	sink := &fakeSink{}

	if got := poller(store, &fakeLister{err: errors.New("boom")}, sink).Poll(context.Background()); got != 0 {
		t.Errorf("queued = %d with an unknown team", got)
	}
	store.SetThread("1789000000.000002", state.Thread{WindowID: "@2", Channel: "C1", TeamID: "T1", UpdatedAt: now})
	if got := poller(store, &fakeLister{err: errors.New("boom")}, sink).Poll(context.Background()); got != 0 {
		t.Errorf("queued = %d after helper errors", got)
	}
	lister := &fakeLister{replies: map[string][]slackfetch.Reply{"1789000000.000001": {{TS: "1789999999.000000"}}}}
	if got := poller(store, lister, sink).Poll(context.Background()); got != 1 || sink.items[0].TeamID != "T1" {
		t.Errorf("team learnt from a sibling thread must be used: queued=%d items=%+v", got, sink.items)
	}
}

func TestRunStopsWithContextAndHonoursDisabledInterval(t *testing.T) {
	store := openStore(t)
	p := New(store, &fakeLister{}, &fakeSink{}, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now, 0, time.Hour, 1)
	done := make(chan struct{})
	go func() { p.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disabled poller must return immediately")
	}

	ctx, cancel := context.WithCancel(context.Background())
	p = New(store, &fakeLister{}, &fakeSink{}, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now, time.Millisecond, time.Hour, 1)
	finished := make(chan struct{})
	go func() { p.Run(ctx); close(finished) }()
	time.Sleep(5 * time.Millisecond)
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("poller must stop on context cancel")
	}
}
