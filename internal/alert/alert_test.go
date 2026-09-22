package alert

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type fakeCmd struct {
	calls [][]string
	err   error
}

func (f *fakeCmd) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	return nil, f.err
}

var base = time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

func notifier(cmd *fakeCmd, argv []string, now *time.Time) *Notifier {
	return New(cmd, argv, time.Hour, func() time.Time { return *now }, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestAlertSubstitutesPlaceholdersAndRespectsCooldownPerKey(t *testing.T) {
	cmd := &fakeCmd{}
	now := base
	n := notifier(cmd, DefaultCommand("darwin"), &now)

	if !n.Alert(context.Background(), "blocked:s1", "handoffd", "rev/PRJ-1 waits for a reply 16m") {
		t.Fatal("first alert must go out")
	}
	if n.Alert(context.Background(), "blocked:s1", "handoffd", "again") {
		t.Error("same key within the cooldown must be suppressed")
	}
	if !n.Alert(context.Background(), "deadletter", "handoffd", "dead letter") {
		t.Error("a different key is independent")
	}
	now = base.Add(61 * time.Minute)
	if !n.Alert(context.Background(), "blocked:s1", "handoffd", "still") {
		t.Error("after the cooldown the key fires again")
	}

	first := strings.Join(cmd.calls[0], " ")
	if !strings.HasPrefix(first, "/usr/bin/osascript -e on run argv") || !strings.HasSuffix(first, "end run handoffd rev/PRJ-1 waits for a reply 16m") {
		t.Errorf("argv = %q", first)
	}
}

func TestAlertIsQuietWhenDisabledOrFailing(t *testing.T) {
	now := base
	if notifier(&fakeCmd{}, nil, &now).Alert(context.Background(), "k", "t", "x") {
		t.Error("no command means no alert")
	}
	var nilNotifier *Notifier
	if nilNotifier.Alert(context.Background(), "k", "t", "x") {
		t.Error("nil notifier must be safe")
	}
	if notifier(&fakeCmd{err: errors.New("no display")}, []string{"notify-send", "{title}", "{text}"}, &now).Alert(context.Background(), "k", "t", "x") {
		t.Error("a failing command reports false")
	}
	if DefaultCommand("plan9") != nil {
		t.Error("unknown platforms start without an alert command")
	}
}
