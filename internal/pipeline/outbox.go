package pipeline

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kriso1337/handoffd/internal/delivery"
	"github.com/Kriso1337/handoffd/internal/journal"
	"github.com/Kriso1337/handoffd/internal/mrref"
	"github.com/Kriso1337/handoffd/internal/outbox"
	"github.com/Kriso1337/handoffd/internal/prompt"
	"github.com/Kriso1337/handoffd/internal/slackfetch"
	"github.com/Kriso1337/handoffd/internal/state"
)

const (
	postAttempts    = 3
	duplicateWindow = 10 * time.Minute
)

type Outbox interface {
	Pending() ([]outbox.Record, []outbox.Malformed, error)
	WriteReceipt(rc outbox.Receipt) error
	Quarantine(path, reason string, now time.Time) error
}

type Poster interface {
	Post(ctx context.Context, channel, threadTS, text string) (string, error)
}

type Readback interface {
	Replies(ctx context.Context, channel, threadTS, since string) ([]slackfetch.Reply, error)
}

type Notes interface {
	Note(ctx context.Context, mr mrref.MR, text string) error
}

func (p *Pipeline) RunOutbox(ctx context.Context) {
	if p.outbox == nil || p.outboxInterval <= 0 {
		return
	}
	ticker := time.NewTicker(p.outboxInterval)
	defer ticker.Stop()
	for {
		p.DrainOutbox(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *Pipeline) DrainOutbox(ctx context.Context) int {
	if p.outbox == nil {
		return 0
	}
	records, malformed, err := p.outbox.Pending()
	if err != nil {
		p.log.Warn("outbox unreadable", "error", err)
		return 0
	}
	for _, m := range malformed {
		id := strings.TrimSuffix(filepath.Base(m.Path), ".json")
		reason := "malformed: " + m.Err.Error()
		if err := p.outbox.Quarantine(m.Path, reason, p.now().UTC()); err != nil {
			p.log.Warn("malformed outbox record not quarantined", "path", m.Path, "error", err)
			continue
		}
		p.record(journal.Entry{ID: "post_" + id, Source: journal.SourceOutbox, Action: journal.ActionRejected, Reason: reason})
	}
	handled := 0
	for _, r := range records {
		if ctx.Err() != nil {
			return handled
		}
		p.handlePost(ctx, r)
		handled++
	}
	return handled
}

type post struct {
	rec    outbox.Record
	thread state.Thread
	agent  string
	driver bool
	stale  bool
	round  int
	entry  journal.Entry
}

var errRoundReplaced = errors.New("the round this message belongs to was replaced")

func roundOf(thread state.Thread) int {
	if thread.Run == nil {
		return 0
	}
	return thread.Run.Round
}

func (p *Pipeline) handlePost(ctx context.Context, r outbox.Record) {
	now := p.now().UTC()
	po := &post{rec: r, entry: journal.Entry{
		ID: "post_" + r.ID, Source: journal.SourceOutbox, ThreadTS: r.ThreadTS, Kind: string(r.Kind),
		SessionID: r.SessionID, HeadSHA: r.SHA, Phase: r.Phase, Text: journal.Snippet(r.Text),
	}}
	reason, err := p.admitPost(ctx, po)
	if err != nil {
		p.postFailed(po, err, now)
		return
	}
	if reason != "" {
		p.rejectPost(po, reason, now)
		return
	}
	text, err := p.postText(po)
	if err != nil {
		p.postFailed(po, err, now)
		return
	}
	permalink, ts, err := p.deliverPost(ctx, po, text)
	switch {
	case err != nil && errors.Is(err, delivery.ErrNotAccepted):
		p.postFailed(po, err, now)
		return
	case err != nil:
		found, foundTS, ok := p.readbackPost(ctx, po, text)
		if !ok {
			p.postUncertain(po, err, now)
			return
		}
		p.log.Info("uncertain delivery found in the thread, taking it as delivered",
			"id", po.rec.ID, "kind", po.rec.Kind, "thread_ts", po.rec.ThreadTS, "ts", foundTS)
		permalink, ts = found, foundTS
	}
	if err := p.recordPost(po, text, permalink, ts, now); err != nil {
		p.postDelivered(po, err, permalink, ts, now)
		return
	}
	p.writeReceipt(outbox.Receipt{ID: r.ID, Status: outbox.StatusPosted, Permalink: permalink, TS: ts, At: now})
	if po.rec.Kind == outbox.KindReviewDone && !po.driver && !po.stale {
		p.collectResults(ctx, r.ThreadTS)
	}
}

func (p *Pipeline) admitPost(ctx context.Context, po *post) (string, error) {
	thread, ok := p.store.Thread(po.rec.ThreadTS)
	if !ok {
		return "unknown thread", nil
	}
	agent, driver, ok := thread.Participant(po.rec.SessionID)
	if !ok {
		return "foreign session", nil
	}
	po.thread, po.agent, po.driver, po.round = thread, agent, driver, roundOf(thread)
	po.entry.Channel, po.entry.WindowID, po.entry.WindowName, po.entry.Worktree = thread.Channel, thread.WindowID, thread.WindowName, thread.Worktree
	if !driver {
		po.entry.Agent = agent
	}
	if label := strings.TrimSpace(p.cfg.Label()); label != "" && strings.HasPrefix(strings.TrimSpace(po.rec.Text), label) {
		return "label in text", nil
	}
	if p.seenPost(po.rec) {
		return "duplicate", nil
	}
	if po.rec.Kind == outbox.KindReply {
		return "", nil
	}
	if po.thread.Run == nil {
		po.thread.OpenRound(po.thread.HeadSHA, po.thread.BaseSHA, state.RunOpenedByHandoff, p.now().UTC())
	}
	if po.rec.Kind == outbox.KindReviewDone {
		reconciled, superseded, err := p.reconcileRound(ctx, po.thread)
		if err != nil {
			return "", err
		}
		po.thread, po.stale = reconciled, superseded
	}
	run := po.thread.Run
	po.entry.Round = run.Round
	if run.HeadSHA != "" && !mrref.SameCommit(po.rec.SHA, run.HeadSHA) {
		if po.rec.Kind != outbox.KindReviewDone {
			return "head moved to " + run.HeadSHA, nil
		}
		po.stale = true
	}
	switch po.rec.Kind {
	case outbox.KindMRNote:
		if po.thread.MR == nil {
			return "thread has no MR", nil
		}
	case outbox.KindMarker:
		if po.rec.Phase == outbox.PhaseTaken {
			if !po.driver {
				return "only the driver posts taken", nil
			}
			if po.thread.Progress != "" {
				return "taken already posted", nil
			}
		}
		if po.rec.Phase == outbox.PhaseReviewStart && po.thread.Progress == "" {
			return "out of order: taken first", nil
		}
	case outbox.KindReviewDone:
		if po.stale {
			break
		}
		if !run.ReviewStarted {
			return "out of order: review_start first", nil
		}
		if run.Joint != nil {
			return fmt.Sprintf("round %d is already closed by the joint result", run.Round), nil
		}
		if po.driver {
			if missing := p.missingResults(ctx, po.thread); len(missing) > 0 {
				return "waiting for: " + strings.Join(missing, ", "), nil
			}
			break
		}
		if _, done := run.Results[po.agent]; done {
			return fmt.Sprintf("%s already submitted a result for round %d", po.agent, run.Round), nil
		}
	}
	return "", nil
}

func (p *Pipeline) reconcileRound(ctx context.Context, thread state.Thread) (state.Thread, bool, error) {
	before := thread.HeadSHA
	thread, baseKnown, err := p.reconcileHead(ctx, thread)
	if err != nil {
		return thread, false, err
	}
	if thread.Run.BaseSHA != "" && !baseKnown {
		return thread, false, fmt.Errorf("provider base for %s not confirmed", thread.Worktree)
	}
	switch {
	case thread.HeadSHA != "" && thread.Run.HeadSHA != "" && !mrref.SameCommit(thread.HeadSHA, thread.Run.HeadSHA):
		p.log.Warn("provider head moved since the round opened, new round", "worktree", thread.Worktree, "was", thread.Run.HeadSHA, "now", thread.HeadSHA)
		thread.OpenRound(thread.HeadSHA, thread.BaseSHA, state.RunOpenedByHeadChanged, p.now().UTC())
		return thread, true, nil
	case thread.BaseSHA != "" && thread.Run.BaseSHA != "" && !mrref.SameCommit(thread.BaseSHA, thread.Run.BaseSHA):
		p.log.Warn("provider base moved since the round opened, new round", "worktree", thread.Worktree, "was", thread.Run.BaseSHA, "now", thread.BaseSHA)
		thread.OpenRound(thread.HeadSHA, thread.BaseSHA, state.RunOpenedByBaseChanged, p.now().UTC())
		return thread, true, nil
	case thread.Run.HeadSHA == "" && thread.HeadSHA != "" && before != thread.HeadSHA:
		thread.Run.HeadSHA = thread.HeadSHA
	}
	return thread, false, nil
}

func postKey(r outbox.Record) string {
	return r.SessionID + "|" + string(r.Kind) + "|" + r.Phase + "|" + r.SHA + "|" + strings.TrimSpace(r.Text)
}

func (p *Pipeline) seenPost(r outbox.Record) bool {
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, at := range p.recentPosts {
		if now.Sub(at) > duplicateWindow {
			delete(p.recentPosts, k)
		}
	}
	_, seen := p.recentPosts[postKey(r)]
	return seen
}

func (p *Pipeline) rememberPost(r outbox.Record) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recentPosts[postKey(r)] = p.now()
}

func (p *Pipeline) forgetPost(r outbox.Record) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.recentPosts, postKey(r))
}

func (p *Pipeline) missingResults(ctx context.Context, thread state.Thread) []string {
	var missing []string
	for _, m := range thread.Members {
		if _, done := thread.Run.Results[m.Agent]; done {
			continue
		}
		window, ok, err := p.windows.Find(ctx, m.WindowID)
		if err == nil && (!ok || !window.MayHoldAgent()) {
			continue
		}
		missing = append(missing, m.Agent)
	}
	return missing
}

func (p *Pipeline) postText(po *post) (string, error) {
	r := po.rec
	switch r.Kind {
	case outbox.KindReply:
		return p.cfg.Label() + r.Text, nil
	case outbox.KindMRNote:
		return fmt.Sprintf("(%s, %s) %s", po.agent, short(r.SHA), r.Text), nil
	case outbox.KindMarker:
		if r.Phase == outbox.PhaseTaken {
			return p.cfg.Label() + p.tagged(po, fmt.Sprintf("[TAKEN] %s / %s", p.mrLink(po.thread), r.SHA)), nil
		}
		return p.cfg.Label() + p.tagged(po, "[REVIEW START] "+r.SHA), nil
	default:
		line, err := p.render.ReviewDone(prompt.ReviewDone{
			SHA: r.SHA, Blockers: r.Blockers, Others: r.Others, Decision: r.Decision, FindingsURL: p.mrLink(po.thread),
			Agent: p.tag(po), StaleRound: po.thread.Run.Round, StaleHead: po.thread.Run.HeadSHA, Stale: po.stale,
		})
		if err != nil {
			return "", err
		}
		return p.cfg.Label() + line, nil
	}
}

func (p *Pipeline) tag(po *post) string {
	if po.driver {
		return ""
	}
	return po.agent
}

func (p *Pipeline) tagged(po *post, line string) string {
	if tag := p.tag(po); tag != "" {
		return line + " (" + tag + ")"
	}
	return line
}

func (p *Pipeline) mrLink(thread state.Thread) string {
	if thread.MR == nil {
		return thread.MergeRef
	}
	return thread.MR.URL()
}

func (p *Pipeline) deliverPost(ctx context.Context, po *post, text string) (permalink, ts string, err error) {
	for attempt := 1; attempt <= postAttempts; attempt++ {
		if po.rec.Kind == outbox.KindMRNote {
			err = p.notes.Note(ctx, *po.thread.MR, text)
			permalink = p.mrLink(po.thread)
		} else {
			ts, err = p.poster.Post(ctx, po.thread.Channel, po.rec.ThreadTS, text)
			if err == nil {
				permalink = prompt.Permalink(p.cfg.SlackWorkspace, po.thread.Channel, ts)
			}
		}
		if err == nil || ctx.Err() != nil {
			return permalink, ts, err
		}
		if !errors.Is(err, delivery.ErrNotAccepted) {
			return "", "", err
		}
		p.log.Warn("outbound delivery attempt refused", "id", po.rec.ID, "kind", po.rec.Kind, "attempt", attempt, "error", err)
		if attempt < postAttempts && !sleep(ctx, p.retryDelay) {
			return "", "", ctx.Err()
		}
	}
	return permalink, ts, err
}

func (p *Pipeline) rejectPost(po *post, reason string, now time.Time) {
	p.log.Warn("outbound message rejected", "id", po.rec.ID, "kind", po.rec.Kind, "thread_ts", po.rec.ThreadTS, "reason", reason)
	po.entry.Action, po.entry.Reason = journal.ActionRejected, reason
	p.record(po.entry)
	p.writeReceipt(outbox.Receipt{ID: po.rec.ID, Status: outbox.StatusRejected, Reason: reason, At: now})
}

func (p *Pipeline) postFailed(po *post, err error, now time.Time) {
	p.log.Error("outbound message not delivered", "id", po.rec.ID, "kind", po.rec.Kind, "error", err)
	po.entry.Action, po.entry.Error = journal.ActionError, err.Error()
	p.record(po.entry)
	p.writeReceipt(outbox.Receipt{ID: po.rec.ID, Status: outbox.StatusPending, Reason: err.Error(), Attempts: postAttempts, At: now})
	if p.alerts != nil {
		p.alerts.Alert(context.WithoutCancel(context.Background()), "outbox:"+po.rec.ID, "handoffd",
			fmt.Sprintf("outbound %s for %s not delivered: %s — handoffd replay", po.rec.Kind, po.rec.ThreadTS, err))
	}
}

func (p *Pipeline) readbackPost(ctx context.Context, po *post, text string) (permalink, ts string, ok bool) {
	if p.readback == nil || po.rec.Kind == outbox.KindMRNote || po.thread.Channel == "" {
		return "", "", false
	}
	replies, err := p.readback.Replies(ctx, po.thread.Channel, po.rec.ThreadTS, "")
	if err != nil {
		p.log.Warn("thread not read back after an uncertain delivery", "id", po.rec.ID, "error", err)
		return "", "", false
	}
	for i := len(replies) - 1; i >= 0; i-- {
		if replies[i].Text != text {
			continue
		}
		return prompt.Permalink(p.cfg.SlackWorkspace, po.thread.Channel, replies[i].TS), replies[i].TS, true
	}
	return "", "", false
}

func (p *Pipeline) postUncertain(po *post, err error, now time.Time) {
	p.log.Error("outbound message may already be in the thread, not sending it again",
		"id", po.rec.ID, "kind", po.rec.Kind, "thread_ts", po.rec.ThreadTS, "error", err)
	po.entry.Action, po.entry.Error = journal.ActionError, "acceptance unknown: "+err.Error()
	p.record(po.entry)
	p.writeReceipt(outbox.Receipt{ID: po.rec.ID, Status: outbox.StatusUnknown, Reason: err.Error(), Attempts: 1, At: now})
	if p.alerts != nil {
		p.alerts.Alert(context.WithoutCancel(context.Background()), "outbox:"+po.rec.ID, "handoffd",
			fmt.Sprintf("outbound %s for %s: acceptance unknown, nothing resent — check the thread: %s",
				po.rec.Kind, po.rec.ThreadTS, err))
	}
}

func (p *Pipeline) writeReceipt(rc outbox.Receipt) {
	if err := p.outbox.WriteReceipt(rc); err != nil {
		p.log.Error("outbox receipt not written, the record can be delivered again", "id", rc.ID, "status", rc.Status, "error", err)
	}
}

func (p *Pipeline) postDelivered(po *post, err error, permalink, ts string, now time.Time) {
	p.log.Error("outbound message delivered but its round transition was not committed", "id", po.rec.ID, "error", err)
	p.writeReceipt(outbox.Receipt{
		ID: po.rec.ID, Status: outbox.StatusPending, Reason: "delivered, transition not committed: " + err.Error(),
		Permalink: permalink, TS: ts, Attempts: postAttempts, At: now,
	})
	if p.alerts != nil {
		p.alerts.Alert(context.WithoutCancel(context.Background()), "outbox:"+po.rec.ID, "handoffd",
			fmt.Sprintf("outbound %s for %s reached the thread but its round transition was not saved: %s — handoffd retry duplicates the message",
				po.rec.Kind, po.rec.ThreadTS, err))
	}
}

func (p *Pipeline) recordPost(po *post, text, permalink, ts string, now time.Time) error {
	p.rememberPost(po.rec)
	po.entry.Action, po.entry.Reason, po.entry.MsgTS = journal.ActionPosted, permalink, ts
	if po.stale {
		po.entry.Reason = "stale: head is " + po.thread.Run.HeadSHA
	}
	p.record(po.entry)
	if po.rec.Kind == outbox.KindReply {
		return nil
	}
	err := p.store.UpdateThread(po.rec.ThreadTS, func(th *state.Thread) error {
		if roundOf(*th) != po.round {
			return errRoundReplaced
		}
		*th = p.applyPost(*th, po, permalink, ts, now)
		return nil
	})
	switch {
	case errors.Is(err, errRoundReplaced):
		e := p.progressEntry(po, now)
		e.Action, e.Reason = journal.ActionStale, "the round was replaced while the message was in flight"
		p.record(e)
		p.log.Warn("outbound message delivered into a round that was replaced meanwhile",
			"id", po.rec.ID, "kind", po.rec.Kind, "thread_ts", po.rec.ThreadTS, "round", po.round)
		return nil
	case err != nil:
		p.forgetPost(po.rec)
		return err
	}
	p.recordProgress(po, now)
	p.log.Info("outbound message delivered", "id", po.rec.ID, "kind", po.rec.Kind, "thread_ts", po.rec.ThreadTS, "agent", po.agent, "permalink", permalink, "text", journal.Snippet(text))
	return nil
}

func (p *Pipeline) applyPost(thread state.Thread, po *post, permalink, ts string, now time.Time) state.Thread {
	thread.HeadSHA, thread.BaseSHA = po.thread.HeadSHA, po.thread.BaseSHA
	switch {
	case po.thread.Run == nil:
	case roundOf(thread) != roundOf(po.thread):
		thread.OpenRound(po.thread.Run.HeadSHA, po.thread.Run.BaseSHA, po.thread.Run.OpenedBy, po.thread.Run.StartedAt)
	default:
		thread.Run.HeadSHA, thread.Run.BaseSHA = po.thread.Run.HeadSHA, po.thread.Run.BaseSHA
	}
	switch po.rec.Kind {
	case outbox.KindMRNote:
		thread.Run.Notes = append(thread.Run.Notes, state.Note{Agent: po.agent, SHA: po.rec.SHA, Text: journal.Snippet(po.rec.Text), At: now})
	case outbox.KindMarker:
		if po.rec.Phase == outbox.PhaseReviewStart {
			thread.Run.ReviewStarted = true
		}
		thread = p.markProgress(thread, po.rec.Phase, po.rec.SHA, p.tag(po), ts, now)
	case outbox.KindReviewDone:
		if po.stale {
			break
		}
		if po.driver {
			thread.Run.Joint = &state.Joint{SHA: po.rec.SHA, At: now, Permalink: permalink}
		} else {
			thread.Run.Record(po.agent, state.Result{
				SHA: po.rec.SHA, Blockers: po.rec.Blockers, Others: po.rec.Others, Decision: po.rec.Decision,
				Findings: len(thread.Run.NotesBy(po.agent)), At: now,
			})
		}
		thread = p.markProgress(thread, journal.PhaseReviewDone, po.rec.SHA, p.tag(po), ts, now)
	}
	thread.UpdatedAt = now
	return thread
}

func (p *Pipeline) recordProgress(po *post, now time.Time) {
	e := p.progressEntry(po, now)
	switch {
	case po.rec.Kind == outbox.KindMarker:
	case po.rec.Kind == outbox.KindReviewDone && po.stale:
		e.Action, e.Reason = journal.ActionStale, "head is "+po.thread.Run.HeadSHA
	case po.rec.Kind == outbox.KindReviewDone:
	default:
		return
	}
	p.record(e)
}

func (p *Pipeline) progressEntry(po *post, now time.Time) journal.Entry {
	phase := po.rec.Phase
	if po.rec.Kind == outbox.KindReviewDone {
		phase = journal.PhaseReviewDone
	}
	return journal.Entry{
		At: now, ID: "progress_" + po.rec.ID, Source: journal.SourceOutbox, Channel: po.thread.Channel, ThreadTS: po.rec.ThreadTS,
		MsgTS: po.entry.MsgTS, Kind: "review", Action: journal.ActionProgress, Reason: phase, Phase: phase, HeadSHA: po.rec.SHA,
		Agent: p.tag(po), SessionID: po.rec.SessionID, WindowID: po.thread.WindowID, WindowName: po.thread.WindowName,
		Worktree: po.thread.Worktree, Round: po.thread.Run.Round,
	}
}

func (p *Pipeline) markProgress(thread state.Thread, phase, sha, agent, ts string, now time.Time) state.Thread {
	if agent == "" {
		thread.Progress, thread.ProgressSHA, thread.ProgressAt = phase, sha, now
	}
	for i := range thread.Members {
		if thread.Members[i].Agent == agent {
			thread.Members[i].Progress, thread.Members[i].ProgressSHA = phase, sha
		}
	}
	if ts != "" {
		thread.LastSeenTS = ts
	}
	return thread
}

func (p *Pipeline) collectResults(ctx context.Context, root string) {
	thread, ok := p.store.Thread(root)
	if !ok || thread.Run == nil || thread.Run.Joint != nil || len(thread.Members) == 0 {
		return
	}
	if missing := p.missingResults(ctx, thread); len(missing) > 0 {
		p.log.Info("committee round still waiting", "thread_ts", root, "round", thread.Run.Round, "waiting", missing)
		return
	}
	text, err := p.render.Results(p.resultsContext(root, thread))
	if err != nil {
		p.log.Warn("results prompt not rendered", "thread_ts", root, "error", err)
		return
	}
	if _, err := p.deliver(ctx, thread, text); err != nil {
		p.log.Warn("results not delivered to the driver", "thread_ts", root, "window", thread.WindowID, "error", err)
		return
	}
	p.log.Info("committee round collected, driver asked for the joint verdict", "thread_ts", root, "round", thread.Run.Round)
}

func (p *Pipeline) resultsContext(root string, thread state.Thread) prompt.ResultsContext {
	rc := prompt.ResultsContext{Root: root, Round: thread.Run.Round, HeadSHA: thread.Run.HeadSHA, PostBin: p.postBin()}
	for _, m := range thread.Members {
		row := prompt.ResultRow{Agent: m.Agent}
		if res, ok := thread.Run.Results[m.Agent]; ok {
			row.Submitted, row.Blockers, row.Others, row.Decision = true, res.Blockers, res.Others, res.Decision
		}
		for _, n := range thread.Run.NotesBy(m.Agent) {
			row.Notes = append(row.Notes, n.Text)
		}
		rc.Rows = append(rc.Rows, row)
	}
	return rc
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
