package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Kriso1337/handoffd/internal/delivery"
	"github.com/Kriso1337/handoffd/internal/journal"
	"github.com/Kriso1337/handoffd/internal/launcher"
	"github.com/Kriso1337/handoffd/internal/outbox"
	"github.com/Kriso1337/handoffd/internal/slackfetch"
	"github.com/Kriso1337/handoffd/internal/state"
	"github.com/Kriso1337/handoffd/internal/worktree"
)

const (
	reviewRoot = "1789049440.051109"
	mrURL      = "https://gitlab.example.com/team-a/service-a/-/merge_requests/899"
)

func reviewHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, AuthorName: "teammate-b", Text: handoff})
	h.handle(t, notification(reviewRoot, ""))
	h.liveWindow("@1", "rev/PRJ-8866")
	return h
}

func (h *harness) post(t *testing.T, r outbox.Record) outbox.Receipt {
	t.Helper()
	if r.ID == "" {
		h.ids++
		r.ID = "post-" + time.Now().Format("150405.000000") + "-" + string(rune('a'+h.ids%26))
	}
	if r.At.IsZero() {
		r.At = h.pipe.now()
	}
	if r.ThreadTS == "" {
		r.ThreadTS = reviewRoot
	}
	if r.SessionID == "" {
		r.SessionID = "sess-1"
	}
	h.outbox.add(r)
	h.pipe.DrainOutbox(context.Background())
	return h.outbox.receipt(r.ID)
}

func TestReplyIsLabelledAndPostedIntoTheThread(t *testing.T) {
	h := reviewHarness(t)

	rc := h.post(t, outbox.Record{Kind: outbox.KindReply, Text: "looking into it, asked about the migration in the MR"})

	if rc.Status != outbox.StatusPosted || rc.Permalink != "https://acme.slack.com/archives/C100HOME/p1789060000000001" {
		t.Fatalf("receipt = %+v", rc)
	}
	if got := h.poster.texts(); len(got) != 1 || got[0] != "🤖 Atlas: looking into it, asked about the migration in the MR" || h.poster.posts[0].ThreadTS != reviewRoot {
		t.Errorf("posts = %+v", h.poster.posts)
	}
	e := h.journal.last()
	if e.Action != journal.ActionPosted || e.Source != journal.SourceOutbox || e.Kind != "reply" || e.SessionID != "sess-1" || e.ThreadTS != reviewRoot {
		t.Errorf("entry = %+v", e)
	}
}

func TestPostsFromUnknownThreadsOrSessionsAreRejected(t *testing.T) {
	h := reviewHarness(t)

	if rc := h.post(t, outbox.Record{Kind: outbox.KindReply, Text: "x", ThreadTS: "1789000000.000009"}); rc.Status != outbox.StatusRejected || rc.Reason != "unknown thread" {
		t.Errorf("unknown thread receipt = %+v", rc)
	}
	if rc := h.post(t, outbox.Record{Kind: outbox.KindReply, Text: "x", SessionID: "sess-stranger"}); rc.Status != outbox.StatusRejected || rc.Reason != "foreign session" {
		t.Errorf("foreign session receipt = %+v", rc)
	}
	if rc := h.post(t, outbox.Record{Kind: outbox.KindReply, Text: "🤖 Atlas: self-labelled"}); rc.Status != outbox.StatusRejected || rc.Reason != "label in text" {
		t.Errorf("label receipt = %+v", rc)
	}
	first := h.post(t, outbox.Record{Kind: outbox.KindReply, Text: "same message"})
	second := h.post(t, outbox.Record{Kind: outbox.KindReply, Text: "same message"})
	if first.Status != outbox.StatusPosted || second.Status != outbox.StatusRejected || second.Reason != "duplicate" {
		t.Errorf("duplicate receipts = %+v %+v", first, second)
	}
	if len(h.poster.texts()) != 1 {
		t.Errorf("only one message may leave: %v", h.poster.texts())
	}
	if e := h.journal.last(); e.Action != journal.ActionRejected || e.Reason != "duplicate" {
		t.Errorf("entry = %+v", e)
	}
}

func TestMarkersFollowTheProtocolAndCloseTheOutcome(t *testing.T) {
	h := reviewHarness(t)

	if rc := h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseReviewStart, SHA: oldSHA}); rc.Reason != "out of order: taken first" {
		t.Errorf("review-start before taken = %+v", rc)
	}
	if rc := h.post(t, outbox.Record{Kind: outbox.KindReviewDone, SHA: oldSHA}); rc.Reason != "out of order: review_start first" {
		t.Errorf("review-done before review-start = %+v", rc)
	}
	taken := h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseTaken, SHA: oldSHA})
	if taken.Status != outbox.StatusPosted {
		t.Fatalf("taken = %+v", taken)
	}
	if got := h.poster.texts()[0]; got != "🤖 Atlas: [TAKEN] "+mrURL+" / "+oldSHA {
		t.Errorf("taken text = %q", got)
	}
	th, _ := h.store.Thread(reviewRoot)
	if th.Progress != "taken" || th.ProgressSHA != oldSHA || th.Run == nil || th.Run.Round != 1 || th.Run.HeadSHA != oldSHA {
		t.Errorf("thread after taken = %+v run=%+v", th, th.Run)
	}
	if rc := h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseTaken, SHA: oldSHA, Text: "again"}); rc.Reason != "taken already posted" {
		t.Errorf("second taken = %+v", rc)
	}
	if rc := h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseReviewStart, SHA: oldSHA}); rc.Status != outbox.StatusPosted {
		t.Fatalf("review-start = %+v", rc)
	}
	th, _ = h.store.Thread(reviewRoot)
	if !th.Run.ReviewStarted || th.Progress != "review_start" {
		t.Errorf("thread after review-start = %+v run=%+v", th, th.Run)
	}

	done := h.post(t, outbox.Record{Kind: outbox.KindReviewDone, SHA: oldSHA, Blockers: 1, Others: 2, Decision: "migration rollback required"})

	if done.Status != outbox.StatusPosted {
		t.Fatalf("review-done = %+v", done)
	}
	if got := h.poster.texts()[2]; got != "🤖 Atlas: [REVIEW DONE] "+oldSHA+" — blockers 1, other 2, findings in the MR: "+mrURL+"; decision needed: migration rollback required" {
		t.Errorf("done text = %q", got)
	}
	th, _ = h.store.Thread(reviewRoot)
	if th.Progress != "review_done" || th.Run.Joint == nil || th.Run.Joint.SHA != oldSHA {
		t.Errorf("thread after done = %+v run=%+v", th, th.Run)
	}
	outs := journal.Outcomes(h.journal.entries, h.pipe.now(), time.Hour)
	if len(outs) != 1 || outs[0].Status != journal.StatusDone || outs[0].DoneSHA != oldSHA {
		t.Errorf("outcomes = %+v", outs)
	}
	var progress []string
	for _, e := range h.journal.entries {
		if e.Action == journal.ActionProgress {
			progress = append(progress, e.Phase)
		}
	}
	if strings.Join(progress, ",") != "taken,review_start,review_done" {
		t.Errorf("progress entries = %v", progress)
	}
}

func TestNotesAndMarkersOnAnotherShaAreRefused(t *testing.T) {
	h := reviewHarness(t)
	h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseTaken, SHA: oldSHA})

	stale := h.post(t, outbox.Record{Kind: outbox.KindMRNote, SHA: baseSHA, Text: "app.go:12 nil deref"})
	if stale.Status != outbox.StatusRejected || stale.Reason != "head moved to "+oldSHA {
		t.Errorf("stale note = %+v", stale)
	}
	if rc := h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseReviewStart, SHA: baseSHA[:12]}); rc.Reason != "head moved to "+oldSHA {
		t.Errorf("stale marker = %+v", rc)
	}

	fresh := h.post(t, outbox.Record{Kind: outbox.KindMRNote, SHA: oldSHA[:12], Text: "app.go:12 nil deref — branch regression"})

	if fresh.Status != outbox.StatusPosted || fresh.Permalink != mrURL {
		t.Errorf("fresh note = %+v", fresh)
	}
	if len(h.notes.notes) != 1 || h.notes.notes[0] != "service-a!899: (driver, 3bd416e4346c) app.go:12 nil deref — branch regression" {
		t.Errorf("notes = %v", h.notes.notes)
	}
	if len(h.poster.texts()) != 1 {
		t.Errorf("notes must not reach Slack: %v", h.poster.texts())
	}
	th, _ := h.store.Thread(reviewRoot)
	if len(th.Run.Notes) != 1 || th.Run.Notes[0].Agent != "driver" || th.Run.Notes[0].SHA != oldSHA[:12] {
		t.Errorf("run notes = %+v", th.Run.Notes)
	}
}

func TestReviewDoneReReadsTheProviderHeadAndQuarantinesStaleVerdicts(t *testing.T) {
	h := reviewHarness(t)
	h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseTaken, SHA: oldSHA})
	h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseReviewStart, SHA: oldSHA})
	h.worktrees.synced = worktree.Result{Path: worktreePath, SHA: oldSHA, HeadSHA: newSHA, BaseSHA: baseSHA}

	rc := h.post(t, outbox.Record{Kind: outbox.KindReviewDone, SHA: oldSHA, Blockers: 1})

	if rc.Status != outbox.StatusPosted {
		t.Fatalf("stale verdict must still be published: %+v", rc)
	}
	texts := h.poster.texts()
	if got := texts[len(texts)-1]; !strings.Contains(got, "[REVIEW DONE] "+oldSHA+" — blockers 1") || !strings.Contains(got, "(stale: current round 2, head "+newSHA[:12]+")") {
		t.Errorf("stale text = %q", got)
	}
	th, _ := h.store.Thread(reviewRoot)
	if th.Run.Round != 2 || th.Run.HeadSHA != newSHA || th.Run.Joint != nil || th.Progress != "review_start" || len(th.RunsHistory) != 1 {
		t.Errorf("thread = %+v run=%+v", th, th.Run)
	}
	if e := h.journal.last(); e.Action != journal.ActionStale || e.Round != 2 {
		t.Errorf("entry = %+v", e)
	}
	if outs := journal.Outcomes(h.journal.entries, h.pipe.now(), time.Hour); outs[0].Status != journal.StatusOpen {
		t.Errorf("outcomes = %+v, the round must stay open", outs)
	}
	if rc := h.post(t, outbox.Record{Kind: outbox.KindReviewDone, SHA: newSHA, Blockers: 0}); rc.Reason != "out of order: review_start first" {
		t.Errorf("new round needs a fresh review-start, got %+v", rc)
	}
	h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseReviewStart, SHA: newSHA})
	if rc := h.post(t, outbox.Record{Kind: outbox.KindReviewDone, SHA: newSHA, Blockers: 0, Text: "fresh"}); rc.Status != outbox.StatusPosted {
		t.Errorf("fresh verdict = %+v", rc)
	}
	if outs := journal.Outcomes(h.journal.entries, h.pipe.now(), time.Hour); outs[0].Status != journal.StatusDone || outs[0].DoneSHA != newSHA {
		t.Errorf("outcomes = %+v", outs)
	}
}

func TestHeadMoveAtContinuationOpensANewRound(t *testing.T) {
	h := reviewHarness(t)
	h.worktrees.synced = worktree.Result{Path: worktreePath, SHA: oldSHA, HeadSHA: newSHA, BaseSHA: baseSHA}
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: peerID, Text: "[HEAD CHANGED] " + oldSHA[:12] + " → " + newSHA[:12]}}

	h.handle(t, notification("1789049999.000100", reviewRoot))

	th, _ := h.store.Thread(reviewRoot)
	if th.Run.Round != 2 || th.Run.HeadSHA != newSHA || th.Run.OpenedBy != "head_changed" || len(th.RunsHistory) != 1 || th.RunsHistory[0].HeadSHA != oldSHA {
		t.Errorf("run = %+v history=%+v", th.Run, th.RunsHistory)
	}
	if sent := h.windows.sent; len(sent) != 1 || !strings.Contains(sent[0], "round 2, head "+newSHA[:12]+", previous results are void") {
		t.Errorf("continuation = %v", sent)
	}
	h.handle(t, notification("1789049999.000200", reviewRoot))
	if th, _ := h.store.Thread(reviewRoot); th.Run.Round != 2 || strings.Contains(h.windows.sent[1], "invalidated") {
		t.Errorf("an unchanged head must not open another round: run=%+v sent=%q", th.Run, h.windows.sent[1])
	}
}

func TestARefusedDeliveryLeavesAPendingReceiptAndAlerts(t *testing.T) {
	h := reviewHarness(t)
	alerts := &fakeAlerts{}
	h.pipe.alerts = alerts
	h.poster.err = fmt.Errorf("%w: slack said channel_not_found", delivery.ErrNotAccepted)

	rc := h.post(t, outbox.Record{Kind: outbox.KindReply, Text: "will not arrive"})

	if rc.Status != outbox.StatusPending || !strings.Contains(rc.Reason, "channel_not_found") || rc.Attempts != postAttempts {
		t.Errorf("receipt = %+v", rc)
	}
	if e := h.journal.last(); e.Action != journal.ActionError || !strings.Contains(e.Error, "channel_not_found") {
		t.Errorf("entry = %+v", e)
	}
	if len(alerts.sent) != 1 || !strings.Contains(alerts.sent[0], "outbound reply") {
		t.Errorf("alerts = %v", alerts.sent)
	}
}

func TestMalformedOutboxRecordIsQuarantined(t *testing.T) {
	h := reviewHarness(t)
	h.outbox.malformed = []outbox.Malformed{{Path: "/state/outbox/zz.json", Err: errors.New("unexpected end of JSON input")}}

	h.pipe.DrainOutbox(context.Background())

	if len(h.outbox.quarantined) != 1 || !strings.HasPrefix(h.outbox.quarantined[0], "/state/outbox/zz.json malformed:") {
		t.Errorf("quarantined = %v", h.outbox.quarantined)
	}
	if e := h.journal.last(); e.Action != journal.ActionRejected || e.ID != "post_zz" {
		t.Errorf("entry = %+v", e)
	}
}

func committeeReview(t *testing.T) *harness {
	t.Helper()
	h := committeeHarness(t, "claude", "codex")
	h.windows.nextIDs = []string{"@1", "@2"}
	h.handle(t, notification(reviewRoot, ""))
	h.windows.mu.Lock()
	h.windows.existing = []launcher.Window{{ID: "@1", Name: "rev/PRJ-8866~codex", Command: "2.1.267", Occupancy: launcher.OccupancyAgent}, {ID: "@2", Name: "rev/PRJ-8866", Command: "2.1.267", Occupancy: launcher.OccupancyAgent}}
	h.windows.mu.Unlock()
	return h
}

const (
	memberSession = "sess-1"
	driverSession = "sess-2"
)

func TestCommitteeResultsAreCollectedAndTheDriverGetsTheTable(t *testing.T) {
	h := committeeReview(t)
	h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseTaken, SHA: oldSHA, SessionID: driverSession})
	h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseReviewStart, SHA: oldSHA, SessionID: driverSession})

	if rc := h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseTaken, SHA: oldSHA, SessionID: memberSession, Text: "m"}); rc.Reason != "only the driver posts taken" {
		t.Errorf("member taken = %+v", rc)
	}
	if rc := h.post(t, outbox.Record{Kind: outbox.KindReviewDone, SHA: oldSHA, SessionID: driverSession, Blockers: 1}); rc.Reason != "waiting for: codex" {
		t.Errorf("early joint verdict = %+v", rc)
	}
	note := h.post(t, outbox.Record{Kind: outbox.KindMRNote, SHA: oldSHA, SessionID: memberSession, Text: "svc.go:40 map race"})
	if note.Status != outbox.StatusPosted || h.notes.notes[0] != "service-a!899: (codex, 3bd416e4346c) svc.go:40 map race" {
		t.Errorf("member note = %+v notes=%v", note, h.notes.notes)
	}

	member := h.post(t, outbox.Record{Kind: outbox.KindReviewDone, SHA: oldSHA, SessionID: memberSession, Blockers: 1, Others: 0, Decision: "no"})

	if member.Status != outbox.StatusPosted {
		t.Fatalf("member verdict = %+v", member)
	}
	texts := h.poster.texts()
	if got := texts[len(texts)-1]; !strings.HasPrefix(got, "🤖 Atlas: [REVIEW DONE] "+oldSHA+" (codex) — blockers 1, other 0") {
		t.Errorf("member text = %q", got)
	}
	th, _ := h.store.Thread(reviewRoot)
	res, ok := th.Run.Results["codex"]
	if !ok || res.Blockers != 1 || res.Findings != 1 || th.Members[0].Progress != "review_done" || th.Progress != "review_start" {
		t.Errorf("thread = %+v run=%+v", th, th.Run)
	}
	h.windows.mu.Lock()
	sent := append([]string(nil), h.windows.sent...)
	h.windows.mu.Unlock()
	if len(sent) != 1 || !strings.Contains(sent[0], "Round 1 results collected") || !strings.Contains(sent[0], "codex: blockers 1, other 0, decision: no; findings: svc.go:40 map race") {
		t.Errorf("driver continuation = %v", sent)
	}
	if e := h.journal.last(); e.Action != journal.ActionProgress || e.Agent != "codex" {
		t.Errorf("member progress entry = %+v", e)
	}
	if outs := journal.Outcomes(h.journal.entries, h.pipe.now(), time.Hour); outs[0].Status != journal.StatusOpen {
		t.Errorf("a member verdict must not close the item: %+v", outs)
	}

	joint := h.post(t, outbox.Record{Kind: outbox.KindReviewDone, SHA: oldSHA, SessionID: driverSession, Blockers: 1, Others: 1, Decision: "do not merge"})

	if joint.Status != outbox.StatusPosted {
		t.Fatalf("joint verdict = %+v", joint)
	}
	texts = h.poster.texts()
	if got := texts[len(texts)-1]; !strings.HasPrefix(got, "🤖 Atlas: [REVIEW DONE] "+oldSHA+" — blockers 1, other 1") || strings.Contains(got, "(claude)") {
		t.Errorf("joint text = %q", got)
	}
	th, _ = h.store.Thread(reviewRoot)
	if th.Run.Joint == nil || th.Progress != "review_done" {
		t.Errorf("thread after joint = %+v run=%+v", th, th.Run)
	}
	if outs := journal.Outcomes(h.journal.entries, h.pipe.now(), time.Hour); len(outs) != 1 || outs[0].Status != journal.StatusDone {
		t.Errorf("outcomes = %+v", outs)
	}
}

func TestDeadCommitteeMemberDoesNotBlockTheJointVerdict(t *testing.T) {
	h := committeeReview(t)
	h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseTaken, SHA: oldSHA, SessionID: driverSession})
	h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseReviewStart, SHA: oldSHA, SessionID: driverSession})
	h.windows.mu.Lock()
	h.windows.existing = []launcher.Window{{ID: "@2", Name: "rev/PRJ-8866", Command: "2.1.267", Occupancy: launcher.OccupancyAgent}}
	h.windows.mu.Unlock()

	rc := h.post(t, outbox.Record{Kind: outbox.KindReviewDone, SHA: oldSHA, SessionID: driverSession, Blockers: 0})

	if rc.Status != outbox.StatusPosted {
		t.Errorf("joint verdict with a dead member = %+v", rc)
	}
	if th, _ := h.store.Thread(reviewRoot); th.Run.Joint == nil {
		t.Errorf("run = %+v", th.Run)
	}
}

func TestADeliveredPostThatCannotCommitItsTransitionStaysRecoverable(t *testing.T) {
	h := reviewHarness(t)
	blockStateWrites(t, h.cfg.StatePath)

	rc := h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseTaken, SHA: oldSHA})

	if rc.Status != outbox.StatusPending {
		t.Fatalf("receipt = %+v, a delivered post whose transition was not committed must stay recoverable", rc)
	}
	if !strings.Contains(rc.Reason, "transition not committed") {
		t.Errorf("reason = %q, it must say the transition was lost, not that the post failed", rc.Reason)
	}
	if got := h.poster.texts(); len(got) != 1 {
		t.Errorf("posts = %v, the message must have reached the thread exactly once", got)
	}
}

func TestAResultIsHeldWhenTheProviderBaseIsNotConfirmed(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, AuthorName: "teammate-b", Text: handoff})
	h.worktrees.result = worktree.Result{Path: worktreePath, SHA: oldSHA, HeadSHA: oldSHA, BaseSHA: baseSHA}
	h.handle(t, notification(reviewRoot, ""))
	h.liveWindow("@1", "rev/PRJ-8866")
	h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseTaken, SHA: oldSHA})
	h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseReviewStart, SHA: oldSHA})
	before := len(h.poster.texts())
	h.worktrees.synced = worktree.Result{Path: worktreePath, SHA: oldSHA, HeadSHA: oldSHA}

	rc := h.post(t, outbox.Record{Kind: outbox.KindReviewDone, SHA: oldSHA, Blockers: 1})

	if rc.Status != outbox.StatusPending || !strings.Contains(rc.Reason, "base") {
		t.Fatalf("receipt = %+v, a result must not be admitted against a base the provider did not confirm", rc)
	}
	if got := len(h.poster.texts()); got != before {
		t.Errorf("posted %d more messages, an unconfirmed base must not reach the thread", got-before)
	}
}

func TestUnknownProviderHeadHoldsTheResultPending(t *testing.T) {
	h := reviewHarness(t)
	h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseTaken, SHA: oldSHA})
	h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseReviewStart, SHA: oldSHA})
	before := len(h.poster.texts())
	h.worktrees.inspectErr = errors.New("fetch refs/merge-requests/899/head failed")

	rc := h.post(t, outbox.Record{Kind: outbox.KindReviewDone, SHA: oldSHA, Blockers: 1})

	if rc.Status != outbox.StatusPending || !strings.Contains(rc.Reason, "provider head") {
		t.Fatalf("receipt = %+v, want the result held until the head is known", rc)
	}
	if got := len(h.poster.texts()); got != before {
		t.Errorf("posted %d messages, a result on an unconfirmed head must not reach the thread", got-before)
	}
	th, _ := h.store.Thread(reviewRoot)
	if th.Run.Joint != nil || th.Progress != "review_start" {
		t.Errorf("run = %+v, the round must stay open", th.Run)
	}
}

func TestProviderBaseMoveOpensANewRound(t *testing.T) {
	h := newHarness(t, slackfetch.Message{AuthorID: peerID, AuthorName: "teammate-b", Text: handoff})
	h.worktrees.result = worktree.Result{Path: worktreePath, SHA: oldSHA, HeadSHA: oldSHA, BaseSHA: baseSHA}
	h.handle(t, notification(reviewRoot, ""))
	h.liveWindow("@1", "rev/PRJ-8866")
	h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseTaken, SHA: oldSHA})
	h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseReviewStart, SHA: oldSHA})
	h.worktrees.synced = worktree.Result{Path: worktreePath, SHA: oldSHA, HeadSHA: oldSHA, BaseSHA: newSHA}

	rc := h.post(t, outbox.Record{Kind: outbox.KindReviewDone, SHA: oldSHA, Blockers: 1})

	if rc.Status != outbox.StatusPosted {
		t.Fatalf("verdict = %+v", rc)
	}
	th, _ := h.store.Thread(reviewRoot)
	if th.Run.Round != 2 || th.Run.BaseSHA != newSHA || th.Run.OpenedBy != state.RunOpenedByBaseChanged {
		t.Errorf("run = %+v, want a new round opened by the base move", th.Run)
	}
	if th.Run.Joint != nil || th.Progress != "review_start" {
		t.Errorf("run = %+v, a verdict on the old base must not close the new round", th.Run)
	}
}

func TestASecondResultFromTheSameMemberIsRejected(t *testing.T) {
	h := committeeReview(t)
	h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseTaken, SHA: oldSHA, SessionID: driverSession})
	h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseReviewStart, SHA: oldSHA, SessionID: driverSession})
	if rc := h.post(t, outbox.Record{Kind: outbox.KindReviewDone, SHA: oldSHA, SessionID: memberSession, Blockers: 1, Decision: "no"}); rc.Status != outbox.StatusPosted {
		t.Fatalf("first member result = %+v", rc)
	}
	before := len(h.poster.texts())

	second := h.post(t, outbox.Record{Kind: outbox.KindReviewDone, SHA: oldSHA, SessionID: memberSession, Blockers: 0, Decision: "changed my mind", Text: "correction"})

	if second.Status != outbox.StatusRejected || !strings.Contains(second.Reason, "already submitted") {
		t.Fatalf("second member result = %+v", second)
	}
	if got := len(h.poster.texts()); got != before {
		t.Errorf("posted %d more messages, a round must not carry two results from one agent", got-before)
	}
	th, _ := h.store.Thread(reviewRoot)
	if res := th.Run.Results["codex"]; res.Blockers != 1 || res.Decision != "no" {
		t.Errorf("result = %+v, the recorded result must not be overwritten", res)
	}
}

func TestASecondJointResultIsRejected(t *testing.T) {
	h := committeeReview(t)
	h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseTaken, SHA: oldSHA, SessionID: driverSession})
	h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseReviewStart, SHA: oldSHA, SessionID: driverSession})
	h.post(t, outbox.Record{Kind: outbox.KindReviewDone, SHA: oldSHA, SessionID: memberSession, Blockers: 1, Decision: "no"})
	if rc := h.post(t, outbox.Record{Kind: outbox.KindReviewDone, SHA: oldSHA, SessionID: driverSession, Blockers: 1, Others: 1, Decision: "do not merge"}); rc.Status != outbox.StatusPosted {
		t.Fatalf("joint result = %+v", rc)
	}
	before := len(h.poster.texts())

	second := h.post(t, outbox.Record{Kind: outbox.KindReviewDone, SHA: oldSHA, SessionID: driverSession, Blockers: 0, Decision: "merge after all", Text: "correction"})

	if second.Status != outbox.StatusRejected || !strings.Contains(second.Reason, "already closed by the joint result") {
		t.Fatalf("second joint result = %+v", second)
	}
	if got := len(h.poster.texts()); got != before {
		t.Errorf("posted %d more messages, a closed round must not get a second verdict", got-before)
	}
	th, _ := h.store.Thread(reviewRoot)
	if th.Run.Joint == nil || th.Run.Joint.SHA != oldSHA {
		t.Errorf("joint = %+v, the recorded verdict must not be overwritten", th.Run.Joint)
	}
}

func TestAMemberResultAfterTheJointIsRejected(t *testing.T) {
	h := committeeReview(t)
	h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseTaken, SHA: oldSHA, SessionID: driverSession})
	h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseReviewStart, SHA: oldSHA, SessionID: driverSession})
	h.post(t, outbox.Record{Kind: outbox.KindReviewDone, SHA: oldSHA, SessionID: memberSession, Blockers: 1, Decision: "no"})
	h.post(t, outbox.Record{Kind: outbox.KindReviewDone, SHA: oldSHA, SessionID: driverSession, Blockers: 1, Others: 1, Decision: "do not merge"})
	before := len(h.poster.texts())

	late := h.post(t, outbox.Record{Kind: outbox.KindReviewDone, SHA: oldSHA, SessionID: memberSession, Blockers: 3, Decision: "found more", Text: "late"})

	if late.Status != outbox.StatusRejected || !strings.Contains(late.Reason, "already closed by the joint result") {
		t.Fatalf("late member result = %+v", late)
	}
	if got := len(h.poster.texts()); got != before {
		t.Errorf("posted %d more messages, a closed round must not accept a late result", got-before)
	}
}

func TestARetryAfterAFailedTransitionIsAdmittedAgain(t *testing.T) {
	h := reviewHarness(t)
	release := blockStateWrites(t, h.cfg.StatePath)

	rc := h.post(t, outbox.Record{ID: "post-taken", Kind: outbox.KindMarker, Phase: outbox.PhaseTaken, SHA: oldSHA})

	if rc.Status != outbox.StatusPending {
		t.Fatalf("receipt = %+v, want the record left for retry", rc)
	}
	if th, _ := h.store.Thread(reviewRoot); th.Progress != "" {
		t.Fatalf("thread = %+v, an uncommitted transition must not stay in memory", th)
	}

	release()
	h.outbox.retry()
	h.pipe.DrainOutbox(context.Background())

	if rc := h.outbox.receipt("post-taken"); rc.Status != outbox.StatusPosted {
		t.Fatalf("receipt after retry = %+v, want the transition committed", rc)
	}
	if th, _ := h.store.Thread(reviewRoot); th.Progress != "taken" {
		t.Errorf("thread after retry = %+v, want the round transition recorded", th)
	}
	if got := len(h.poster.texts()); got != 2 {
		t.Errorf("posts = %d, a retry of a delivered message costs a duplicate in the thread", got)
	}
}

func TestAPeerResultIsRecordedIntoARoundThatReloadedWithoutResults(t *testing.T) {
	h := committeeReview(t)
	h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseTaken, SHA: oldSHA, SessionID: driverSession})
	h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseReviewStart, SHA: oldSHA, SessionID: driverSession})
	reloaded, _ := h.store.Thread(reviewRoot)
	reloaded.Run.Results = nil
	h.store.SetThread(reviewRoot, reloaded)

	rc := h.post(t, outbox.Record{Kind: outbox.KindReviewDone, SHA: oldSHA, SessionID: memberSession, Blockers: 1})

	if rc.Status != outbox.StatusPosted {
		t.Fatalf("member verdict = %+v", rc)
	}
	th, _ := h.store.Thread(reviewRoot)
	if res, ok := th.Run.Results["codex"]; !ok || res.Blockers != 1 {
		t.Errorf("results = %+v, a round reloaded without results must still take one", th.Run.Results)
	}
}

func TestADeliveryDoesNotUndoARoundOpenedWhileItWasInFlight(t *testing.T) {
	h := reviewHarness(t)
	h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseTaken, SHA: oldSHA})
	h.poster.inTransit = func() {
		if err := h.store.UpdateThread(reviewRoot, func(th *state.Thread) error {
			th.HeadSHA = newSHA
			th.OpenRound(newSHA, baseSHA, state.RunOpenedByHeadChanged, h.pipe.now())
			return nil
		}); err != nil {
			t.Errorf("UpdateThread: %v", err)
		}
	}

	rc := h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseReviewStart, SHA: oldSHA})

	if rc.Status != outbox.StatusPosted {
		t.Fatalf("receipt = %+v", rc)
	}
	th, _ := h.store.Thread(reviewRoot)
	if th.Run == nil || th.Run.Round != 2 || th.Run.HeadSHA != newSHA || len(th.RunsHistory) != 1 {
		t.Errorf("run = %+v history = %+v, the round opened during delivery must survive it", th.Run, th.RunsHistory)
	}
	if th.Run.ReviewStarted {
		t.Error("the transition belonged to the round that was replaced, it must not land on the new one")
	}
}

func TestAProgressMarkerDoesNotUndoAChangeMadeWhileTheProviderWasRead(t *testing.T) {
	h := reviewHarness(t)
	h.worktrees.synced = worktree.Result{Path: worktreePath, SHA: oldSHA, HeadSHA: oldSHA, BaseSHA: baseSHA}
	h.worktrees.inTransit = func() {
		if err := h.store.UpdateThread(reviewRoot, func(th *state.Thread) error {
			th.Members = append(th.Members, state.Member{Agent: "codex", WindowID: "@7", SessionID: "sess-codex"})
			return nil
		}); err != nil {
			t.Errorf("UpdateThread: %v", err)
		}
	}
	h.pipe.fetcher = fakeFetcher{msg: slackfetch.Message{AuthorID: selfID, Text: "[REVIEW DONE] " + oldSHA + " — blockers 0"}}

	h.handle(t, notification("1789049999.000100", reviewRoot))

	th, _ := h.store.Thread(reviewRoot)
	if len(th.Members) != 1 || th.Members[0].Agent != "codex" {
		t.Errorf("members = %+v, a member attached while the provider was read must survive", th.Members)
	}
	if th.Progress != "review_done" {
		t.Errorf("thread = %+v, the marker must still be recorded", th)
	}
}

func TestADeliveryWhoseAcceptanceIsUnknownIsNotSentAgain(t *testing.T) {
	h := reviewHarness(t)
	alerts := &fakeAlerts{}
	h.pipe.alerts = alerts
	h.poster.err = errors.New("post to slack: context deadline exceeded")

	rc := h.post(t, outbox.Record{Kind: outbox.KindReply, Text: "delivery may be ambiguous"})

	if h.poster.tries() != 1 {
		t.Errorf("tries = %d, a message that may already be in the thread must not be sent again", h.poster.tries())
	}
	if rc.Status != outbox.StatusUnknown || !strings.Contains(rc.Reason, "deadline exceeded") {
		t.Errorf("receipt = %+v, want the acceptance recorded as unknown", rc)
	}
	if len(alerts.sent) != 1 || !strings.Contains(alerts.sent[0], "acceptance unknown") {
		t.Errorf("alerts = %v, a human has to look at the thread", alerts.sent)
	}
	h.outbox.retry()
	pending, _, err := h.outbox.Pending()
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("pending = %+v, an unknown acceptance must not be picked up for another attempt", pending)
	}
}

func TestADeliveryTheProviderRefusedIsRetried(t *testing.T) {
	h := reviewHarness(t)
	h.poster.fails = 2

	rc := h.post(t, outbox.Record{Kind: outbox.KindReply, Text: "on the second attempt"})

	if rc.Status != outbox.StatusPosted {
		t.Fatalf("receipt = %+v, a refusal says the message is not in the thread", rc)
	}
	if h.poster.tries() != 3 {
		t.Errorf("tries = %d, want the refused attempts retried", h.poster.tries())
	}
}

func TestAnUncertainDeliveryIsConfirmedByReadingTheThread(t *testing.T) {
	h := reviewHarness(t)
	text := "🤖 Atlas: delivered, but no reply was observed"
	h.thread.holds(slackfetch.Reply{TS: "1789060000.000777", AuthorID: selfID, Text: text})
	h.poster.err = errors.New("post to slack: context deadline exceeded")

	rc := h.post(t, outbox.Record{Kind: outbox.KindReply, Text: "delivered, but no reply was observed"})

	if rc.Status != outbox.StatusPosted || rc.TS != "1789060000.000777" {
		t.Fatalf("receipt = %+v, a message found in the thread is delivered, not unknown", rc)
	}
	if h.poster.tries() != 1 {
		t.Errorf("tries = %d, the message was already there", h.poster.tries())
	}
}

func TestAnUncertainDeliveryTheThreadDoesNotHoldStaysUnknown(t *testing.T) {
	h := reviewHarness(t)
	h.poster.err = errors.New("post to slack: context deadline exceeded")

	rc := h.post(t, outbox.Record{Kind: outbox.KindReply, Text: "not present in the thread"})

	if rc.Status != outbox.StatusUnknown {
		t.Errorf("receipt = %+v, a thread that does not hold the message cannot confirm it", rc)
	}
}
