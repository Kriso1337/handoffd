package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Kriso1337/handoffd/internal/config"
	"github.com/Kriso1337/handoffd/internal/deadletter"
	"github.com/Kriso1337/handoffd/internal/journal"
	"github.com/Kriso1337/handoffd/internal/label"
	"github.com/Kriso1337/handoffd/internal/launcher"
	"github.com/Kriso1337/handoffd/internal/linkq"
	"github.com/Kriso1337/handoffd/internal/mention"
	"github.com/Kriso1337/handoffd/internal/mrref"
	"github.com/Kriso1337/handoffd/internal/notifylog"
	"github.com/Kriso1337/handoffd/internal/prompt"
	"github.com/Kriso1337/handoffd/internal/promptfile"
	"github.com/Kriso1337/handoffd/internal/session"
	"github.com/Kriso1337/handoffd/internal/slackfetch"
	"github.com/Kriso1337/handoffd/internal/state"
	"github.com/Kriso1337/handoffd/internal/triage"
	"github.com/Kriso1337/handoffd/internal/waitq"
	"github.com/Kriso1337/handoffd/internal/worktree"
)

const handleAttempts = 3

type Fetcher interface {
	Fetch(ctx context.Context, channel, ts, threadTS string) (slackfetch.Message, error)
}

type Worktrees interface {
	Prepare(ctx context.Context, mr mrref.MR, busy map[string]bool) (worktree.Result, error)
	Revision(ctx context.Context, mr mrref.MR) (worktree.Result, error)
	Inspect(ctx context.Context, path string, mr mrref.MR) (worktree.Result, error)
	Refresh(ctx context.Context, path string, mr mrref.MR) (worktree.Result, error)
}

type Windows interface {
	Windows(ctx context.Context) ([]launcher.Window, error)
	Find(ctx context.Context, id string) (launcher.Window, bool, error)
	NewWindow(ctx context.Context, name, cwd, promptFile string) (string, error)
	Rename(ctx context.Context, id, name string) error
	KillWindow(ctx context.Context, id string) error
	CapturePane(ctx context.Context, id string) (string, error)
	HoldOnExit(ctx context.Context, id string) error
	SendPrompt(ctx context.Context, id, text string) error
}

type Prompts interface {
	Write(p promptfile.Payload) (string, error)
}

type Verdicts interface {
	Path(ts string) string
	Wait(ctx context.Context, path string) (triage.Verdict, error)
	Peek(path string) (triage.Verdict, bool)
}

type Sessions interface {
	Read(id string) (session.Record, bool, error)
}

type Journal interface {
	Append(e journal.Entry) error
}

type Ledger interface {
	Add(n notifylog.Notification, reason string, at time.Time) error
	Peek() ([]deadletter.Record, error)
	Remove(id string) error
}

type Alerter interface {
	Alert(ctx context.Context, key, title, text string) bool
}

type Labels interface {
	Append(l label.Label) error
	Recent(n int) ([]label.Label, error)
}

type Deps struct {
	Alerts         Alerter
	Labels         Labels
	Agents         []string
	Fetcher        Fetcher
	Classifier     mention.Classifier
	Worktrees      Worktrees
	Windows        Windows
	Prompts        Prompts
	Verdicts       Verdicts
	Sessions       Sessions
	Journal        Journal
	Inbox          Ledger
	DeadLetters    Ledger
	Outbox         Outbox
	Links          linkq.Dir
	Waiting        waitq.Dir
	Poster         Poster
	Readback       Readback
	Notes          Notes
	Render         prompt.Renderer
	Store          *state.Store
	Now            func() time.Time
	NewID          func() string
	Log            *slog.Logger
	HookSettings   string
	SelfBin        string
	ConfigPath     string
	PollInterval   time.Duration
	RetryDelay     time.Duration
	OutboxInterval time.Duration
	LinkInterval   time.Duration
}

type Pipeline struct {
	cfg            config.Config
	alerts         Alerter
	labels         Labels
	agents         map[string]bool
	fetcher        Fetcher
	classifier     mention.Classifier
	worktrees      Worktrees
	windows        Windows
	prompts        Prompts
	verdicts       Verdicts
	sessions       Sessions
	journal        Journal
	inbox          Ledger
	deadLetters    Ledger
	outbox         Outbox
	links          linkq.Dir
	waiting        waitq.Dir
	poster         Poster
	readback       Readback
	notes          Notes
	render         prompt.Renderer
	store          *state.Store
	now            func() time.Time
	newID          func() string
	log            *slog.Logger
	hookSettings   string
	selfBin        string
	configPath     string
	pollInterval   time.Duration
	retryDelay     time.Duration
	outboxInterval time.Duration
	linkInterval   time.Duration
	queue          chan notifylog.Notification

	mu          sync.Mutex
	inflight    map[string]bool
	deferred    map[string][]notifylog.Notification
	replaying   map[string]bool
	recentPosts map[string]time.Time
	triages     sync.WaitGroup
}

func New(cfg config.Config, d Deps) *Pipeline {
	return &Pipeline{
		cfg: cfg, alerts: d.Alerts, labels: d.Labels, agents: installed(d.Agents), fetcher: d.Fetcher, classifier: d.Classifier, worktrees: d.Worktrees,
		windows: d.Windows, prompts: d.Prompts, verdicts: d.Verdicts, sessions: d.Sessions,
		journal: d.Journal, inbox: d.Inbox, deadLetters: d.DeadLetters, outbox: d.Outbox, links: d.Links, waiting: d.Waiting, poster: d.Poster, readback: d.Readback, notes: d.Notes,
		render: d.Render, store: d.Store, now: d.Now, newID: d.NewID, log: d.Log,
		hookSettings: d.HookSettings, selfBin: d.SelfBin, configPath: d.ConfigPath, pollInterval: d.PollInterval, retryDelay: d.RetryDelay, outboxInterval: d.OutboxInterval, linkInterval: d.LinkInterval,
		queue:       make(chan notifylog.Notification, cfg.QueueSize),
		inflight:    map[string]bool{},
		deferred:    map[string][]notifylog.Notification{},
		replaying:   map[string]bool{},
		recentPosts: map[string]time.Time{},
	}
}

func installed(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

func (p *Pipeline) postBin() string {
	if p.configPath == "" {
		return p.selfBin
	}
	return p.selfBin + " -config " + session.ShellQuote(p.configPath)
}

func (p *Pipeline) Wait() {
	p.triages.Wait()
}

func (p *Pipeline) Enqueue(ctx context.Context, n notifylog.Notification) bool {
	if p.inbox != nil {
		if err := p.inbox.Add(n, source(n), p.now().UTC()); err != nil {
			p.log.Error("notification not admitted, inbox not written", "id", n.ID, "error", err)
			return false
		}
	}
	select {
	case p.queue <- n:
		return true
	case <-ctx.Done():
		return false
	}
}

func (p *Pipeline) Recover(ctx context.Context) int {
	if p.inbox == nil {
		return 0
	}
	records, err := p.inbox.Peek()
	if err != nil {
		p.log.Warn("inbox unreadable, admitted notifications not recovered", "error", err)
		return 0
	}
	recovered := 0
	for _, r := range records {
		if p.store.Seen(r.Notification.ID) {
			p.forget(r.Notification)
			continue
		}
		if !p.Enqueue(ctx, r.Notification) {
			break
		}
		recovered++
	}
	return recovered
}

func (p *Pipeline) Work(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case n := <-p.queue:
			if err := p.Handle(ctx, n); err != nil && ctx.Err() == nil {
				p.log.Error("notification not handled", "id", n.ID, "error", err)
			}
		}
	}
}

func (p *Pipeline) Replay(ctx context.Context) (int, error) {
	if p.deadLetters == nil {
		return 0, errors.New("dead letters are not configured")
	}
	records, err := p.deadLetters.Peek()
	if err != nil {
		return 0, err
	}
	queued := 0
	for _, r := range records {
		n := r.Notification
		n.Source = notifylog.SourceReplay
		p.mu.Lock()
		busy := p.replaying[n.ID]
		p.replaying[n.ID] = true
		p.mu.Unlock()
		if busy {
			continue
		}
		p.log.Info("replaying dead letter", "id", n.ID, "channel", n.Channel, "ts", n.MsgTS, "reason", r.Reason)
		if !p.Enqueue(ctx, n) {
			p.mu.Lock()
			delete(p.replaying, n.ID)
			p.mu.Unlock()
			if err := ctx.Err(); err != nil {
				return queued, err
			}
			return queued, fmt.Errorf("replay stopped: %s was not admitted", n.ID)
		}
		queued++
	}
	return queued, nil
}

func (p *Pipeline) releaseReplay(n notifylog.Notification) {
	if n.Source != notifylog.SourceReplay {
		return
	}
	p.mu.Lock()
	delete(p.replaying, n.ID)
	p.mu.Unlock()
}

func (p *Pipeline) bury(n notifylog.Notification, reason string) error {
	if p.deadLetters == nil {
		return nil
	}
	if err := p.deadLetters.Add(n, reason, p.now().UTC()); err != nil {
		return fmt.Errorf("dead letter for %s: %w", n.ID, err)
	}
	if p.alerts != nil {
		p.alerts.Alert(context.WithoutCancel(context.Background()), "deadletter:"+n.ID, "handoffd",
			fmt.Sprintf("dead letter %s/%s: %s — handoffd replay", n.Channel, n.MsgTS, reason))
	}
	return nil
}

func (p *Pipeline) Handle(ctx context.Context, n notifylog.Notification) error {
	replay := n.Source == notifylog.SourceReplay
	if !replay && p.store.Seen(n.ID) {
		p.forget(n)
		return nil
	}

	var (
		action string
		err    error
	)
	for attempt := 1; attempt <= handleAttempts; attempt++ {
		action, err = p.process(ctx, n)
		if errors.Is(err, errDeferred) {
			return nil
		}
		if errors.Is(err, errNotBuried) {
			p.log.Error("notification kept in the inbox, dead letter not written", "id", n.ID, "error", err)
			return err
		}
		if err == nil {
			if action != "" {
				p.settle(n, action)
			}
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
		p.log.Warn("notification attempt failed", "id", n.ID, "attempt", attempt, "error", err)
		if attempt < handleAttempts && !sleep(ctx, p.retryDelay) {
			return ctx.Err()
		}
	}
	p.record(journal.Entry{
		ID: n.ID, Source: source(n), Channel: n.Channel, MsgTS: n.MsgTS, ThreadTS: n.ThreadTS,
		Action: journal.ActionError, Error: err.Error(),
	})
	p.releaseReplay(n)
	if buryErr := p.bury(n, err.Error()); buryErr != nil {
		p.log.Error("notification kept in the inbox, dead letter not written", "id", n.ID, "error", buryErr)
		return errors.Join(err, buryErr)
	}
	p.settle(n, journal.ActionError)
	return err
}

func (p *Pipeline) settle(n notifylog.Notification, action string) {
	committed := true
	if err := p.store.CommitSeen(n.ID); err != nil {
		committed = false
		p.log.Error("durable copies kept, seen state not committed after notification", "id", n.ID, "error", err)
	}
	if committed {
		p.forget(n)
	}
	p.releaseReplay(n)
	if n.Source != notifylog.SourceReplay {
		return
	}
	if !committed || action == journal.ActionDropped || action == journal.ActionError || p.deadLetters == nil {
		return
	}
	if err := p.deadLetters.Remove(n.ID); err != nil {
		p.log.Warn("replayed dead letter not removed", "id", n.ID, "error", err)
	}
}

func (p *Pipeline) forget(n notifylog.Notification) {
	if p.inbox == nil {
		return
	}
	if err := p.inbox.Remove(n.ID); err != nil {
		p.log.Warn("inbox record not removed", "id", n.ID, "error", err)
	}
}

func source(n notifylog.Notification) string {
	if n.Source != "" {
		return n.Source
	}
	return journal.SourceNotification
}

func (p *Pipeline) record(e journal.Entry) {
	if p.journal == nil {
		return
	}
	if e.At.IsZero() {
		e.At = p.now().UTC()
	}
	if err := p.journal.Append(e); err != nil {
		p.log.Warn("journal entry not written", "id", e.ID, "error", err)
	}
}

type outcome struct {
	entry        journal.Entry
	resume       *state.Thread
	notification notifylog.Notification
}

func (o *outcome) done(action, reason string) {
	o.entry.Action, o.entry.Reason = action, reason
}

func (p *Pipeline) finish(out *outcome) error {
	if out.entry.Action == "" {
		return nil
	}
	p.record(out.entry)
	if out.entry.Action != journal.ActionDropped {
		return nil
	}
	return p.bury(out.notification, out.entry.Reason)
}

func (p *Pipeline) process(ctx context.Context, n notifylog.Notification) (action string, err error) {
	msg, err := p.fetcher.Fetch(ctx, n.Channel, n.MsgTS, n.ThreadTS)
	if err != nil {
		return "", err
	}

	if msg.ThreadTS != "" && msg.ThreadTS != n.MsgTS {
		n.ThreadTS = msg.ThreadTS
	}
	root := n.RootTS()
	thread, hasThread := p.store.Thread(root)
	res := p.classifier.Classify(mention.Message{
		Channel:      n.Channel,
		ChannelLabel: msg.ChannelLabel,
		TS:           n.MsgTS,
		ThreadTS:     n.ThreadTS,
		AuthorID:     msg.AuthorID,
		AuthorName:   msg.AuthorName,
		Text:         msg.Text,
		IsDM:         msg.IsDM,
		IsBot:        msg.IsBot,
		Subtype:      msg.Subtype,
		SelfInThread: msg.SelfInThread,
	}, p.cfg.ReviewEnabled && hasThread && thread.Kind == mention.KindReview.String())

	out := &outcome{notification: n, entry: journal.Entry{
		ID: n.ID, Source: source(n), Channel: n.Channel, ChannelLabel: msg.ChannelLabel,
		MsgTS: n.MsgTS, ThreadTS: root, AuthorID: msg.AuthorID, AuthorName: msg.AuthorName,
		Text: journal.Snippet(msg.Text), Kind: res.Kind.String(),
		Mentioned: res.Mentioned, ReviewSignal: res.ReviewSignal,
	}}
	defer func() {
		if buryErr := p.finish(out); buryErr != nil {
			action, err = "", fmt.Errorf("%w: %w", errNotBuried, buryErr)
		}
	}()
	err = p.route(ctx, n, root, thread, hasThread, msg, res, out)
	return out.entry.Action, err
}

func (p *Pipeline) route(
	ctx context.Context,
	n notifylog.Notification,
	root string,
	thread state.Thread,
	hasThread bool,
	msg slackfetch.Message,
	res mention.Result,
	out *outcome,
) error {
	remembered := hasThread
	if hasThread && !thread.WindowGone() {
		window, ok, err := p.windows.Find(ctx, thread.WindowID)
		if err != nil {
			return err
		}
		p.log.Info("thread window lookup",
			"thread_ts", root, "window", thread.WindowID, "name", thread.WindowName,
			"found", ok, "command", window.Command, "dead", window.Dead,
			"occupancy", window.Occupancy)
		if !ok || !window.MayHoldAgent() {
			p.closeSessions(root, thread, "window_gone")
			release := func(th *state.Thread) error {
				th.WindowID, th.WindowName, th.SessionID = "", "", ""
				th.UpdatedAt = p.now().UTC()
				return nil
			}
			if err := p.store.UpdateThread(root, release); err != nil {
				return err
			}
			if err := release(&thread); err != nil {
				return err
			}
		}
	}
	hasThread = hasThread && !thread.WindowGone()

	if res.Kind == mention.KindStop {
		return p.stop(ctx, root, thread, hasThread, out)
	}
	if res.Kind == mention.KindNone {
		if res.Progress != "" && remembered {
			return p.progress(ctx, root, thread, res, msg.Text, n.MsgTS, out)
		}
		out.done(journal.ActionIgnored, res.Filtered)
		return nil
	}
	if res.Kind == mention.KindReview || res.Kind == mention.KindHelp {
		res.Refs = refsWithThreadFallback(res.Refs, msg)
	}

	if remembered && !hasThread {
		out.resume = &thread
		if len(res.Refs.MRs) == 0 && thread.MR != nil {
			res.Refs.MRs = []mrref.MR{*thread.MR}
		}
	}
	pctx := p.promptContext(n, root, msg, res)
	pctx.Resumed = out.resume != nil

	if hasThread {
		return p.continueInWindow(ctx, root, thread, pctx, out)
	}

	if res.Kind == mention.KindReview && !res.Mentioned && !res.ReviewSignal {
		if !remembered {
			p.log.Info("skipped thread chatter with no live window", "ts", n.MsgTS, "thread_ts", root)
			out.done(journal.ActionSkipped, "thread chatter, no live window")
			return nil
		}
		if p.classifier.Acknowledgement(msg.Text) {
			out.done(journal.ActionSkipped, "acknowledgement in a remembered thread")
			return nil
		}
		p.log.Info("reply in a remembered thread goes to triage", "ts", n.MsgTS, "thread_ts", root)
		res.Kind = mention.KindTriage
		out.entry.Kind = res.Kind.String()
	}

	switch res.Kind {
	case mention.KindTriage:
		return p.runTriage(ctx, n, root, res, msg, pctx, out)
	case mention.KindHelp:
		pctx.Macro = true
		return p.escalate(ctx, root, res, msg, pctx, "", "macro phrase in the message", out)
	default:
		return p.openWindow(ctx, n, root, res, pctx, out)
	}
}

func (p *Pipeline) stop(ctx context.Context, root string, thread state.Thread, live bool, out *outcome) error {
	if !live {
		p.log.Info("stop phrase without a live window, recorded as feedback", "thread_ts", root)
		out.done(journal.ActionFeedback, "stop phrase, no live window")
		return nil
	}
	out.entry.WindowID, out.entry.WindowName, out.entry.SessionID = thread.WindowID, thread.WindowName, thread.SessionID
	if err := p.windows.KillWindow(ctx, thread.WindowID); err != nil {
		return fmt.Errorf("stop phrase: window %s not closed: %w", thread.WindowID, err)
	}
	if moved, err := p.waiting.Quarantine(thread.WindowID, "stopped by the owner", p.now().UTC()); err != nil {
		p.log.Warn("waiting prompts of a stopped window not kept aside", "window", thread.WindowID, "error", err)
	} else if moved > 0 {
		p.log.Info("waiting prompts dropped by the stop phrase", "window", thread.WindowID, "count", moved)
	}
	p.labelStop(thread)
	p.closeSessions(root, thread, "stop_phrase")
	thread.WindowID, thread.WindowName, thread.SessionID = "", "", ""
	thread.UpdatedAt = p.now().UTC()
	p.store.SetThread(root, thread)
	p.log.Info("window closed on stop phrase", "thread_ts", root, "window", out.entry.WindowID)
	out.done(journal.ActionFeedback, "stop phrase, window closed")
	return p.store.Save()
}

func refsWithThreadFallback(refs mrref.Refs, msg slackfetch.Message) mrref.Refs {
	if len(refs.MRs) > 0 {
		return refs
	}
	var tail strings.Builder
	for _, reply := range msg.ThreadTail {
		tail.WriteString(reply.Text)
		tail.WriteString("\n")
	}
	fromThread := mrref.Extract(tail.String())
	if len(fromThread.MRs) == 0 {
		return refs
	}
	refs.MRs = fromThread.MRs
	if len(refs.JiraKeys) == 0 {
		refs.JiraKeys = fromThread.JiraKeys
	}
	if refs.SHA == "" {
		refs.SHA = fromThread.SHA
	}
	return refs
}

func (p *Pipeline) promptContext(
	n notifylog.Notification,
	root string,
	msg slackfetch.Message,
	res mention.Result,
) prompt.Context {
	pctx := prompt.Context{
		Workspace:     p.cfg.SlackWorkspace,
		SelfUserID:    p.cfg.SelfUserID,
		Channel:       n.Channel,
		ChannelName:   p.channelName(n.Channel, msg),
		AuthorID:      msg.AuthorID,
		AuthorName:    msg.AuthorName,
		ReviewEnabled: p.cfg.ReviewEnabled,
		ReviewMode:    res.Kind == mention.KindReview,
		MsgTS:         n.MsgTS,
		ThreadTS:      root,
		Text:          msg.Text,
		IsDM:          msg.IsDM,
		SHA:           res.Refs.SHA,
		PostBin:       p.postBin(),
		Round:         1,
	}
	if mr, ok := res.Refs.Primary(); ok {
		pctx.MergeRef = mr.HeadRef()
		pctx.Forge = mr.Forge
	}
	return pctx
}

func (p *Pipeline) channelName(channel string, msg slackfetch.Message) string {
	if channel == p.cfg.Channel {
		return "#" + p.cfg.ChannelName
	}
	if msg.ChannelLabel != "" {
		return msg.ChannelLabel
	}
	return channel
}

func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
