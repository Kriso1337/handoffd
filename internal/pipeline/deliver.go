package pipeline

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Kriso1337/handoffd/internal/journal"
	"github.com/Kriso1337/handoffd/internal/mention"
	"github.com/Kriso1337/handoffd/internal/prompt"
	"github.com/Kriso1337/handoffd/internal/session"
	"github.com/Kriso1337/handoffd/internal/state"
	"github.com/Kriso1337/handoffd/internal/waitq"
)

func (p *Pipeline) continueInWindow(
	ctx context.Context,
	root string,
	thread state.Thread,
	pctx prompt.Context,
	out *outcome,
) error {
	pctx.Worktree = thread.Worktree
	if pctx.MergeRef == "" {
		pctx.MergeRef = thread.MergeRef
	}
	if pctx.Forge == "" && thread.MR != nil {
		pctx.Forge = thread.MR.Forge
	}
	if thread.Worktree != "" && thread.MR != nil {
		thread = p.syncWorktree(ctx, thread, &pctx)
	}
	if thread.Run != nil {
		pctx.Round = thread.Run.Round
	}

	reviewMode := thread.Kind == mention.KindReview.String()
	text, err := p.render.Continuation(pctx, reviewMode)
	if err != nil {
		return err
	}
	queued, err := p.deliver(ctx, thread, text)
	if err != nil {
		return err
	}
	p.deliverToMembers(ctx, thread, text)
	out.entry.WindowID, out.entry.WindowName, out.entry.SessionID = thread.WindowID, thread.WindowName, thread.SessionID
	out.entry.Worktree, out.entry.HeadSHA = thread.Worktree, thread.HeadSHA
	if queued {
		out.done(journal.ActionQueued, "session blocked")
	} else {
		out.done(journal.ActionContinued, "")
	}
	return p.store.UpdateThread(root, func(th *state.Thread) error {
		th.HeadSHA, th.BaseSHA = thread.HeadSHA, thread.BaseSHA
		if pctx.NewRound && thread.Run != nil && roundOf(*th) < thread.Run.Round {
			th.OpenRound(thread.Run.HeadSHA, thread.Run.BaseSHA, thread.Run.OpenedBy, thread.Run.StartedAt)
		}
		th.LastSeenTS, th.UpdatedAt = pctx.MsgTS, p.now().UTC()
		return nil
	})
}

func (p *Pipeline) sessionState(id string) session.State {
	if id == "" || p.sessions == nil {
		return session.Unknown
	}
	rec, ok, err := p.sessions.Read(id)
	if err != nil {
		p.log.Warn("session state unreadable", "session", id, "error", err)
	}
	if !ok {
		return session.Unknown
	}
	return rec.State
}

func (p *Pipeline) deliver(ctx context.Context, thread state.Thread, text string) (bool, error) {
	p.mu.Lock()
	waiting, err := p.waiting.Peek(thread.WindowID)
	if err != nil {
		p.mu.Unlock()
		return false, fmt.Errorf("waiting prompts for %s unreadable: %w", thread.WindowID, err)
	}
	if len(waiting) == 0 && p.sessionState(thread.SessionID) != session.Blocked {
		p.mu.Unlock()
		if err := p.windows.SendPrompt(ctx, thread.WindowID, text); err != nil {
			return false, err
		}
		p.log.Info("prompt sent to live window", "window", thread.WindowID, "name", thread.WindowName)
		return false, nil
	}
	queued := waitq.Prompt{
		ID: p.newID(), At: p.now().UTC(), WindowID: thread.WindowID,
		SessionID: thread.SessionID, ThreadTS: thread.OpenedTS, Text: text,
	}
	if err := p.waiting.Append(queued); err != nil {
		p.mu.Unlock()
		return false, fmt.Errorf("prompt for %s not queued: %w", thread.WindowID, err)
	}
	p.mu.Unlock()
	p.log.Info("session blocked on a dialog, prompt waits on disk",
		"window", thread.WindowID, "session", thread.SessionID, "queued", len(waiting)+1)
	if len(waiting) == 0 {
		go p.drain(context.WithoutCancel(ctx), thread.WindowID, thread.WindowName)
	}
	return true, nil
}

func (p *Pipeline) ResumeWaiting(ctx context.Context) {
	windows, err := p.waiting.Windows()
	if err != nil {
		p.log.Error("waiting prompts unreadable", "error", err)
		return
	}
	for _, id := range windows {
		window, ok, err := p.windows.Find(ctx, id)
		if err != nil {
			p.log.Warn("window of a waiting prompt not looked up", "window", id, "error", err)
			continue
		}
		if !ok || !window.MayHoldAgent() {
			p.keepUndelivered(id, errors.New("the window is gone"))
			continue
		}
		p.log.Info("resuming prompts that waited across a restart", "window", id, "name", window.Name)
		go p.drain(context.WithoutCancel(ctx), id, window.Name)
	}
}

func (p *Pipeline) drain(ctx context.Context, windowID, windowName string) {
	for {
		waiting, err := p.waiting.Peek(windowID)
		if err != nil {
			p.log.Error("waiting prompts unreadable", "window", windowID, "error", err)
			return
		}
		if len(waiting) == 0 {
			return
		}
		next := waiting[0]
		if err := p.awaitUnblocked(ctx, next.SessionID); err != nil {
			p.keepUndelivered(windowID, err)
			return
		}
		if err := p.windows.SendPrompt(ctx, windowID, next.Text); err != nil {
			p.keepUndelivered(windowID, err)
			return
		}
		if err := p.waiting.Remove(next.ID); err != nil {
			p.log.Error("delivered prompt not removed from the queue", "window", windowID, "id", next.ID, "error", err)
			return
		}
		p.log.Info("queued prompt delivered", "window", windowID, "name", windowName)
	}
}

func (p *Pipeline) awaitUnblocked(ctx context.Context, sessionID string) error {
	deadline := time.Now().Add(p.cfg.DeliverTimeout())
	for {
		switch p.sessionState(sessionID) {
		case session.Blocked:
		case session.Ended:
			return errors.New("session ended while a prompt was waiting")
		default:
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("session stayed blocked for %s", p.cfg.DeliverTimeout())
		}
		if !sleep(ctx, p.pollInterval) {
			return ctx.Err()
		}
	}
}

func (p *Pipeline) keepUndelivered(windowID string, cause error) {
	moved, err := p.waiting.Quarantine(windowID, cause.Error(), p.now().UTC())
	if err != nil {
		p.log.Error("undelivered prompts not kept aside", "window", windowID, "error", err)
	}
	if moved == 0 {
		return
	}
	p.log.Warn("queued prompts left undelivered", "window", windowID, "count", moved, "error", cause)
	if p.alerts != nil {
		p.alerts.Alert(context.WithoutCancel(context.Background()), "waiting:"+windowID, "handoffd",
			fmt.Sprintf("%d prompt(s) for %s not delivered: %s — they are kept in waiting/undelivered", moved, windowID, cause))
	}
}

func (p *Pipeline) Sweep(ctx context.Context) error {
	windows, err := p.windows.Windows(ctx)
	if err != nil {
		return fmt.Errorf("sweep threads: %w", err)
	}
	live := map[string]bool{}
	for _, w := range windows {
		if w.MayHoldAgent() {
			live[w.ID] = true
		}
	}
	before := p.store.Threads()
	dropped := p.store.Sweep(p.now(), p.cfg.ThreadTTL(), p.cfg.GoneThreadTTL(), func(th state.Thread) bool { return live[th.WindowID] })
	for _, root := range dropped {
		if th, ok := before[root]; ok && !th.WindowGone() {
			p.closeSessions(root, th, "swept")
		}
		p.log.Info("thread record dropped", "thread_ts", root)
	}
	if len(dropped) == 0 {
		return nil
	}
	return p.store.Save()
}
