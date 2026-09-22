package alert

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const (
	TitlePlaceholder = "{title}"
	TextPlaceholder  = "{text}"
)

type Commander interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type Notifier struct {
	cmd      Commander
	argv     []string
	cooldown time.Duration
	now      func() time.Time
	log      *slog.Logger
	mu       sync.Mutex
	last     map[string]time.Time
}

func New(cmd Commander, argv []string, cooldown time.Duration, now func() time.Time, log *slog.Logger) *Notifier {
	return &Notifier{cmd: cmd, argv: argv, cooldown: cooldown, now: now, log: log, last: map[string]time.Time{}}
}

func DefaultCommand(goos string) []string {
	switch goos {
	case "darwin":
		return []string{"/usr/bin/osascript",
			"-e", "on run argv",
			"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
			"-e", "end run",
			TitlePlaceholder, TextPlaceholder}
	case "linux":
		return []string{"notify-send", TitlePlaceholder, TextPlaceholder}
	default:
		return nil
	}
}

func (n *Notifier) Enabled() bool {
	return n != nil && len(n.argv) > 0
}

func (n *Notifier) Alert(ctx context.Context, key, title, text string) bool {
	if !n.Enabled() {
		return false
	}
	now := n.now()
	n.mu.Lock()
	if at, ok := n.last[key]; ok && n.cooldown > 0 && now.Sub(at) < n.cooldown {
		n.mu.Unlock()
		return false
	}
	n.last[key] = now
	for other, at := range n.last {
		if n.cooldown > 0 && now.Sub(at) >= n.cooldown {
			delete(n.last, other)
		}
	}
	n.mu.Unlock()

	args := make([]string, 0, len(n.argv)-1)
	for _, a := range n.argv[1:] {
		a = strings.ReplaceAll(a, TitlePlaceholder, title)
		a = strings.ReplaceAll(a, TextPlaceholder, text)
		args = append(args, a)
	}
	if out, err := n.cmd.Run(ctx, n.argv[0], args...); err != nil {
		n.log.Warn("alert command failed", "key", key, "error", err, "output", strings.TrimSpace(string(out)))
		return false
	}
	n.log.Info("alert sent", "key", key, "text", text)
	return true
}
