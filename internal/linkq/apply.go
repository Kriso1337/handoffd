package linkq

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Kriso1337/handoffd/internal/launcher"
	"github.com/Kriso1337/handoffd/internal/mention"
	"github.com/Kriso1337/handoffd/internal/mrref"
	"github.com/Kriso1337/handoffd/internal/naming"
	"github.com/Kriso1337/handoffd/internal/state"
	"github.com/Kriso1337/handoffd/internal/worktree"
)

type Windows interface {
	Windows(ctx context.Context) ([]launcher.Window, error)
	Find(ctx context.Context, id string) (launcher.Window, bool, error)
	Rename(ctx context.Context, id, name string) error
}

type Revisions interface {
	Revision(ctx context.Context, mr mrref.MR) (worktree.Result, error)
}

type Deps struct {
	Windows       Windows
	Revisions     Revisions
	ReviewEnabled bool
	Allows        func(mr mrref.MR) bool
	Store         *state.Store
	Now           func() time.Time
	NewID         func() string
	Released      func(rootTS string, thread state.Thread)
}

func Apply(ctx context.Context, req Request, deps Deps) (Receipt, error) {
	if err := req.Validate(); err != nil {
		return Receipt{}, err
	}
	kind, _ := ParseKind(req.Kind)
	now := deps.Now()
	if kind == mention.KindReview && !deps.ReviewEnabled {
		return rejected(req, now, "review workflows are disabled; set review_enabled to true"), nil
	}

	window, ok, err := deps.Windows.Find(ctx, req.WindowID)
	if err != nil {
		return Receipt{}, err
	}
	if !ok || !window.MayHoldAgent() {
		return rejected(req, now, fmt.Sprintf("window %s no longer holds an agent", req.WindowID)), nil
	}

	held, holder, err := deps.holder(ctx, req)
	if err != nil {
		return Receipt{}, err
	}
	if holder != nil && !req.Force {
		return rejected(req, now, fmt.Sprintf("thread %s is held by window %s (%s); pass --force to take it over",
			req.ThreadTS, holder.ID, holder.Name)), nil
	}

	rev, reason, err := deps.revision(ctx, req, kind)
	if err != nil {
		return Receipt{}, err
	}
	if reason != "" {
		return rejected(req, now, reason), nil
	}

	name, err := deps.name(ctx, req, kind, now)
	if err != nil {
		return Receipt{}, err
	}
	if err := deps.Windows.Rename(ctx, window.ID, name); err != nil {
		return Receipt{}, err
	}

	if holder != nil && deps.Released != nil {
		deps.Released(req.ThreadTS, held)
	}
	thread := bind(held, req, window.ID, name, deps.session(window.ID), now)
	if kind == mention.KindReview {
		pin(&thread, rev, now)
	}
	deps.Store.SetThread(req.ThreadTS, thread)
	if err := deps.release(req.ThreadTS, window.ID, now); err != nil {
		return Receipt{}, err
	}
	if err := deps.Store.Save(); err != nil {
		return Receipt{}, err
	}
	return Receipt{
		ID: req.ID, Status: StatusLinked, WindowID: window.ID,
		WindowName: name, SessionID: thread.SessionID, At: now.UTC(),
	}, nil
}

func bind(thread state.Thread, req Request, windowID, name, session string, now time.Time) state.Thread {
	thread.WindowID, thread.WindowName, thread.SessionID = windowID, name, session
	thread.Worktree, thread.Kind, thread.Channel = req.Worktree, req.Kind, req.Channel
	thread.OpenedTS, thread.UpdatedAt = req.ThreadTS, now.UTC()
	if thread.LastSeenTS == "" {
		thread.LastSeenTS = req.ThreadTS
	}
	if req.Subject != "" {
		thread.Subject = req.Subject
	}
	if mr, ok := req.Refs.Primary(); ok {
		thread.MR, thread.MergeRef = &mr, mr.URL()
	}
	return thread
}

func pin(thread *state.Thread, rev worktree.Result, now time.Time) {
	thread.HeadSHA, thread.BaseSHA = rev.HeadSHA, rev.BaseSHA
	run := thread.Run
	if run == nil {
		return
	}
	switch {
	case run.HeadSHA == "":
		pinned := *run
		pinned.HeadSHA, pinned.BaseSHA = rev.HeadSHA, rev.BaseSHA
		thread.Run = &pinned
	case !mrref.SameCommit(run.HeadSHA, rev.HeadSHA):
		thread.OpenRound(rev.HeadSHA, rev.BaseSHA, state.RunOpenedByHeadChanged, now.UTC())
	case run.BaseSHA != "" && !mrref.SameCommit(run.BaseSHA, rev.BaseSHA):
		thread.OpenRound(rev.HeadSHA, rev.BaseSHA, state.RunOpenedByBaseChanged, now.UTC())
	}
}

func (d Deps) revision(ctx context.Context, req Request, kind mention.Kind) (worktree.Result, string, error) {
	if kind != mention.KindReview {
		return worktree.Result{}, "", nil
	}
	mr, ok := req.Refs.Primary()
	if !ok {
		return worktree.Result{}, "a review needs an MR link to pin its revision", nil
	}
	if d.Allows != nil && !d.Allows(mr) {
		return worktree.Result{}, fmt.Sprintf("%s is not a forge this watcher reviews", mr.Host), nil
	}
	if d.Revisions == nil {
		return worktree.Result{}, "", errors.New("no revision source to pin a review")
	}
	res, err := d.Revisions.Revision(ctx, mr)
	if err != nil {
		return worktree.Result{}, fmt.Sprintf("revision of %s is unreadable: %v", mr.Slug(), err), nil
	}
	if res.HeadSHA == "" || res.BaseSHA == "" {
		return worktree.Result{}, fmt.Sprintf("%s has no confirmed head and base", mr.Slug()), nil
	}
	return res, "", nil
}

func (d Deps) holder(ctx context.Context, req Request) (state.Thread, *launcher.Window, error) {
	thread, ok := d.Store.Thread(req.ThreadTS)
	if !ok || thread.WindowGone() || thread.WindowID == req.WindowID {
		return thread, nil, nil
	}
	window, ok, err := d.Windows.Find(ctx, thread.WindowID)
	if err != nil || !ok || !window.MayHoldAgent() {
		return thread, nil, err
	}
	return thread, &window, nil
}

func (d Deps) name(ctx context.Context, req Request, kind mention.Kind, now time.Time) (string, error) {
	windows, err := d.Windows.Windows(ctx)
	if err != nil {
		return "", err
	}
	taken := make(map[string]bool, len(windows))
	for _, w := range windows {
		if w.ID != req.WindowID {
			taken[w.Name] = true
		}
	}
	in := naming.Input{Kind: kind, Refs: req.Refs, Author: req.Label, IsDM: true, At: now}
	return naming.Window(in, func(name string) bool { return taken[name] }), nil
}

func (d Deps) release(linked, windowID string, now time.Time) error {
	for root, thread := range d.Store.Threads() {
		if root == linked || thread.WindowID != windowID {
			continue
		}
		err := d.Store.UpdateThread(root, func(th *state.Thread) error {
			th.WindowID, th.WindowName, th.SessionID = "", "", ""
			th.UpdatedAt = now.UTC()
			return nil
		})
		if err != nil {
			return fmt.Errorf("release %s from %s: %w", windowID, root, err)
		}
	}
	return nil
}

func (d Deps) session(windowID string) string {
	for _, thread := range d.Store.Threads() {
		if thread.WindowID == windowID && thread.SessionID != "" {
			return thread.SessionID
		}
	}
	return d.NewID()
}

func rejected(req Request, now time.Time, reason string) Receipt {
	return Receipt{ID: req.ID, Status: StatusRejected, Reason: reason, At: now.UTC()}
}
