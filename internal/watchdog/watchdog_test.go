package watchdog

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kriso1337/handoffd/internal/launcher"
	"github.com/Kriso1337/handoffd/internal/session"
	"github.com/Kriso1337/handoffd/internal/state"
)

var now = time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

type fakeWindows struct{ list []launcher.Window }

func (f fakeWindows) Windows(context.Context) ([]launcher.Window, error) { return f.list, nil }

type fakeSessions map[string]session.Record

func (f fakeSessions) Read(id string) (session.Record, bool, error) {
	rec, ok := f[id]
	return rec, ok, nil
}

type fakeAlerts struct{ sent []string }

func (f *fakeAlerts) Alert(_ context.Context, key, _, text string) bool {
	f.sent = append(f.sent, key+" "+text)
	return true
}

func TestCheckAlertsOnLongBlockedSessionsAndStaleLog(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"), 10)
	if err != nil {
		t.Fatal(err)
	}
	store.SetThread("r1", state.Thread{WindowID: "@1", WindowName: "rev/PRJ-1", SessionID: "s-blocked"})
	store.SetThread("r2", state.Thread{WindowID: "@2", WindowName: "rev/PRJ-2", SessionID: "s-fresh"})
	store.SetThread("r3", state.Thread{WindowID: "@3", WindowName: "help/x", SessionID: "s-dead-window"})
	store.SetThread("r4", state.Thread{WindowID: "@4", WindowName: "help/y", SessionID: "s-working"})
	sessions := fakeSessions{
		"s-blocked":     {State: session.Blocked, Detail: "Bash", UpdatedAt: now.Add(-16 * time.Minute)},
		"s-fresh":       {State: session.Blocked, UpdatedAt: now.Add(-3 * time.Minute)},
		"s-dead-window": {State: session.Blocked, UpdatedAt: now.Add(-time.Hour)},
		"s-working":     {State: session.Working, UpdatedAt: now.Add(-time.Hour)},
	}
	windows := fakeWindows{list: []launcher.Window{{ID: "@1", Command: "2.1.267", Occupancy: launcher.OccupancyAgent}, {ID: "@2", Command: "2.1.267", Occupancy: launcher.OccupancyAgent}, {ID: "@3", Dead: true, Occupancy: launcher.OccupancyFree}, {ID: "@4", Command: "2.1.267", Occupancy: launcher.OccupancyAgent}}}
	logPath := filepath.Join(t.TempDir(), "webapp-console.log")
	if err := os.WriteFile(logPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	staleAt := now.Add(-3 * time.Hour)
	if err := os.Chtimes(logPath, staleAt, staleAt); err != nil {
		t.Fatal(err)
	}
	alerts := &fakeAlerts{}
	w := New(store, windows, sessions, alerts, Options{LogPath: logPath, StaleAfter: 2 * time.Hour, BlockedAfter: 15 * time.Minute, Interval: time.Minute},
		func() time.Time { return now }, slog.New(slog.NewTextHandler(io.Discard, nil)))

	fired := w.Check(context.Background())

	if len(fired) != 2 {
		t.Fatalf("fired = %v, want the stale log and the one long-blocked live session", alerts.sent)
	}
	if !strings.HasPrefix(fired[0], "stale-log:") || !strings.Contains(alerts.sent[0], "3h0m0s") {
		t.Errorf("stale alert = %q", alerts.sent[0])
	}
	if !strings.HasPrefix(fired[1], "blocked:s-blocked:") || !strings.Contains(alerts.sent[1], "rev/PRJ-1 waits for a reply 16m0s (Bash)") {
		t.Errorf("blocked alert = %q", alerts.sent[1])
	}
}

func TestCheckStaysQuietWhenEverythingIsHealthyOrDisabled(t *testing.T) {
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"), 10)
	store.SetThread("r1", state.Thread{WindowID: "@1", WindowName: "rev/PRJ-1", SessionID: "s1"})
	sessions := fakeSessions{"s1": {State: session.Blocked, UpdatedAt: now.Add(-time.Hour)}}
	logPath := filepath.Join(t.TempDir(), "webapp-console.log")
	os.WriteFile(logPath, []byte("x"), 0o600)
	os.Chtimes(logPath, now.Add(-time.Minute), now.Add(-time.Minute))
	alerts := &fakeAlerts{}
	w := New(store, fakeWindows{list: []launcher.Window{{ID: "@1", Command: "2.1.267", Occupancy: launcher.OccupancyAgent}}}, sessions, alerts,
		Options{LogPath: logPath, StaleAfter: 2 * time.Hour, BlockedAfter: 0, Interval: 0}, func() time.Time { return now }, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if fired := w.Check(context.Background()); len(fired) != 0 {
		t.Errorf("fired = %v with blocked alerts disabled and a fresh log", fired)
	}
	done := make(chan struct{})
	go func() { w.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watchdog without an interval must return at once")
	}
}

func TestCheckAlertsWhenARoundIsCollectedWithoutAJointVerdict(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"), 10)
	if err != nil {
		t.Fatal(err)
	}
	collected := &state.Run{Round: 2, Results: map[string]state.Result{"codex": {SHA: "abc", At: now.Add(-20 * time.Minute)}}}
	store.SetThread("r1", state.Thread{WindowID: "@1", WindowName: "rev/PRJ-1", Members: []state.Member{{Agent: "codex"}}, Run: collected})
	fresh := &state.Run{Round: 1, Results: map[string]state.Result{"codex": {SHA: "abc", At: now.Add(-2 * time.Minute)}}}
	store.SetThread("r2", state.Thread{WindowID: "@2", WindowName: "rev/PRJ-2", Members: []state.Member{{Agent: "codex"}}, Run: fresh})
	waiting := &state.Run{Round: 1, Results: map[string]state.Result{}}
	store.SetThread("r3", state.Thread{WindowID: "@3", WindowName: "rev/PRJ-3", Members: []state.Member{{Agent: "codex"}}, Run: waiting})
	closed := &state.Run{Round: 1, Results: map[string]state.Result{"codex": {At: now.Add(-time.Hour)}}, Joint: &state.Joint{SHA: "abc"}}
	store.SetThread("r4", state.Thread{WindowID: "@4", WindowName: "rev/PRJ-4", Members: []state.Member{{Agent: "codex"}}, Run: closed})
	alerts := &fakeAlerts{}
	w := New(store, fakeWindows{}, fakeSessions{}, alerts, Options{CollectedAfter: 15 * time.Minute, Interval: time.Minute}, func() time.Time { return now }, slog.New(slog.NewTextHandler(io.Discard, nil)))

	fired := w.Check(context.Background())

	if len(fired) != 1 || fired[0] != "collected:r1:2" {
		t.Fatalf("fired = %v", fired)
	}
	if !strings.Contains(alerts.sent[0], "rev/PRJ-1: round 2 collected 20m0s ago") {
		t.Errorf("alert = %q", alerts.sent[0])
	}
}
