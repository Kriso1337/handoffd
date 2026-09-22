package label

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kriso1337/handoffd/internal/journal"
)

var base = time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

func TestAppendRecentKeepsLatestVerdictPerEntryWithText(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "labels.jsonl"))
	must := func(l Label) {
		t.Helper()
		if err := s.Append(l); err != nil {
			t.Fatal(err)
		}
	}
	must(Label{At: base, ID: "T1_1.1", Verdict: Bad, React: false, Text: "weather chatter", Source: SourceManual})
	must(Label{At: base.Add(time.Minute), ID: "T1_1.2", Verdict: Good, React: true, Text: "check MR", Source: SourceManual})
	must(Label{At: base.Add(2 * time.Minute), ID: "T1_1.3", Verdict: Bad, React: false, Source: SourceStopPhrase})
	must(Label{At: base.Add(3 * time.Minute), ID: "T1_1.1", Verdict: Good, React: true, Text: "weather chatter", Note: "changed my mind", Source: SourceManual})

	recent, err := s.Recent(5)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 2 {
		t.Fatalf("recent = %+v, want two labelled texts", recent)
	}
	if recent[0].ID != "T1_1.2" || recent[1].ID != "T1_1.1" || !recent[1].React || recent[1].Note != "changed my mind" {
		t.Errorf("recent = %+v", recent)
	}
	if one, _ := s.Recent(1); len(one) != 1 || one[0].ID != "T1_1.1" {
		t.Errorf("recent(1) = %+v", one)
	}
	if none, _ := s.Recent(0); none != nil {
		t.Errorf("recent(0) = %+v", none)
	}
}

func TestAppendRejectsUnknownVerdictAndReadTolerantOfGarbage(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "labels.jsonl"))
	if err := s.Append(Label{ID: "x", Verdict: "meh"}); err == nil {
		t.Error("unknown verdict must be rejected")
	}
	if labels, err := Read(s.path); err != nil || len(labels) != 0 {
		t.Errorf("missing file: %v %v", labels, err)
	}
}

func TestTimestampFromRefAcceptsPermalinks(t *testing.T) {
	cases := map[string]string{
		"https://acme.slack.com/archives/D0000000001/p1789156662321049":                           "1789156662.321049",
		"https://acme.slack.com/archives/C1/p1789156662321049?thread_ts=1789156600.000100&cid=C1": "1789156662.321049",
		"1789156662.321049":    "1789156662.321049",
		"T1_1789156662.321049": "T1_1789156662.321049",
	}
	for in, want := range cases {
		if got := TimestampFromRef(in); got != want {
			t.Errorf("%s → %q, want %q", in, got, want)
		}
	}
}

func TestEvaluateSplitsFilterAndTriageDecisions(t *testing.T) {
	entries := []journal.Entry{
		{ID: "a", Action: journal.ActionOpened},
		{ID: "b", Action: journal.ActionIgnored},
		{ID: "c", Action: journal.ActionEscalated, Triage: &journal.Triage{React: true}},
		{ID: "d", Action: journal.ActionSilent, Triage: &journal.Triage{React: false}},
		{ID: "e", Action: journal.ActionSilent, Triage: &journal.Triage{React: false}, Text: "check MR please"},
	}
	labels := []Label{
		{At: base, ID: "a", Verdict: Good, React: true},
		{At: base, ID: "b", Verdict: Bad, React: true},
		{At: base, ID: "c", Verdict: Bad, React: false},
		{At: base, ID: "d", Verdict: Good, React: false},
		{At: base, ID: "e", Verdict: Bad, React: true},
		{At: base, ID: "gone", Verdict: Good, React: true},
	}

	r := Evaluate(labels, entries)

	if r.Labelled != 6 || r.Unmatched != 1 {
		t.Errorf("report = %+v", r)
	}
	if r.Filter != (Confusion{TP: 1, FN: 1}) {
		t.Errorf("filter confusion = %+v", r.Filter)
	}
	if r.Triage != (Confusion{FP: 1, TN: 1, FN: 1}) {
		t.Errorf("triage confusion = %+v", r.Triage)
	}
	if len(r.Misses) != 3 {
		t.Errorf("misses = %+v", r.Misses)
	}
	text := r.String()
	for _, want := range []string{"labels 6 (unmatched 1)", "code filters n=2 precision 1.00 recall 0.50", "triage       n=3 precision 0.00 recall 0.00", "missed reaction", "check MR please"} {
		if !strings.Contains(text, want) {
			t.Errorf("report %q lacks %q", text, want)
		}
	}
}
