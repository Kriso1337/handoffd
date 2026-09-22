package threadpoll

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/Kriso1337/handoffd/internal/notifylog"
	"github.com/Kriso1337/handoffd/internal/slackfetch"
	"github.com/Kriso1337/handoffd/internal/state"
)

type Lister interface {
	Replies(ctx context.Context, channel, threadTS, since string) ([]slackfetch.Reply, error)
}

type Sink interface {
	Enqueue(ctx context.Context, n notifylog.Notification) bool
}

type Poller struct {
	store    *state.Store
	lister   Lister
	sink     Sink
	log      *slog.Logger
	now      func() time.Time
	interval time.Duration
	window   time.Duration
	limit    int
}

func New(store *state.Store, lister Lister, sink Sink, log *slog.Logger, now func() time.Time, interval, window time.Duration, limit int) *Poller {
	return &Poller{store: store, lister: lister, sink: sink, log: log, now: now, interval: interval, window: window, limit: limit}
}

func (p *Poller) Run(ctx context.Context) {
	if p.interval <= 0 {
		return
	}
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.Poll(ctx)
		}
	}
}

type candidate struct {
	root   string
	thread state.Thread
}

func (p *Poller) Poll(ctx context.Context) int {
	candidates, team := p.candidates()
	queued := 0
	for _, c := range candidates {
		if ctx.Err() != nil {
			break
		}
		teamID := c.thread.TeamID
		if teamID == "" {
			teamID = team
		}
		if teamID == "" {
			p.log.Warn("thread poll skipped, Slack team id unknown", "thread_ts", c.root)
			continue
		}
		since := c.thread.LastSeenTS
		if since == "" {
			since = slackTS(p.now())
		}
		replies, err := p.lister.Replies(ctx, c.thread.Channel, c.root, since)
		if err != nil {
			p.log.Warn("thread poll failed", "thread_ts", c.root, "error", err)
			continue
		}
		sort.Slice(replies, func(i, j int) bool { return after(replies[j].TS, replies[i].TS) })
		latest := since
		for _, r := range replies {
			if r.TS == "" || !after(r.TS, since) {
				continue
			}
			n := notifylog.Notification{
				ID: teamID + "_" + r.TS, TeamID: teamID, Channel: c.thread.Channel, MsgTS: r.TS, ThreadTS: c.root,
				Source: notifylog.SourceThreadPoll,
			}
			if !p.store.Seen(n.ID) {
				if !p.sink.Enqueue(ctx, n) {
					p.log.Warn("thread poll stopped, reply not admitted", "thread_ts", c.root, "ts", r.TS)
					break
				}
				queued++
			}
			latest = r.TS
		}
		if latest != c.thread.LastSeenTS {
			err := p.store.UpdateThread(c.root, func(th *state.Thread) error {
				th.LastSeenTS = latest
				return nil
			})
			if err != nil {
				p.log.Warn("thread poll cursor not saved", "thread_ts", c.root, "error", err)
			}
		}
	}
	return queued
}

func (p *Poller) candidates() ([]candidate, string) {
	cutoff := p.now().Add(-p.window)
	var out []candidate
	team := ""
	for root, th := range p.store.Threads() {
		if th.TeamID != "" {
			team = th.TeamID
		}
		if th.Channel == "" || th.UpdatedAt.Before(cutoff) {
			continue
		}
		out = append(out, candidate{root: root, thread: th})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].thread.UpdatedAt.After(out[j].thread.UpdatedAt) })
	if p.limit > 0 && len(out) > p.limit {
		out = out[:p.limit]
	}
	return out, team
}

func after(ts, than string) bool {
	a, errA := strconv.ParseFloat(ts, 64)
	b, errB := strconv.ParseFloat(than, 64)
	if errA != nil || errB != nil {
		return ts > than
	}
	return a > b
}

func slackTS(t time.Time) string {
	return strconv.FormatInt(t.Unix(), 10) + ".000000"
}
