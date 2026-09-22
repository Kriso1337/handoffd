package watchdog

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/Kriso1337/handoffd/internal/launcher"
	"github.com/Kriso1337/handoffd/internal/session"
	"github.com/Kriso1337/handoffd/internal/state"
)

const title = "handoffd"

type Windows interface {
	Windows(ctx context.Context) ([]launcher.Window, error)
}

type Sessions interface {
	Read(id string) (session.Record, bool, error)
}

type Alerter interface {
	Alert(ctx context.Context, key, title, text string) bool
}

type Watchdog struct {
	store          *state.Store
	windows        Windows
	sessions       Sessions
	alerts         Alerter
	logPath        string
	staleAfter     time.Duration
	blockedAfter   time.Duration
	collectedAfter time.Duration
	interval       time.Duration
	now            func() time.Time
	log            *slog.Logger
}

type Options struct {
	LogPath        string
	StaleAfter     time.Duration
	BlockedAfter   time.Duration
	CollectedAfter time.Duration
	Interval       time.Duration
}

func New(store *state.Store, windows Windows, sessions Sessions, alerts Alerter, opts Options, now func() time.Time, log *slog.Logger) *Watchdog {
	return &Watchdog{
		store: store, windows: windows, sessions: sessions, alerts: alerts,
		logPath: opts.LogPath, staleAfter: opts.StaleAfter, blockedAfter: opts.BlockedAfter, collectedAfter: opts.CollectedAfter, interval: opts.Interval,
		now: now, log: log,
	}
}

func (w *Watchdog) Run(ctx context.Context) {
	if w.interval <= 0 {
		return
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.Check(ctx)
		}
	}
}

func (w *Watchdog) Check(ctx context.Context) []string {
	var fired []string
	if key, text, ok := w.staleLog(); ok && w.alerts.Alert(ctx, key, title, text) {
		fired = append(fired, key)
	}
	for _, b := range w.blockedSessions(ctx) {
		key := "blocked:" + b.sessionID + ":" + strconv.FormatInt(b.since.Unix(), 10)
		text := fmt.Sprintf("%s waits for a reply %s", b.window, b.for_.Round(time.Minute))
		if b.detail != "" {
			text += " (" + b.detail + ")"
		}
		if w.alerts.Alert(ctx, key, title, text) {
			fired = append(fired, key)
		}
	}
	for _, c := range w.collectedRounds() {
		key := fmt.Sprintf("collected:%s:%d", c.root, c.round)
		text := fmt.Sprintf("%s: round %d collected %s ago, no joint verdict", c.window, c.round, c.for_.Round(time.Minute))
		if w.alerts.Alert(ctx, key, title, text) {
			fired = append(fired, key)
		}
	}
	return fired
}

type collected struct {
	root   string
	window string
	round  int
	for_   time.Duration
}

func (w *Watchdog) collectedRounds() []collected {
	if w.collectedAfter <= 0 {
		return nil
	}
	var out []collected
	for root, th := range w.store.Threads() {
		if th.Run == nil || th.Run.Joint != nil || len(th.Members) == 0 || th.WindowGone() {
			continue
		}
		last := time.Time{}
		complete := true
		for _, m := range th.Members {
			res, ok := th.Run.Results[m.Agent]
			if !ok {
				complete = false
				break
			}
			if res.At.After(last) {
				last = res.At
			}
		}
		if !complete {
			continue
		}
		if waiting := w.now().Sub(last); waiting >= w.collectedAfter {
			out = append(out, collected{root: root, window: th.WindowName, round: th.Run.Round, for_: waiting})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].root < out[j].root })
	return out
}

func (w *Watchdog) staleLog() (string, string, bool) {
	if w.logPath == "" || w.staleAfter <= 0 {
		return "", "", false
	}
	info, err := os.Stat(w.logPath)
	if err != nil {
		return "stale-log:missing", "Slack log is missing: " + w.logPath, true
	}
	age := w.now().Sub(info.ModTime())
	if age < w.staleAfter {
		return "", "", false
	}
	return "stale-log:" + strconv.FormatInt(info.ModTime().Unix(), 10),
		fmt.Sprintf("Slack log silent for %s, notifications are not arriving", age.Round(time.Minute)), true
}

type blocked struct {
	sessionID string
	window    string
	detail    string
	since     time.Time
	for_      time.Duration
}

func (w *Watchdog) blockedSessions(ctx context.Context) []blocked {
	if w.blockedAfter <= 0 {
		return nil
	}
	windows, err := w.windows.Windows(ctx)
	if err != nil {
		w.log.Warn("watchdog skipped blocked check, window list unavailable", "error", err)
		return nil
	}
	live := map[string]bool{}
	for _, win := range windows {
		if win.MayHoldAgent() {
			live[win.ID] = true
		}
	}
	var out []blocked
	for _, th := range w.store.Threads() {
		if th.SessionID == "" || !live[th.WindowID] {
			continue
		}
		rec, ok, err := w.sessions.Read(th.SessionID)
		if err != nil || !ok {
			continue
		}
		if waiting := rec.BlockedFor(w.now()); waiting >= w.blockedAfter {
			out = append(out, blocked{sessionID: th.SessionID, window: th.WindowName, detail: rec.Detail, since: rec.UpdatedAt, for_: waiting})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].window < out[j].window })
	return out
}
