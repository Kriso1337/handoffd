package pipeline

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/Kriso1337/handoffd/internal/journal"
	"github.com/Kriso1337/handoffd/internal/mention"
	"github.com/Kriso1337/handoffd/internal/naming"
	"github.com/Kriso1337/handoffd/internal/notifylog"
	"github.com/Kriso1337/handoffd/internal/prompt"
	"github.com/Kriso1337/handoffd/internal/promptfile"
	"github.com/Kriso1337/handoffd/internal/slackfetch"
	"github.com/Kriso1337/handoffd/internal/triage"
)

var (
	errWindowDied = errors.New("triage window exited before writing a verdict")
	errDeferred   = errors.New("message deferred behind a running triage")
	errNotBuried  = errors.New("outcome not recoverable, dead letter not written")
)

func (p *Pipeline) runTriage(
	ctx context.Context,
	n notifylog.Notification,
	root string,
	res mention.Result,
	msg slackfetch.Message,
	pctx prompt.Context,
	out *outcome,
) error {
	p.mu.Lock()
	if p.inflight[root] {
		p.deferred[root] = append(p.deferred[root], n)
		p.mu.Unlock()
		p.log.Info("triage already running for this thread, message deferred", "thread_ts", root, "ts", n.MsgTS)
		out.done(journal.ActionQueued, "triage in flight for this thread")
		return errDeferred
	}
	p.mu.Unlock()

	if p.cooledDown(root, res, msg.Text) {
		streak := p.store.SilentCount(root, p.now(), p.cfg.TriageCooldownWindow())
		p.log.Info("thread in cooldown after silent verdicts, message skipped", "thread_ts", root, "ts", n.MsgTS, "silent", streak)
		out.done(journal.ActionSkipped, fmt.Sprintf("thread cooldown after %d silent verdicts", streak))
		return nil
	}
	if !p.store.AllowTriage(p.now(), p.cfg.TriageLimit, p.cfg.OtherWindow()) {
		p.log.Warn("triage rate limit reached, message skipped",
			"channel", n.Channel, "ts", n.MsgTS)
		out.done(journal.ActionDropped, "triage rate limit")
		return nil
	}

	verdictPath := p.verdicts.Path(n.MsgTS)
	text, err := p.render.Triage(p.triageContext(pctx, msg, verdictPath))
	if err != nil {
		return err
	}
	file, err := p.prompts.Write(promptfile.Payload{
		Prompt:       text,
		Model:        p.cfg.TriageModelName(),
		AllowedTools: p.cfg.TriageToolList(),
		AddDirs:      []string{filepath.Dir(verdictPath)},
	})
	if err != nil {
		return err
	}

	name := p.windowName(ctx, naming.Input{
		Kind:         mention.KindTriage,
		Refs:         res.Refs,
		Author:       msg.AuthorName,
		ChannelLabel: msg.ChannelLabel,
		IsDM:         msg.IsDM,
		At:           p.now(),
	})
	windowID, err := p.windows.NewWindow(ctx, name, p.cfg.ProjectsDir, file)
	if windowID == "" && err != nil {
		return err
	}
	if err := p.windows.HoldOnExit(ctx, windowID); err != nil {
		p.log.Warn("triage window will vanish on exit, early death goes unnoticed", "window", windowID, "error", err)
	}
	p.store.RecordTriage(p.now(), p.cfg.OtherWindow())
	if err := p.store.Save(); err != nil {
		return err
	}
	p.log.Info("triage window opened", "window", windowID, "name", name, "ts", n.MsgTS)

	p.mu.Lock()
	p.inflight[root] = true
	p.mu.Unlock()
	pending := *out
	out.entry.Action = ""
	p.triages.Add(1)
	go func() {
		defer p.triages.Done()
		if buryErr := p.finishTriage(ctx, n, root, res, msg, pctx, &pending, windowID, verdictPath); buryErr != nil {
			p.log.Error("triage outcome kept in the inbox, dead letter not written", "id", n.ID, "error", buryErr)
		} else if ctx.Err() == nil {
			p.settle(n, pending.entry.Action)
		}
		p.replayDeferred(ctx, root)
	}()
	return nil
}

func (p *Pipeline) cooledDown(root string, res mention.Result, text string) bool {
	if p.cfg.TriageCooldownN <= 0 || res.Mentioned || res.Tagged || mention.Pointed(text) {
		return false
	}
	return p.store.SilentCount(root, p.now(), p.cfg.TriageCooldownWindow()) >= p.cfg.TriageCooldownN
}

func (p *Pipeline) finishTriage(
	ctx context.Context,
	n notifylog.Notification,
	root string,
	res mention.Result,
	msg slackfetch.Message,
	pctx prompt.Context,
	out *outcome,
	windowID, verdictPath string,
) (err error) {
	defer func() { err = errors.Join(err, p.finish(out)) }()

	started := time.Now()
	verdict, waitErr := p.awaitVerdict(ctx, windowID, verdictPath)
	result := journal.Triage{Outcome: journal.OutcomeVerdict}
	if waitErr != nil {
		result.Outcome = journal.OutcomeTimeout
		if errors.Is(waitErr, errWindowDied) {
			result.Outcome = journal.OutcomeWindowDied
		}
		if recovered, ok := p.verdictFromPane(ctx, windowID); ok {
			verdict, waitErr, result.Outcome = recovered, nil, journal.OutcomePane
		}
	}
	result.LatencyMS = time.Since(started).Milliseconds()
	result.React, result.Reason, result.Task = verdict.React, verdict.Reason, verdict.Task
	out.entry.Triage = &result
	if killErr := p.windows.KillWindow(context.WithoutCancel(ctx), windowID); killErr != nil {
		p.log.Warn("triage window not closed", "window", windowID, "error", killErr)
	}
	if ctx.Err() != nil {
		p.log.Info("triage interrupted while the watcher was stopping, the message stays admitted",
			"id", n.ID, "ts", n.MsgTS, "channel", n.Channel)
		out.entry.Action = ""
		return nil
	}
	if waitErr != nil && res.Tagged {
		p.log.Warn("no triage verdict but the message tags us directly, escalating anyway",
			"ts", n.MsgTS, "channel", n.Channel, "error", waitErr)
		verdict = triage.Verdict{React: true, Reason: "direct tag without a triage verdict: " + waitErr.Error()}
		pctx.VerdictMissing = true
		waitErr = nil
	}
	if waitErr != nil {
		p.log.Warn("triage gave no usable verdict, nothing escalated",
			"ts", n.MsgTS, "channel", n.Channel, "error", waitErr)
		out.done(journal.ActionDropped, "no triage verdict: "+waitErr.Error())
		return nil
	}
	if !verdict.React {
		streak := p.store.RecordSilent(root, p.now(), p.cfg.TriageCooldownWindow())
		p.log.Info("triage decided to stay silent",
			"ts", n.MsgTS, "channel", n.Channel, "reason", verdict.Reason, "silent_streak", streak)
		out.done(journal.ActionSilent, verdict.Reason)
		if saveErr := p.store.Save(); saveErr != nil {
			p.log.Warn("state not saved after silent verdict", "error", saveErr)
		}
		return nil
	}
	p.store.ClearSilent(root)
	if escalateErr := p.escalate(ctx, root, res, msg, pctx, verdict.Task, verdict.Reason, out); escalateErr != nil {
		p.log.Error("escalation after triage failed", "ts", n.MsgTS, "error", escalateErr)
		out.done(journal.ActionError, "escalation failed")
		out.entry.Error = escalateErr.Error()
		return p.bury(n, "escalation failed: "+escalateErr.Error())
	}
	return nil
}

func (p *Pipeline) replayDeferred(ctx context.Context, root string) {
	p.mu.Lock()
	delete(p.inflight, root)
	queued := p.deferred[root]
	delete(p.deferred, root)
	p.mu.Unlock()
	for _, n := range queued {
		if ctx.Err() != nil {
			return
		}
		p.log.Info("replaying message deferred behind triage", "thread_ts", root, "ts", n.MsgTS)
		if err := p.Handle(ctx, n); err != nil && ctx.Err() == nil {
			p.log.Error("deferred message not handled", "id", n.ID, "error", err)
		}
	}
}

func (p *Pipeline) awaitVerdict(ctx context.Context, windowID, path string) (triage.Verdict, error) {
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type outcome struct {
		verdict triage.Verdict
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		v, err := p.verdicts.Wait(waitCtx, path)
		done <- outcome{v, err}
	}()

	ticker := time.NewTicker(p.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case res := <-done:
			return res.verdict, res.err
		case <-ticker.C:
			window, found, err := p.windows.Find(ctx, windowID)
			if err != nil || (found && !window.Dead) {
				continue
			}
			cancel()
			if res := <-done; res.err == nil {
				return res.verdict, nil
			}
			if verdict, ok := p.verdicts.Peek(path); ok {
				return verdict, nil
			}
			p.log.Warn("triage window died before a verdict", "window", windowID, "found", found)
			return triage.Verdict{}, errWindowDied
		}
	}
}

func (p *Pipeline) verdictFromPane(ctx context.Context, windowID string) (triage.Verdict, bool) {
	pane, err := p.windows.CapturePane(ctx, windowID)
	if err != nil {
		return triage.Verdict{}, false
	}
	verdict, err := triage.FromPane(pane)
	if err != nil {
		return triage.Verdict{}, false
	}
	p.log.Warn("verdict taken from window output, no file was written", "window", windowID)
	return verdict, true
}

func (p *Pipeline) triageContext(pctx prompt.Context, msg slackfetch.Message, verdictPath string) prompt.TriageContext {
	tail := make([]prompt.TailReply, 0, len(msg.ThreadTail))
	rootInTail := false
	for _, r := range msg.ThreadTail {
		tail = append(tail, prompt.TailReply{AuthorName: r.AuthorName, AuthorID: r.AuthorID, Text: r.Text})
		if r.TS == pctx.ThreadTS {
			rootInTail = true
		}
	}
	rootAuthor, rootText := "", ""
	if msg.ThreadTS != "" && !rootInTail {
		rootAuthor, rootText = msg.RootAuthor, msg.RootText
	}
	lastFromSelf := len(msg.ThreadTail) > 0 && msg.ThreadTail[len(msg.ThreadTail)-1].AuthorID == p.cfg.SelfUserID
	return prompt.TriageContext{
		Context:      pctx,
		ChannelLabel: msg.ChannelLabel,
		IsDM:         msg.IsDM,
		SelfInThread: msg.SelfInThread,
		ThreadSize:   msg.ThreadSize,
		ThreadTail:   tail,
		VerdictPath:  verdictPath,
		Namesake:     p.cfg.NamesakeName(),
		PeerAgents:   p.cfg.PeerAgents,
		Examples:     p.examples(),
		RootAuthor:   rootAuthor,
		RootText:     rootText,
		LastFromSelf: lastFromSelf,
	}
}

func (p *Pipeline) examples() []prompt.Example {
	if p.labels == nil || p.cfg.TriageExamples <= 0 {
		return nil
	}
	recent, err := p.labels.Recent(p.cfg.TriageExamples)
	if err != nil {
		p.log.Warn("labels unreadable, triage runs without examples", "error", err)
		return nil
	}
	out := make([]prompt.Example, 0, len(recent))
	for _, l := range recent {
		out = append(out, prompt.Example{Text: l.Text, React: l.React})
	}
	return out
}
