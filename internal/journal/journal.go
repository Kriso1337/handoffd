package journal

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	SourceNotification = "notification"
	SourceLastRead     = "last_read"
	SourceReplay       = "replay"
	SourceHook         = "hook"
	SourceThreadPoll   = "thread_poll"
	SourceWatcher      = "watcher"
	SourceOutbox       = "outbox"

	ActionOpened    = "opened"
	ActionContinued = "continued"
	ActionQueued    = "queued"
	ActionSilent    = "silent"
	ActionEscalated = "escalated"
	ActionSkipped   = "skipped"
	ActionIgnored   = "ignored"
	ActionDropped   = "dropped"
	ActionError     = "error"
	ActionFeedback  = "feedback"
	ActionProgress  = "progress"
	ActionStale     = "stale_progress"
	ActionPending   = "pending"
	ActionSession   = "session"
	ActionPosted    = "posted"
	ActionRejected  = "rejected"

	OutcomeVerdict    = "verdict"
	OutcomePane       = "pane"
	OutcomeTimeout    = "timeout"
	OutcomeWindowDied = "window_died"

	textLimit = 200
)

type Triage struct {
	React     bool   `json:"react"`
	Reason    string `json:"reason,omitempty"`
	Task      string `json:"task,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
	Outcome   string `json:"outcome"`
}

type SessionStats struct {
	DurationMS    int64 `json:"duration_ms"`
	FirstActionMS int64 `json:"first_action_ms"`
	BlockedMS     int64 `json:"blocked_ms"`
	BlockedCount  int   `json:"blocked_count"`
	Turns         int   `json:"turns"`
}

type Entry struct {
	At           time.Time     `json:"at"`
	ID           string        `json:"id"`
	Source       string        `json:"source"`
	Channel      string        `json:"channel"`
	ChannelLabel string        `json:"channel_label,omitempty"`
	MsgTS        string        `json:"ts"`
	ThreadTS     string        `json:"thread_ts,omitempty"`
	AuthorID     string        `json:"author_id,omitempty"`
	AuthorName   string        `json:"author_name,omitempty"`
	Text         string        `json:"text,omitempty"`
	Kind         string        `json:"kind"`
	Mentioned    bool          `json:"mentioned"`
	ReviewSignal bool          `json:"review_signal"`
	Action       string        `json:"action"`
	Reason       string        `json:"reason,omitempty"`
	WindowID     string        `json:"window,omitempty"`
	WindowName   string        `json:"window_name,omitempty"`
	SessionID    string        `json:"session,omitempty"`
	Worktree     string        `json:"worktree,omitempty"`
	HeadSHA      string        `json:"head_sha,omitempty"`
	Triage       *Triage       `json:"triage,omitempty"`
	Phase        string        `json:"phase,omitempty"`
	Agent        string        `json:"agent,omitempty"`
	Round        int           `json:"round,omitempty"`
	Session      *SessionStats `json:"session_stats,omitempty"`
	Error        string        `json:"error,omitempty"`
}

func Reacted(action string) bool {
	switch action {
	case ActionOpened, ActionContinued, ActionQueued, ActionEscalated:
		return true
	}
	return false
}

func Labelable(action string) bool {
	switch action {
	case ActionOpened, ActionContinued, ActionQueued, ActionEscalated, ActionSilent, ActionSkipped, ActionIgnored, ActionDropped:
		return true
	}
	return false
}

func Snippet(text string) string {
	collapsed := strings.Join(strings.Fields(text), " ")
	runes := []rune(collapsed)
	if len(runes) <= textLimit {
		return collapsed
	}
	return string(runes[:textLimit]) + "…"
}

type Writer struct {
	path string
	mu   sync.Mutex
}

func NewWriter(path string) *Writer {
	return &Writer{path: path}
}

func (w *Writer) Append(e Entry) error {
	raw, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encode journal entry: %w", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(w.path), 0o700); err != nil {
		return fmt.Errorf("create journal dir: %w", err)
	}
	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open journal: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(raw, '\n')); err != nil {
		return fmt.Errorf("write journal: %w", err)
	}
	return nil
}

func Read(path string, since time.Time) ([]Entry, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}
	defer f.Close()

	var out []Entry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var e Entry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue
		}
		if e.At.Before(since) {
			continue
		}
		out = append(out, e)
	}
	if err := scanner.Err(); err != nil {
		return out, fmt.Errorf("read journal: %w", err)
	}
	return out, nil
}

func Since(entries []Entry, since time.Time) []Entry {
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if !e.At.Before(since) {
			out = append(out, e)
		}
	}
	return out
}

type Summary struct {
	Total                 int
	Actions               map[string]int
	Kinds                 map[string]int
	Sources               map[string]int
	TriageRuns            int
	TriageReact           int
	TriageSilent          int
	TriageNoVerdict       int
	TriageLatencyMedianMS int64
	TriageLatencyP90MS    int64
}

func Summarize(entries []Entry) Summary {
	s := Summary{Actions: map[string]int{}, Kinds: map[string]int{}, Sources: map[string]int{}}
	var latencies []int64
	for _, e := range entries {
		s.Total++
		s.Actions[e.Action]++
		s.Kinds[e.Kind]++
		s.Sources[e.Source]++
		if e.Triage == nil {
			continue
		}
		s.TriageRuns++
		latencies = append(latencies, e.Triage.LatencyMS)
		switch {
		case e.Triage.Outcome == OutcomeTimeout || e.Triage.Outcome == OutcomeWindowDied:
			s.TriageNoVerdict++
		case e.Triage.React:
			s.TriageReact++
		default:
			s.TriageSilent++
		}
	}
	if len(latencies) > 0 {
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		s.TriageLatencyMedianMS = latencies[(len(latencies)-1)/2]
		s.TriageLatencyP90MS = latencies[min(len(latencies)-1, int(float64(len(latencies))*0.9))]
	}
	return s
}

type Cost struct {
	Triages int
	Windows int
	USD     float64
}

func (s Summary) Cost(perTriage, perWindow float64) (Cost, bool) {
	if perTriage <= 0 && perWindow <= 0 {
		return Cost{}, false
	}
	c := Cost{Triages: s.TriageRuns, Windows: s.Actions[ActionOpened] + s.Actions[ActionEscalated]}
	c.USD = float64(c.Triages)*perTriage + float64(c.Windows)*perWindow
	return c, true
}

func (c Cost) String() string {
	return fmt.Sprintf("est. cost $%.2f (%d triages, %d windows)", c.USD, c.Triages, c.Windows)
}

func (s Summary) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "total %d", s.Total)
	for _, action := range []string{ActionOpened, ActionContinued, ActionQueued, ActionEscalated, ActionSilent, ActionSkipped, ActionIgnored, ActionDropped, ActionError, ActionFeedback, ActionProgress, ActionStale, ActionPosted, ActionRejected} {
		if n := s.Actions[action]; n > 0 {
			fmt.Fprintf(&b, ", %s %d", action, n)
		}
	}
	fmt.Fprintf(&b, "\ntriage runs %d, react %d, silent %d, no verdict %d, latency median %ds, p90 %ds",
		s.TriageRuns, s.TriageReact, s.TriageSilent, s.TriageNoVerdict, s.TriageLatencyMedianMS/1000, s.TriageLatencyP90MS/1000)
	if len(s.Sources) > 0 {
		keys := make([]string, 0, len(s.Sources))
		for k := range s.Sources {
			if k != "" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s %d", k, s.Sources[k]))
		}
		fmt.Fprintf(&b, "\nsources: %s", strings.Join(parts, ", "))
	}
	return b.String()
}

func NeedsAttention(entries []Entry) []Entry {
	var out []Entry
	for _, e := range entries {
		switch e.Action {
		case ActionSkipped, ActionDropped, ActionError, ActionRejected:
			out = append(out, e)
		}
	}
	return out
}
