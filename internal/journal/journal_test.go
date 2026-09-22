package journal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var base = time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)

func entry(at time.Time, action string) Entry {
	return Entry{At: at, ID: "T1_" + at.Format("150405"), Source: SourceNotification, Channel: "C1", MsgTS: "1.1", Kind: "triage", Action: action}
}

func TestAppendThenReadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "decisions.jsonl")
	w := NewWriter(path)
	e := entry(base, ActionEscalated)
	e.Triage = &Triage{React: true, Reason: "asked for review", Task: "review it", LatencyMS: 21000, Outcome: OutcomeVerdict}
	e.Text = "Alex, check both"

	if err := w.Append(e); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := Read(path, time.Time{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if len(got) != 1 || got[0].ID != e.ID || got[0].Triage == nil || got[0].Triage.LatencyMS != 21000 || got[0].Text != e.Text {
		t.Errorf("entries = %+v", got)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("journal must be private, mode = %v", info.Mode().Perm())
	}
}

func TestReadFiltersBySinceAndSkipsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "decisions.jsonl")
	w := NewWriter(path)
	for _, e := range []Entry{entry(base, ActionSilent), entry(base.Add(time.Hour), ActionOpened)} {
		if err := w.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString("not json\n")
	f.Close()

	got, err := Read(path, base.Add(30*time.Minute))

	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 1 || got[0].Action != ActionOpened {
		t.Errorf("entries = %+v", got)
	}
}

func TestReadMissingFileIsEmpty(t *testing.T) {
	got, err := Read(filepath.Join(t.TempDir(), "absent.jsonl"), time.Time{})
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v", got, err)
	}
}

func TestSummarizeCountsActionsAndTriage(t *testing.T) {
	entries := []Entry{
		entry(base, ActionOpened),
		entry(base, ActionContinued),
		entry(base, ActionSkipped),
		entry(base, ActionIgnored),
		entry(base, ActionDropped),
		{At: base, Kind: "triage", Action: ActionSilent, Triage: &Triage{LatencyMS: 10000, Outcome: OutcomeVerdict}},
		{At: base, Kind: "triage", Action: ActionSilent, Triage: &Triage{LatencyMS: 20000, Outcome: OutcomeVerdict}},
		{At: base, Kind: "triage", Action: ActionEscalated, Triage: &Triage{React: true, LatencyMS: 30000, Outcome: OutcomePane}},
		{At: base, Kind: "triage", Action: ActionDropped, Triage: &Triage{LatencyMS: 150000, Outcome: OutcomeTimeout}},
	}

	s := Summarize(entries)

	if s.Total != 9 || s.Actions[ActionSilent] != 2 || s.Actions[ActionDropped] != 2 {
		t.Errorf("summary = %+v", s)
	}
	if s.TriageRuns != 4 || s.TriageReact != 1 || s.TriageNoVerdict != 1 {
		t.Errorf("triage = runs %d react %d noverdict %d", s.TriageRuns, s.TriageReact, s.TriageNoVerdict)
	}
	if s.TriageLatencyMedianMS != 20000 || s.TriageLatencyP90MS != 150000 {
		t.Errorf("latency median %d p90 %d", s.TriageLatencyMedianMS, s.TriageLatencyP90MS)
	}
	text := s.String()
	for _, want := range []string{"total 9", "triage runs 4", "react 1", "no verdict 1", "median 20s", "p90 150s", "skipped 1", "dropped 2"} {
		if !strings.Contains(text, want) {
			t.Errorf("summary text missing %q:\n%s", want, text)
		}
	}
}

func TestSummarizeEmpty(t *testing.T) {
	s := Summarize(nil)
	if s.Total != 0 || s.TriageLatencyMedianMS != 0 || !strings.Contains(s.String(), "total 0") {
		t.Errorf("summary = %+v", s)
	}
}

func TestNeedsAttentionListsLossesOnly(t *testing.T) {
	entries := []Entry{
		entry(base, ActionOpened),
		entry(base, ActionSkipped),
		entry(base, ActionDropped),
		entry(base, ActionError),
		entry(base, ActionIgnored),
	}

	got := NeedsAttention(entries)

	if len(got) != 3 {
		t.Errorf("attention = %+v", got)
	}
}

func TestCostCountsTriagesAndOpenedWindowsOnlyWhenPriced(t *testing.T) {
	triaged := entry(base, ActionEscalated)
	triaged.Triage = &Triage{React: true, Outcome: OutcomeVerdict}
	silent := entry(base, ActionSilent)
	silent.Triage = &Triage{Outcome: OutcomeVerdict}
	s := Summarize([]Entry{entry(base, ActionOpened), entry(base, ActionContinued), triaged, silent, entry(base, ActionIgnored)})

	cost, ok := s.Cost(0.02, 1.5)
	if !ok || cost.Triages != 2 || cost.Windows != 2 || cost.USD != 3.04 {
		t.Errorf("cost = %+v ok=%v", cost, ok)
	}
	if got := cost.String(); got != "est. cost $3.04 (2 triages, 2 windows)" {
		t.Errorf("cost string = %q", got)
	}
	if _, ok := s.Cost(0, 0); ok {
		t.Error("without prices there is no estimate")
	}
}
