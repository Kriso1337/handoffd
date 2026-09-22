package pipeline

import (
	"context"
	"errors"
	"fmt"

	"github.com/Kriso1337/handoffd/internal/mrref"
	"github.com/Kriso1337/handoffd/internal/prompt"
	"github.com/Kriso1337/handoffd/internal/session"
	"github.com/Kriso1337/handoffd/internal/state"
	"github.com/Kriso1337/handoffd/internal/worktree"
)

type workspace struct {
	cwd     string
	addDirs []string
	primary worktree.Result
	mr      *mrref.MR
}

func (p *Pipeline) workspace(ctx context.Context, refs mrref.Refs, pctx *prompt.Context) (workspace, error) {
	return p.workspaceExcluding(ctx, refs, pctx, nil)
}

func (p *Pipeline) workspaceExcluding(ctx context.Context, refs mrref.Refs, pctx *prompt.Context, taken map[string]bool) (workspace, error) {
	ws := workspace{cwd: p.cfg.ProjectsDir}
	if len(refs.MRs) == 0 {
		return ws, nil
	}
	busy := p.busyWorktrees(ctx)
	for path := range taken {
		busy[path] = true
	}
	var primaryErr error
	for i, mr := range refs.MRs {
		result, err := p.prepareWorktree(ctx, mr, busy)
		if err != nil {
			p.log.Warn("worktree not prepared", "project", mr.Project, "mr", mr.IID, "error", err)
		}
		if i == 0 {
			ws.primary, ws.mr, primaryErr = result, &refs.MRs[0], err
			continue
		}
		if result.Path != "" {
			ws.addDirs = append(ws.addDirs, result.Path)
		}
	}
	pctx.Worktree = ws.primary.Path
	applyRevision(pctx, ws.primary)
	if ws.primary.Path != "" {
		ws.cwd = ws.primary.Path
	}
	if primaryErr != nil {
		return ws, fmt.Errorf("worktree for %s: %w", refs.MRs[0].Slug(), primaryErr)
	}
	return ws, nil
}

func applyRevision(pctx *prompt.Context, res worktree.Result) {
	pctx.WorktreeSHA = res.SHA
	pctx.HeadSHA = res.HeadSHA
	pctx.BaseSHA = res.BaseSHA
	pctx.TargetBranch = res.TargetBranch
	pctx.WorktreeDirty = res.Dirty
	pctx.WorktreeNote = res.Note
}

func (p *Pipeline) prepareWorktree(ctx context.Context, mr mrref.MR, busy map[string]bool) (worktree.Result, error) {
	if !p.cfg.Allows(mr) {
		note := fmt.Sprintf("%s %s is not in %s, no worktree for %s", mr.ForgeName(), mr.Host, p.cfg.HostsKey(mr), mr.Slug())
		return worktree.Result{Note: note}, errors.New(note)
	}
	return p.worktrees.Prepare(ctx, mr, busy)
}

func (p *Pipeline) busyWorktrees(ctx context.Context) map[string]bool {
	busy := map[string]bool{}
	windows, err := p.windows.Windows(ctx)
	if err != nil {
		p.log.Warn("window list unavailable, worktrees treated as free", "error", err)
		return busy
	}
	live := map[string]bool{}
	for _, w := range windows {
		if w.MayHoldAgent() {
			live[w.ID] = true
		}
	}
	for _, th := range p.store.Threads() {
		if th.Worktree != "" && live[th.WindowID] {
			busy[th.Worktree] = true
		}
		for _, m := range th.Members {
			if m.Worktree != "" && live[m.WindowID] {
				busy[m.Worktree] = true
			}
		}
	}
	return busy
}

func (p *Pipeline) syncWorktree(ctx context.Context, thread state.Thread, pctx *prompt.Context) state.Thread {
	var (
		res worktree.Result
		err error
	)
	if p.sessionState(thread.SessionID) == session.Idle {
		res, err = p.worktrees.Refresh(ctx, thread.Worktree, *thread.MR)
	} else {
		res, err = p.worktrees.Inspect(ctx, thread.Worktree, *thread.MR)
	}
	if err != nil {
		p.log.Warn("worktree sync incomplete", "worktree", thread.Worktree, "error", err)
	}
	applyRevision(pctx, res)
	if res.HeadSHA != "" {
		thread.HeadSHA = res.HeadSHA
	}
	if res.BaseSHA != "" {
		thread.BaseSHA = res.BaseSHA
	}
	if thread.Run != nil && res.HeadSHA != "" && thread.Run.HeadSHA != "" && !mrref.SameCommit(res.HeadSHA, thread.Run.HeadSHA) {
		run := thread.OpenRound(res.HeadSHA, res.BaseSHA, state.RunOpenedByHeadChanged, p.now().UTC())
		pctx.NewRound = true
		p.log.Info("MR head moved, new review round", "worktree", thread.Worktree, "round", run.Round, "head", res.HeadSHA)
	}
	p.log.Info("worktree synced for continuation",
		"worktree", thread.Worktree, "sha", res.SHA, "head", res.HeadSHA, "base", res.BaseSHA, "dirty", res.Dirty, "note", res.Note)
	return thread
}
