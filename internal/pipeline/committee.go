package pipeline

import (
	"context"
	"strings"

	"github.com/Kriso1337/handoffd/internal/mention"
	"github.com/Kriso1337/handoffd/internal/naming"
	"github.com/Kriso1337/handoffd/internal/prompt"
	"github.com/Kriso1337/handoffd/internal/promptfile"
	"github.com/Kriso1337/handoffd/internal/state"
)

func (p *Pipeline) committee(res mention.Result, text string) []string {
	marker := strings.ToLower(strings.TrimSpace(p.cfg.CommitteeMarker))
	if res.Kind != mention.KindReview || marker == "" || len(p.cfg.CommitteeAgents) < 2 || !strings.Contains(strings.ToLower(text), marker) {
		return nil
	}
	var members []string
	for _, name := range p.cfg.CommitteeAgents {
		if p.agents[name] {
			members = append(members, name)
		} else {
			p.log.Warn("committee agent is not installed, reviewing without it", "agent", name)
		}
	}
	if len(members) < 2 {
		p.log.Warn("committee needs two installed agents, falling back to a single review", "installed", members)
		return nil
	}
	return members
}

type memberPlan struct {
	name string
	ws   workspace
	pctx prompt.Context
}

func (p *Pipeline) openCommitteeMembers(
	ctx context.Context,
	res mention.Result,
	pctx prompt.Context,
	ws workspace,
	denied []string,
	members []string,
) []state.Member {
	taken := map[string]bool{}
	if ws.primary.Path != "" {
		taken[ws.primary.Path] = true
	}
	var plans []memberPlan
	for _, name := range members[1:] {
		mctx := pctx
		mws, err := p.workspaceExcluding(ctx, res.Refs, &mctx, taken)
		if err != nil {
			p.log.Warn("committee member has no exact worktree, left out", "agent", name, "error", err)
			continue
		}
		if mws.primary.Path != "" {
			taken[mws.primary.Path] = true
		}
		plans = append(plans, memberPlan{name: name, ws: mws, pctx: mctx})
	}
	roster := []string{members[0]}
	for _, plan := range plans {
		roster = append(roster, plan.name)
	}
	var opened []state.Member
	for _, plan := range plans {
		plan.pctx.Committee = prompt.Committee{Agent: plan.name, Peers: strings.Join(without(roster, plan.name), ", "), Driver: false, DriverName: members[0]}
		text, err := p.render.Initial(plan.pctx)
		if err != nil {
			p.log.Warn("committee member prompt not rendered", "agent", plan.name, "error", err)
			continue
		}
		settings, err := p.settings(mention.KindReview)
		if err != nil {
			p.log.Warn("committee member settings not built", "agent", plan.name, "error", err)
			continue
		}
		sessionID := p.newID()
		file, err := p.prompts.Write(promptfile.Payload{
			Prompt: text, AddDirs: plan.ws.addDirs, Model: p.cfg.ModelFor(plan.name), DeniedTools: denied,
			SessionID: sessionID, Settings: settings, Agent: plan.name,
		})
		if err != nil {
			p.log.Warn("committee member prompt not written", "agent", plan.name, "error", err)
			continue
		}
		windowName := p.windowName(ctx, naming.Input{
			Kind: mention.KindReview, Refs: res.Refs, Author: pctx.AuthorName, ChannelLabel: pctx.ChannelName, IsDM: pctx.IsDM, At: p.now(), Tag: plan.name,
		})
		windowID, err := p.windows.NewWindow(ctx, windowName, plan.ws.cwd, file)
		if windowID == "" && err != nil {
			p.log.Warn("committee member window not opened", "agent", plan.name, "error", err)
			continue
		}
		opened = append(opened, state.Member{Agent: plan.name, WindowID: windowID, WindowName: windowName, SessionID: sessionID, Worktree: plan.ws.primary.Path})
		p.log.Info("committee member window opened", "agent", plan.name, "window", windowID, "name", windowName, "cwd", plan.ws.cwd, "session", sessionID)
	}
	return opened
}

func (p *Pipeline) attachMembers(root, driver string, opened []state.Member, out *outcome) {
	out.entry.Reason = "committee: " + strings.Join(append([]string{driver}, agentNames(opened)...), ", ")
	err := p.store.UpdateThread(root, func(th *state.Thread) error {
		th.Members = opened
		return nil
	})
	if err != nil {
		p.log.Warn("committee members not attached", "thread_ts", root, "error", err)
	}
}

func agentNames(members []state.Member) []string {
	out := make([]string, 0, len(members))
	for _, m := range members {
		out = append(out, m.Agent)
	}
	return out
}

func without(names []string, skip string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n != skip {
			out = append(out, n)
		}
	}
	return out
}

func (p *Pipeline) deliverToMembers(ctx context.Context, thread state.Thread, text string) {
	for _, m := range thread.Members {
		window, ok, err := p.windows.Find(ctx, m.WindowID)
		if err != nil || !ok || !window.MayHoldAgent() {
			continue
		}
		member := state.Thread{WindowID: m.WindowID, WindowName: m.WindowName, SessionID: m.SessionID}
		if _, err := p.deliver(ctx, member, text); err != nil {
			p.log.Warn("continuation not delivered to committee member", "agent", m.Agent, "window", m.WindowID, "error", err)
		}
	}
}
