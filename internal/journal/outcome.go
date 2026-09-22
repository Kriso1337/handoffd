package journal

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	StatusDone      = "done"
	StatusOpen      = "open"
	StatusStale     = "stale"
	StatusAbandoned = "abandoned"

	PhaseTaken       = "taken"
	PhaseReviewStart = "review_start"
	PhaseHeadChanged = "head_changed"
	PhaseReviewDone  = "review_done"
	PhaseDone        = "done"
)

type Outcome struct {
	Root       string
	Channel    string
	Kind       string
	WindowName string
	SessionID  string
	HandoffTS  string
	OpenedAt   time.Time
	TakenAt    time.Time
	StartedAt  time.Time
	DoneAt     time.Time
	DoneSHA    string
	Ended      bool
	EndedAt    time.Time
	Stats      *SessionStats
	Status     string
	Latency    time.Duration
}

func (o Outcome) Age(now time.Time) time.Duration {
	return now.Sub(o.OpenedAt)
}

func Outcomes(entries []Entry, now time.Time, stale time.Duration) []Outcome {
	var items []*Outcome
	byRoot := map[string]*Outcome{}
	bySession := map[string]*Outcome{}
	for _, e := range entries {
		switch e.Action {
		case ActionOpened, ActionEscalated:
			item := &Outcome{
				Root: e.ThreadTS, Channel: e.Channel, Kind: e.Kind, WindowName: e.WindowName, SessionID: e.SessionID,
				HandoffTS: e.MsgTS, OpenedAt: e.At,
			}
			if e.Action == ActionEscalated {
				item.Kind = "help"
			}
			items = append(items, item)
			byRoot[e.ThreadTS] = item
			if e.SessionID != "" {
				bySession[e.SessionID] = item
			}
		case ActionProgress:
			item, ok := byRoot[e.ThreadTS]
			if !ok || e.Agent != "" {
				continue
			}
			at := tsTime(e.MsgTS, e.At)
			switch e.Phase {
			case PhaseTaken:
				item.TakenAt = at
			case PhaseReviewStart:
				item.StartedAt = at
			case PhaseReviewDone, PhaseDone:
				item.DoneAt, item.DoneSHA = at, e.HeadSHA
			}
		case ActionSession:
			item, ok := bySession[e.SessionID]
			if !ok {
				continue
			}
			item.Ended, item.EndedAt, item.Stats = true, e.At, e.Session
		}
	}
	out := make([]Outcome, 0, len(items))
	for _, item := range items {
		item.Status, item.Latency = resolve(*item, now, stale)
		out = append(out, *item)
	}
	return out
}

func resolve(o Outcome, now time.Time, stale time.Duration) (string, time.Duration) {
	switch {
	case !o.DoneAt.IsZero():
		return StatusDone, o.DoneAt.Sub(tsTime(o.HandoffTS, o.OpenedAt))
	case o.Ended:
		return StatusAbandoned, 0
	case stale > 0 && now.Sub(o.OpenedAt) > stale:
		return StatusStale, 0
	default:
		return StatusOpen, 0
	}
}

func tsTime(ts string, fallback time.Time) time.Time {
	parts := strings.SplitN(ts, ".", 2)
	seconds, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || seconds <= 0 {
		return fallback
	}
	var micros int64
	if len(parts) == 2 {
		frac := parts[1]
		if len(frac) > 6 {
			frac = frac[:6]
		}
		for len(frac) < 6 {
			frac += "0"
		}
		micros, _ = strconv.ParseInt(frac, 10, 64)
	}
	return time.Unix(seconds, micros*1000).UTC()
}

type OutcomeSummary struct {
	Total         int
	Done          int
	Open          int
	Stale         int
	Abandoned     int
	LatencyMedian time.Duration
	LatencyP90    time.Duration
	BlockedMedian time.Duration
}

func SummarizeOutcomes(outs []Outcome) OutcomeSummary {
	var s OutcomeSummary
	var latencies, blocked []time.Duration
	for _, o := range outs {
		s.Total++
		switch o.Status {
		case StatusDone:
			s.Done++
			latencies = append(latencies, o.Latency)
		case StatusOpen:
			s.Open++
		case StatusStale:
			s.Stale++
		case StatusAbandoned:
			s.Abandoned++
		}
		if o.Stats != nil {
			blocked = append(blocked, time.Duration(o.Stats.BlockedMS)*time.Millisecond)
		}
	}
	s.LatencyMedian, s.LatencyP90 = percentiles(latencies)
	s.BlockedMedian, _ = percentiles(blocked)
	return s
}

func percentiles(values []time.Duration) (median, p90 time.Duration) {
	if len(values) == 0 {
		return 0, 0
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return values[(len(values)-1)/2], values[min(len(values)-1, int(float64(len(values))*0.9))]
}

func (s OutcomeSummary) String() string {
	if s.Total == 0 {
		return "work items: none"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "work items %d: done %d", s.Total, s.Done)
	if s.Done > 0 {
		fmt.Fprintf(&b, " (handoff→done median %s, p90 %s)", s.LatencyMedian.Round(time.Minute), s.LatencyP90.Round(time.Minute))
	}
	fmt.Fprintf(&b, ", open %d, stale %d, ended without DONE %d", s.Open, s.Stale, s.Abandoned)
	if s.BlockedMedian > 0 {
		fmt.Fprintf(&b, ", blocked median %s", s.BlockedMedian.Round(time.Second))
	}
	return b.String()
}

func Unfinished(outs []Outcome) []Outcome {
	var out []Outcome
	for _, o := range outs {
		if o.Status == StatusStale || o.Status == StatusAbandoned {
			out = append(out, o)
		}
	}
	return out
}
