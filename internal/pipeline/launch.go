package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/Kriso1337/handoffd/internal/journal"
	"github.com/Kriso1337/handoffd/internal/mention"
	"github.com/Kriso1337/handoffd/internal/naming"
	"github.com/Kriso1337/handoffd/internal/notifylog"
	"github.com/Kriso1337/handoffd/internal/prompt"
	"github.com/Kriso1337/handoffd/internal/promptfile"
	"github.com/Kriso1337/handoffd/internal/session"
	"github.com/Kriso1337/handoffd/internal/slackfetch"
	"github.com/Kriso1337/handoffd/internal/state"
)

func (p *Pipeline) escalate(
	ctx context.Context,
	root string,
	res mention.Result,
	msg slackfetch.Message,
	pctx prompt.Context,
	task, reason string,
	out *outcome,
) error {
	ws, err := p.workspace(ctx, res.Refs, &pctx)
	if err != nil {
		p.log.Warn("help window opens without an exact worktree", "error", err)
	} else if ws.mr != nil {
		p.log.Info("worktree prepared for escalation", "project", ws.mr.Project, "mr", ws.mr.IID)
	}

	promptReason := reason
	if pctx.Macro || pctx.VerdictMissing {
		promptReason = ""
	}
	text, err := p.render.Ask(pctx, task, promptReason)
	if err != nil {
		return err
	}
	name := p.windowName(ctx, naming.Input{
		Kind:         mention.KindHelp,
		Refs:         res.Refs,
		Author:       msg.AuthorName,
		ChannelLabel: msg.ChannelLabel,
		IsDM:         msg.IsDM,
		At:           p.now(),
	})
	if err := p.launch(ctx, root, mention.KindHelp, name, text, ws, pctx, nil, "", out); err != nil {
		return err
	}
	out.done(journal.ActionEscalated, reason)
	return nil
}

func (p *Pipeline) openWindow(
	ctx context.Context,
	n notifylog.Notification,
	root string,
	res mention.Result,
	pctx prompt.Context,
	out *outcome,
) error {
	if res.Kind == mention.KindOther {
		if !p.store.AllowLaunch(p.now(), p.cfg.OtherLimit, p.cfg.OtherWindow()) {
			p.log.Warn("rate limit reached, window not opened", "ts", n.MsgTS)
			out.done(journal.ActionDropped, "launch rate limit")
			return nil
		}
	}

	ws := workspace{cwd: p.cfg.ProjectsDir}
	var denied []string
	if res.Kind == mention.KindReview {
		if len(res.Refs.MRs) == 0 {
			p.log.Warn("review refused, no MR link in the message", "thread_ts", root, "ts", n.MsgTS)
			out.done(journal.ActionDropped, "review refused: no MR link in the message")
			return nil
		}
		var err error
		if ws, err = p.workspace(ctx, res.Refs, &pctx); err != nil {
			return fmt.Errorf("review refused without an exact worktree: %w", err)
		}
		denied = p.cfg.ReviewDeniedTools
	}

	members := p.committee(res, pctx.Text)
	var opened []state.Member
	if len(members) > 1 {
		opened = p.openCommitteeMembers(ctx, res, pctx, ws, denied, members)
		if len(opened) == 0 {
			p.log.Warn("no committee member window opened, falling back to a single review", "planned", members[1:])
			members = nil
		} else {
			pctx.Committee = prompt.Committee{Agent: members[0], Peers: strings.Join(agentNames(opened), ", "), Driver: true, DriverName: members[0]}
		}
	}
	text, err := p.render.Initial(pctx)
	if err != nil {
		return err
	}
	name := p.windowName(ctx, naming.Input{
		Kind:         res.Kind,
		Refs:         res.Refs,
		Author:       pctx.AuthorName,
		ChannelLabel: pctx.ChannelName,
		IsDM:         pctx.IsDM,
		At:           p.now(),
	})
	if err := p.launch(ctx, root, res.Kind, name, text, ws, pctx, denied, firstOf(members), out); err != nil {
		return err
	}
	out.done(journal.ActionOpened, "")
	if len(opened) > 0 {
		p.attachMembers(root, members[0], opened, out)
	}
	if res.Kind == mention.KindOther {
		p.store.RecordLaunch(p.now(), p.cfg.OtherWindow())
		return p.store.Save()
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstOf(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func (p *Pipeline) launch(
	ctx context.Context,
	root string,
	kind mention.Kind,
	name, text string,
	ws workspace,
	pctx prompt.Context,
	denied []string,
	agentName string,
	out *outcome,
) error {
	if out.resume != nil && out.resume.Kind == mention.KindReview.String() {
		kind = mention.KindReview
		if denied == nil {
			denied = p.cfg.ReviewDeniedTools
		}
	}
	sessionID := p.newID()
	settings, err := p.settings(kind)
	if err != nil {
		return err
	}
	file, err := p.prompts.Write(promptfile.Payload{
		Prompt:      text,
		AddDirs:     ws.addDirs,
		Model:       p.cfg.ModelFor(agentName),
		DeniedTools: denied,
		SessionID:   sessionID,
		Settings:    settings,
		Agent:       agentName,
	})
	if err != nil {
		return err
	}

	windowID, err := p.windows.NewWindow(ctx, name, ws.cwd, file)
	if windowID == "" && err != nil {
		return err
	}
	if err != nil {
		p.log.Warn("window created with a warning", "window", windowID, "error", err)
	}

	thread := state.Thread{
		WindowID:   windowID,
		WindowName: name,
		Agent:      firstNonEmpty(agentName, p.cfg.Agent),
		Worktree:   pctx.Worktree,
		MergeRef:   pctx.MergeRef,
		MR:         ws.mr,
		HeadSHA:    ws.primary.HeadSHA,
		BaseSHA:    ws.primary.BaseSHA,
		SessionID:  sessionID,
		Kind:       kind.String(),
		TeamID:     out.notification.TeamID,
		Channel:    pctx.Channel,
		Subject:    journal.Snippet(pctx.Text),
		OpenedTS:   pctx.MsgTS,
		LastSeenTS: pctx.MsgTS,
		UpdatedAt:  p.now().UTC(),
	}
	if kind == mention.KindReview {
		thread.OpenRound(ws.primary.HeadSHA, ws.primary.BaseSHA, state.RunOpenedByHandoff, p.now().UTC())
	}
	p.store.SetThread(root, thread)
	p.log.Info("window opened",
		"window", windowID, "name", name, "kind", kind.String(), "cwd", ws.cwd,
		"session", sessionID, "ts", pctx.MsgTS)
	out.entry.WindowID, out.entry.WindowName, out.entry.SessionID = windowID, name, sessionID
	out.entry.Worktree, out.entry.HeadSHA = pctx.Worktree, ws.primary.HeadSHA
	return p.store.Save()
}

func (p *Pipeline) settings(kind mention.Kind) (string, error) {
	if kind != mention.KindReview {
		return p.hookSettings, nil
	}
	return session.WithPermissions(p.hookSettings, p.cfg.ReviewAllowedTools, p.cfg.ReviewDeniedTools)
}

func (p *Pipeline) windowName(ctx context.Context, in naming.Input) string {
	windows, err := p.windows.Windows(ctx)
	if err != nil {
		p.log.Warn("window list unavailable, name collisions possible", "error", err)
	}
	taken := make(map[string]bool, len(windows))
	for _, w := range windows {
		taken[w.Name] = true
	}
	name := naming.Window(in, func(name string) bool { return taken[name] })
	if taken[name] {
		p.log.Warn("window name still collides", "name", name, "visible", len(windows))
	}
	return name
}
