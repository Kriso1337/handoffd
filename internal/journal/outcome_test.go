package journal

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func opened(at time.Time, root, session, kind string) Entry {
	return Entry{At: at, ID: "T1_" + root, Source: SourceNotification, Channel: "C1", MsgTS: tsString(at), ThreadTS: root,
		Kind: kind, Action: ActionOpened, WindowName: "rev/" + root, SessionID: session}
}

func progress(at time.Time, root, phase, sha string) Entry {
	ts := tsString(at)
	return Entry{At: at, ID: "T1_" + ts, Source: SourceLastRead, Channel: "C1", MsgTS: ts, ThreadTS: root,
		Kind: "none", Action: ActionProgress, Phase: phase, HeadSHA: sha}
}

func ended(at time.Time, session string) Entry {
	return Entry{At: at, ID: "session_" + session, Source: SourceHook, Kind: "session", Action: ActionSession,
		SessionID: session, Session: &SessionStats{DurationMS: 3_600_000, BlockedMS: 120_000, BlockedCount: 2}}
}

func tsString(at time.Time) string {
	return strconv.FormatInt(at.Unix(), 10) + ".000100"
}

func TestOutcomesCloseWorkItemsByOwnMarkersAndSessionEnd(t *testing.T) {
	handoff := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	done := handoff.Add(41 * time.Minute)
	entries := []Entry{
		opened(handoff, "1789000000.000001", "s-done", "review"),
		progress(handoff.Add(2*time.Minute), "1789000000.000001", PhaseTaken, ""),
		progress(handoff.Add(3*time.Minute), "1789000000.000001", PhaseReviewStart, "08c9fc23a726"),
		progress(done, "1789000000.000001", PhaseReviewDone, "08c9fc23a726"),
		ended(done.Add(time.Minute), "s-done"),
		opened(handoff.Add(time.Hour), "1789000000.000002", "s-abandoned", "review"),
		ended(handoff.Add(90*time.Minute), "s-abandoned"),
		opened(handoff.Add(2*time.Hour), "1789000000.000003", "s-stale", "review"),
		opened(handoff.Add(9*time.Hour), "1789000000.000004", "s-open", "other"),
		progress(handoff.Add(9*time.Hour), "1789999999.000009", PhaseReviewDone, "deadbeef1234"),
	}
	now := handoff.Add(10 * time.Hour)

	outs := Outcomes(entries, now, 6*time.Hour)

	if len(outs) != 4 {
		t.Fatalf("outcomes = %d, want 4 work items", len(outs))
	}
	byRoot := map[string]Outcome{}
	for _, o := range outs {
		byRoot[o.Root] = o
	}
	first := byRoot["1789000000.000001"]
	if first.Status != StatusDone || first.DoneSHA != "08c9fc23a726" || first.TakenAt.IsZero() || first.StartedAt.IsZero() {
		t.Errorf("done item = %+v", first)
	}
	if first.Latency.Round(time.Minute) != 41*time.Minute {
		t.Errorf("latency = %s, want 41m from handoff to [REVIEW DONE]", first.Latency)
	}
	if !first.Ended || first.Stats == nil || first.Stats.BlockedMS != 120_000 {
		t.Errorf("done item must carry session stats: %+v", first)
	}
	if got := byRoot["1789000000.000002"].Status; got != StatusAbandoned {
		t.Errorf("session that ended without DONE = %s, want abandoned", got)
	}
	if got := byRoot["1789000000.000003"].Status; got != StatusStale {
		t.Errorf("item open for 8h = %s, want stale", got)
	}
	if got := byRoot["1789000000.000004"].Status; got != StatusOpen {
		t.Errorf("fresh item = %s, want open", got)
	}

	summary := SummarizeOutcomes(outs)
	if summary.Total != 4 || summary.Done != 1 || summary.Abandoned != 1 || summary.Stale != 1 || summary.Open != 1 {
		t.Errorf("summary = %+v", summary)
	}
	text := summary.String()
	for _, want := range []string{"work items 4: done 1", "median 41m0s", "ended without DONE 1", "blocked median 2m0s"} {
		if !strings.Contains(text, want) {
			t.Errorf("summary %q lacks %q", text, want)
		}
	}
	if unfinished := Unfinished(outs); len(unfinished) != 2 {
		t.Errorf("unfinished = %+v, want the stale and the abandoned item", unfinished)
	}
}

func TestOutcomesWithoutWorkItems(t *testing.T) {
	outs := Outcomes([]Entry{entry(base, ActionSilent), progress(base, "1.1", PhaseDone, "")}, base, time.Hour)

	if len(outs) != 0 {
		t.Errorf("outcomes = %+v", outs)
	}
	if got := SummarizeOutcomes(outs).String(); got != "work items: none" {
		t.Errorf("summary = %q", got)
	}
}

func TestSlackTimestampBeatsJournalTimeForLatency(t *testing.T) {
	handoffTS := "1789000000.000001"
	handoffAt := time.Unix(1789000000, 1000).UTC()
	late := handoffAt.Add(30 * time.Minute)
	entries := []Entry{
		{At: late, ID: "T1_x", Source: SourceReplay, Channel: "C1", MsgTS: handoffTS, ThreadTS: handoffTS, Kind: "review", Action: ActionOpened},
		{At: late.Add(time.Minute), ID: "T1_y", Channel: "C1", MsgTS: "1789003600.000001", ThreadTS: handoffTS, Kind: "none", Action: ActionProgress, Phase: PhaseReviewDone},
	}

	outs := Outcomes(entries, late.Add(time.Hour), 0)

	if len(outs) != 1 || outs[0].Latency.Round(time.Minute) != time.Hour {
		t.Errorf("outcomes = %+v, want latency measured from the Slack handoff timestamp", outs)
	}
}

func TestTaggedMemberVerdictsDoNotCloseTheWorkItem(t *testing.T) {
	handoff := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	member := progress(handoff.Add(10*time.Minute), "1789000000.000001", PhaseReviewDone, "08c9fc23a726")
	member.Agent = "codex"
	entries := []Entry{opened(handoff, "1789000000.000001", "s1", "review"), member}

	outs := Outcomes(entries, handoff.Add(time.Hour), 6*time.Hour)

	if len(outs) != 1 || outs[0].Status != StatusOpen {
		t.Errorf("outcomes = %+v, a member's tagged verdict is not the joint one", outs)
	}
}
