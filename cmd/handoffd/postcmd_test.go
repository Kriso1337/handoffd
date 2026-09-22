package main

import (
	"strings"
	"testing"
	"time"

	"github.com/Kriso1337/handoffd/internal/outbox"
)

var postNow = time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)

func TestParsePostBuildsEachKind(t *testing.T) {
	cases := []struct {
		args []string
		want outbox.Record
	}{
		{[]string{"--thread", "1.1", "hello", "there"}, outbox.Record{Kind: outbox.KindReply, Text: "hello there"}},
		{[]string{"--thread", "1.1", "--mr-note", "--sha", "abc1234", "app.go:12", "nil"}, outbox.Record{Kind: outbox.KindMRNote, SHA: "abc1234", Text: "app.go:12 nil"}},
		{[]string{"--thread", "1.1", "--marker", "taken", "--sha", "abc1234"}, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseTaken, SHA: "abc1234"}},
		{[]string{"--thread", "1.1", "--marker", "review-start", "--sha", "abc1234"}, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseReviewStart, SHA: "abc1234"}},
		{[]string{"--thread", "1.1", "--review-done", "--sha", "abc1234", "--blockers", "2", "--others", "3", "--decision", "rollback"}, outbox.Record{Kind: outbox.KindReviewDone, SHA: "abc1234", Blockers: 2, Others: 3, Decision: "rollback"}},
	}
	for _, c := range cases {
		opts, err := parsePost(c.args, "sess-1", postNow)
		if err != nil {
			t.Errorf("%v: %v", c.args, err)
			continue
		}
		got := opts.record
		if got.ID == "" || got.ThreadTS != "1.1" || got.SessionID != "sess-1" || !got.At.Equal(postNow) {
			t.Errorf("%v: envelope = %+v", c.args, got)
		}
		got.ID, got.ThreadTS, got.SessionID, got.At = "", "", "", time.Time{}
		if got != c.want {
			t.Errorf("%v: record = %+v, want %+v", c.args, got, c.want)
		}
		if opts.wait != postWaitDefault {
			t.Errorf("%v: wait = %s", c.args, opts.wait)
		}
	}
}

func TestParsePostRefusesAmbiguousOrIncompleteInput(t *testing.T) {
	cases := map[string][]string{
		"two kinds":        {"--thread", "1.1", "--mr-note", "--review-done", "--sha", "abc1234", "x"},
		"unknown marker":   {"--thread", "1.1", "--marker", "done", "--sha", "abc1234"},
		"note without sha": {"--thread", "1.1", "--mr-note", "x"},
		"no thread":        {"hello"},
		"empty reply":      {"--thread", "1.1"},
	}
	for name, args := range cases {
		if _, err := parsePost(args, "sess-1", postNow); err == nil {
			t.Errorf("%s: accepted %v", name, args)
		}
	}
	if _, err := parsePost([]string{"--thread", "1.1", "hi"}, "", postNow); err == nil || !strings.Contains(err.Error(), "HANDOFFD_SESSION_ID") {
		t.Errorf("missing session must name the variable, got %v", err)
	}
	if opts, err := parsePost([]string{"--thread", "1.1", "--wait", "5s", "hi"}, "sess-1", postNow); err != nil || opts.wait != 5*time.Second {
		t.Errorf("wait flag = %+v %v", opts, err)
	}
}
