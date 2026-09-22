package status

import (
	"fmt"
	"strings"
	"time"
)

type Thread struct {
	Root        string        `json:"root"`
	Kind        string        `json:"kind"`
	Window      string        `json:"window,omitempty"`
	WindowName  string        `json:"window_name,omitempty"`
	Live        bool          `json:"live"`
	Session     string        `json:"session,omitempty"`
	BlockedFor  time.Duration `json:"blocked_for,omitempty"`
	Worktree    string        `json:"worktree,omitempty"`
	Head        string        `json:"head,omitempty"`
	Progress    string        `json:"progress,omitempty"`
	ProgressSHA string        `json:"progress_sha,omitempty"`
	Members     []string      `json:"members,omitempty"`
}

type Unfinished struct {
	Root       string        `json:"root"`
	Kind       string        `json:"kind"`
	WindowName string        `json:"window_name"`
	Status     string        `json:"status"`
	Age        time.Duration `json:"age"`
}

type Report struct {
	DaemonPID       int           `json:"daemon_pid"`
	DaemonAlive     bool          `json:"daemon_alive"`
	Agent           string        `json:"agent"`
	SlackLogAge     time.Duration `json:"slack_log_age"`
	SlackLogStale   bool          `json:"slack_log_stale"`
	LastDecisionAge time.Duration `json:"last_decision_age"`
	Decisions24h    string        `json:"decisions_24h,omitempty"`
	DeadLetters     int           `json:"dead_letters"`
	Threads         []Thread      `json:"threads"`
	Work            string        `json:"work,omitempty"`
	Unfinished      []Unfinished  `json:"unfinished,omitempty"`
}

func (r Report) Counts() (live, blocked int) {
	for _, th := range r.Threads {
		if th.Live {
			live++
		}
		if th.BlockedFor > 0 {
			blocked++
		}
	}
	return live, blocked
}

func (r Report) Short(blockedAfter time.Duration) string {
	if !r.DaemonAlive {
		return "watcher down"
	}
	live, _ := r.Counts()
	parts := []string{fmt.Sprintf("%d live", live)}
	waiting := 0
	for _, th := range r.Threads {
		if th.BlockedFor >= blockedAfter && blockedAfter > 0 {
			waiting++
		}
	}
	if waiting > 0 {
		parts = append(parts, fmt.Sprintf("%d waiting", waiting))
	}
	if n := len(r.Unfinished); n > 0 {
		parts = append(parts, fmt.Sprintf("%d unfinished", n))
	}
	if r.DeadLetters > 0 {
		parts = append(parts, fmt.Sprintf("%d dead", r.DeadLetters))
	}
	if r.SlackLogStale {
		parts = append(parts, "log stale")
	}
	return strings.Join(parts, ", ")
}

func Render(r Report) string {
	var b strings.Builder
	if r.DaemonAlive {
		fmt.Fprintf(&b, "watcher: running (pid %d), agent %s\n", r.DaemonPID, r.Agent)
	} else {
		b.WriteString("watcher: NOT running\n")
	}
	if r.SlackLogAge > 0 {
		fmt.Fprintf(&b, "slack log: last write %s ago", r.SlackLogAge.Round(time.Second))
		if r.SlackLogStale {
			b.WriteString("  STALE — is Slack writing its log? is the IPC bridge alive?")
		}
		b.WriteString("\n")
	}
	if r.LastDecisionAge > 0 {
		fmt.Fprintf(&b, "last decision: %s ago\n", r.LastDecisionAge.Round(time.Second))
	}
	live, _ := r.Counts()
	fmt.Fprintf(&b, "threads: %d (%d live)\n", len(r.Threads), live)
	for _, th := range r.Threads {
		fmt.Fprintf(&b, "  %s ", th.Root)
		if th.Live {
			fmt.Fprintf(&b, "%s %s %s", th.Window, th.WindowName, th.Kind)
			if th.Session != "" {
				fmt.Fprintf(&b, " session=%s", th.Session)
			}
			if th.BlockedFor > 0 {
				fmt.Fprintf(&b, " waiting %s", th.BlockedFor.Round(time.Minute))
			}
		} else {
			fmt.Fprintf(&b, "%s window closed", th.Kind)
		}
		if th.Worktree != "" {
			fmt.Fprintf(&b, " worktree=%s", th.Worktree)
		}
		if th.Head != "" {
			fmt.Fprintf(&b, " head=%s", th.Head)
		}
		if th.Progress != "" {
			fmt.Fprintf(&b, " progress=%s", th.Progress)
			if th.ProgressSHA != "" {
				fmt.Fprintf(&b, "@%s", th.ProgressSHA)
			}
		}
		if len(th.Members) > 0 {
			fmt.Fprintf(&b, " committee=%s", strings.Join(th.Members, ","))
		}
		b.WriteString("\n")
	}
	if r.Work != "" {
		fmt.Fprintf(&b, "%s\n", r.Work)
	}
	for _, u := range r.Unfinished {
		fmt.Fprintf(&b, "  %s %s %s %s for %s\n", u.Root, u.WindowName, u.Kind, u.Status, u.Age.Round(time.Minute))
	}
	fmt.Fprintf(&b, "dead letters: %d", r.DeadLetters)
	if r.DeadLetters > 0 {
		b.WriteString(" (handoffd replay)")
	}
	b.WriteString("\n")
	if r.Decisions24h != "" {
		fmt.Fprintf(&b, "decisions 24h: %s\n", strings.ReplaceAll(r.Decisions24h, "\n", "\n               "))
	}
	return b.String()
}
