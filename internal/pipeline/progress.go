package pipeline

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Kriso1337/handoffd/internal/journal"
	"github.com/Kriso1337/handoffd/internal/label"
	"github.com/Kriso1337/handoffd/internal/mention"
	"github.com/Kriso1337/handoffd/internal/mrref"
	"github.com/Kriso1337/handoffd/internal/session"
	"github.com/Kriso1337/handoffd/internal/state"
)

func (p *Pipeline) closeSessions(root string, thread state.Thread, reason string) {
	p.closeSession(root, thread.SessionID, reason)
	for _, m := range thread.Members {
		p.closeSession(root, m.SessionID, reason)
	}
}

func (p *Pipeline) closeSession(root, sessionID, reason string) {
	if sessionID == "" || p.journal == nil {
		return
	}
	var rec session.Record
	if p.sessions != nil {
		found, ok, err := p.sessions.Read(sessionID)
		if err != nil {
			p.log.Warn("session state unreadable", "session", sessionID, "error", err)
		}
		if ok && found.State == session.Ended {
			return
		}
		if ok {
			rec = found
		}
	}
	stats := rec.Stats(p.now())
	p.record(journal.Entry{
		ID: "session_" + sessionID, Source: journal.SourceWatcher, ThreadTS: root, Kind: "session",
		Action: journal.ActionSession, Reason: reason, SessionID: sessionID, Session: &stats,
	})
}

func (p *Pipeline) labelStop(thread state.Thread) {
	if p.labels == nil || thread.TeamID == "" || thread.OpenedTS == "" {
		return
	}
	err := p.labels.Append(label.Label{
		At: p.now().UTC(), ID: thread.TeamID + "_" + thread.OpenedTS, Verdict: label.Bad, React: false,
		Kind: thread.Kind, Action: journal.ActionOpened, Text: thread.Subject, Note: "stop phrase closed the window",
		Source: label.SourceStopPhrase,
	})
	if err != nil {
		p.log.Warn("stop phrase label not written", "thread_ts", thread.OpenedTS, "error", err)
	}
}

func (p *Pipeline) progress(ctx context.Context, root string, thread state.Thread, res mention.Result, msgText, ts string, out *outcome) error {
	now := p.now().UTC()
	agent := p.taggedAgent(thread, msgText)
	out.entry.WindowID, out.entry.WindowName, out.entry.SessionID = thread.WindowID, thread.WindowName, thread.SessionID
	out.entry.Worktree, out.entry.HeadSHA, out.entry.Phase, out.entry.Agent = thread.Worktree, res.ProgressSHA, res.Progress, agent
	if completes(res.Progress) && res.ProgressSHA != "" {
		reconciled, _, err := p.reconcileHead(ctx, thread)
		thread = reconciled
		if err != nil {
			p.log.Warn("provider head unknown, completion not accepted, round stays open",
				"thread_ts", root, "phase", res.Progress, "sha", res.ProgressSHA, "agent", agent, "error", err)
			out.done(journal.ActionPending, "provider head unknown: "+err.Error())
			return p.store.UpdateThread(root, p.pinRevision(thread, ts, now))
		}
		if thread.HeadSHA != "" && !mrref.SameCommit(res.ProgressSHA, thread.HeadSHA) {
			p.log.Warn("own completion marker names a stale sha, current round stays open",
				"thread_ts", root, "phase", res.Progress, "sha", res.ProgressSHA, "head", thread.HeadSHA, "agent", agent)
			out.done(journal.ActionStale, "head is "+thread.HeadSHA)
			return p.store.UpdateThread(root, p.pinRevision(thread, ts, now))
		}
	}
	p.log.Info("own progress marker recorded", "thread_ts", root, "phase", res.Progress, "sha", res.ProgressSHA, "agent", agent)
	out.done(journal.ActionProgress, res.Progress)
	return p.store.UpdateThread(root, func(th *state.Thread) error {
		if err := p.pinRevision(thread, ts, now)(th); err != nil {
			return err
		}
		if agent == "" {
			th.Progress, th.ProgressSHA, th.ProgressAt = res.Progress, res.ProgressSHA, now
		}
		for i := range th.Members {
			if th.Members[i].Agent == agent {
				th.Members[i].Progress, th.Members[i].ProgressSHA = res.Progress, res.ProgressSHA
			}
		}
		return nil
	})
}

func (p *Pipeline) pinRevision(read state.Thread, ts string, now time.Time) func(*state.Thread) error {
	return func(th *state.Thread) error {
		th.HeadSHA, th.BaseSHA = read.HeadSHA, read.BaseSHA
		th.LastSeenTS, th.UpdatedAt = ts, now
		return nil
	}
}

func completes(phase string) bool {
	return phase == mention.ProgressReviewDone || phase == mention.ProgressDone
}

func (p *Pipeline) reconcileHead(ctx context.Context, thread state.Thread) (state.Thread, bool, error) {
	if thread.MR == nil || thread.Worktree == "" {
		return thread, false, nil
	}
	res, err := p.worktrees.Inspect(ctx, thread.Worktree, *thread.MR)
	if res.HeadSHA != "" {
		thread.HeadSHA = res.HeadSHA
	}
	if res.BaseSHA != "" {
		thread.BaseSHA = res.BaseSHA
	}
	if res.HeadSHA == "" && err != nil {
		return thread, false, fmt.Errorf("provider head for %s not read: %w", thread.Worktree, err)
	}
	return thread, res.BaseSHA != "", nil
}

func (p *Pipeline) taggedAgent(thread state.Thread, text string) string {
	if len(thread.Members) == 0 {
		return ""
	}
	line, _, _ := strings.Cut(strings.ToLower(text), "\n")
	names := []string{}
	for _, m := range thread.Members {
		names = append(names, m.Agent)
	}
	for _, name := range append(p.cfg.CommitteeAgents, names...) {
		if name != "" && strings.Contains(line, "("+strings.ToLower(name)+")") {
			return name
		}
	}
	return ""
}
