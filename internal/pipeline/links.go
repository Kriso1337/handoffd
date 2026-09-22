package pipeline

import (
	"context"
	"time"

	"github.com/Kriso1337/handoffd/internal/linkq"
	"github.com/Kriso1337/handoffd/internal/state"
)

func (p *Pipeline) RunLinks(ctx context.Context) {
	if p.links.Path() == "" || p.linkInterval <= 0 {
		return
	}
	ticker := time.NewTicker(p.linkInterval)
	defer ticker.Stop()
	for {
		p.DrainLinks(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *Pipeline) DrainLinks(ctx context.Context) int {
	if p.links.Path() == "" {
		return 0
	}
	requests, malformed, err := p.links.Pending()
	if err != nil {
		p.log.Warn("link requests unreadable", "error", err)
		return 0
	}
	for _, m := range malformed {
		if err := p.links.Quarantine(m.Path, "malformed: "+m.Err.Error(), p.now().UTC()); err != nil {
			p.log.Warn("link request not quarantined", "path", m.Path, "error", err)
		}
	}
	linked := 0
	for _, req := range requests {
		rc, err := linkq.Apply(ctx, req, linkq.Deps{
			Windows:       p.windows,
			Revisions:     p.worktrees,
			ReviewEnabled: p.cfg.ReviewEnabled,
			Allows:        p.cfg.Allows,
			Store:         p.store,
			Now:           p.now,
			NewID:         p.newID,
			Released: func(root string, thread state.Thread) {
				p.closeSessions(root, thread, "relinked")
			},
		})
		if err != nil {
			p.log.Warn("link request not applied", "id", req.ID, "thread_ts", req.ThreadTS, "error", err)
			continue
		}
		if err := p.links.WriteReceipt(rc); err != nil {
			p.log.Warn("link receipt not written", "id", req.ID, "error", err)
		}
		if rc.Status != linkq.StatusLinked {
			p.log.Info("link request rejected", "id", req.ID, "thread_ts", req.ThreadTS, "reason", rc.Reason)
			continue
		}
		p.log.Info("window linked",
			"window", rc.WindowID, "name", rc.WindowName, "thread_ts", req.ThreadTS, "session", rc.SessionID)
		linked++
	}
	return linked
}
