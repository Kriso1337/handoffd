package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/Kriso1337/handoffd/internal/mrref"
)

type Member struct {
	Agent       string `json:"agent"`
	WindowID    string `json:"window_id"`
	WindowName  string `json:"window_name"`
	SessionID   string `json:"session_id,omitempty"`
	Worktree    string `json:"worktree,omitempty"`
	Progress    string `json:"progress,omitempty"`
	ProgressSHA string `json:"progress_sha,omitempty"`
}

type Result struct {
	SHA      string    `json:"sha"`
	Blockers int       `json:"blockers"`
	Others   int       `json:"others"`
	Decision string    `json:"decision,omitempty"`
	Findings int       `json:"findings"`
	At       time.Time `json:"at"`
}

type Joint struct {
	SHA       string    `json:"sha"`
	At        time.Time `json:"at"`
	Permalink string    `json:"permalink,omitempty"`
}

type Note struct {
	Agent string    `json:"agent"`
	SHA   string    `json:"sha"`
	Text  string    `json:"text"`
	At    time.Time `json:"at"`
}

type Run struct {
	Round         int               `json:"round"`
	HeadSHA       string            `json:"head_sha,omitempty"`
	BaseSHA       string            `json:"base_sha,omitempty"`
	StartedAt     time.Time         `json:"started_at"`
	OpenedBy      string            `json:"opened_by"`
	ReviewStarted bool              `json:"review_started,omitempty"`
	Notes         []Note            `json:"notes,omitempty"`
	Results       map[string]Result `json:"results,omitempty"`
	Joint         *Joint            `json:"joint,omitempty"`
}

func (r *Run) normalize() {
	if r != nil && r.Results == nil {
		r.Results = map[string]Result{}
	}
}

func (r *Run) Record(agent string, res Result) {
	if r.Results == nil {
		r.Results = map[string]Result{}
	}
	r.Results[agent] = res
}

func (r Run) NotesBy(agent string) []Note {
	var out []Note
	for _, n := range r.Notes {
		if n.Agent == agent {
			out = append(out, n)
		}
	}
	return out
}

const (
	RunOpenedByHandoff     = "handoff"
	RunOpenedByHeadChanged = "head_changed"
	RunOpenedByBaseChanged = "base_changed"
	DriverKey              = "driver"
)

type Thread struct {
	WindowID    string    `json:"window_id"`
	WindowName  string    `json:"window_name"`
	Agent       string    `json:"agent,omitempty"`
	Worktree    string    `json:"worktree"`
	MergeRef    string    `json:"merge_ref"`
	MR          *mrref.MR `json:"mr,omitempty"`
	HeadSHA     string    `json:"head_sha,omitempty"`
	BaseSHA     string    `json:"base_sha,omitempty"`
	SessionID   string    `json:"session_id,omitempty"`
	Kind        string    `json:"kind"`
	TeamID      string    `json:"team_id,omitempty"`
	Channel     string    `json:"channel,omitempty"`
	Subject     string    `json:"subject,omitempty"`
	OpenedTS    string    `json:"opened_ts,omitempty"`
	LastSeenTS  string    `json:"last_seen_ts,omitempty"`
	Progress    string    `json:"progress,omitempty"`
	ProgressSHA string    `json:"progress_sha,omitempty"`
	ProgressAt  time.Time `json:"progress_at,omitempty"`
	Members     []Member  `json:"members,omitempty"`
	Run         *Run      `json:"run,omitempty"`
	RunsHistory []Run     `json:"runs_history,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (t *Thread) OpenRound(head, base, openedBy string, now time.Time) Run {
	round := 1
	if t.Run != nil {
		t.RunsHistory = append(t.RunsHistory, *t.Run)
		round = t.Run.Round + 1
	}
	t.Run = &Run{Round: round, HeadSHA: head, BaseSHA: base, StartedAt: now, OpenedBy: openedBy, Results: map[string]Result{}}
	return *t.Run
}

func (t Thread) DriverKey() string {
	if t.Agent != "" {
		return t.Agent
	}
	return DriverKey
}

func (t Thread) Participant(sessionID string) (agent string, driver, ok bool) {
	if sessionID == "" {
		return "", false, false
	}
	if sessionID == t.SessionID {
		return t.DriverKey(), true, true
	}
	for _, m := range t.Members {
		if m.SessionID == sessionID {
			return m.Agent, false, true
		}
	}
	return "", false, false
}

type data struct {
	Seen           []string               `json:"seen"`
	Threads        map[string]Thread      `json:"threads"`
	Launches       []time.Time            `json:"launches"`
	TriageLaunches []time.Time            `json:"triage_launches"`
	SilentVerdicts map[string][]time.Time `json:"silent_verdicts,omitempty"`
}

type Store struct {
	mu        sync.Mutex
	path      string
	recovered string
	capacity  int
	data      data
	seenIndex map[string]bool
}

func Open(path string, capacity int) (*Store, error) {
	s := &Store{
		path:      path,
		capacity:  capacity,
		data:      data{Threads: map[string]Thread{}, SilentVerdicts: map[string][]time.Time{}},
		seenIndex: map[string]bool{},
	}

	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, &s.data); err != nil {
		moved, qerr := quarantine(path)
		if qerr != nil {
			return nil, fmt.Errorf("parse state %s: %w (%v)", path, err, qerr)
		}
		s.data = data{Threads: map[string]Thread{}, SilentVerdicts: map[string][]time.Time{}}
		s.recovered = moved
		return s, nil
	}
	if s.data.Threads == nil {
		s.data.Threads = map[string]Thread{}
	}
	if s.data.SilentVerdicts == nil {
		s.data.SilentVerdicts = map[string][]time.Time{}
	}
	for root, thread := range s.data.Threads {
		thread.Run.normalize()
		for i := range thread.RunsHistory {
			thread.RunsHistory[i].normalize()
		}
		s.data.Threads[root] = thread
	}
	for _, id := range s.data.Seen {
		s.seenIndex[id] = true
	}
	return s, nil
}

func (s *Store) Recovered() (string, bool) {
	return s.recovered, s.recovered != ""
}

func quarantine(path string) (string, error) {
	moved := path + ".corrupt-" + time.Now().UTC().Format("20060102T150405Z")
	if err := os.Rename(path, moved); err != nil {
		return "", fmt.Errorf("quarantine %s: %w", path, err)
	}
	return moved, nil
}

func (s *Store) Seen(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seenIndex[id]
}

func (s *Store) CommitSeen(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := s.data.Seen
	if !s.seenIndex[id] {
		s.data.Seen = append(append([]string(nil), before...), id)
		for len(s.data.Seen) > s.capacity {
			s.data.Seen = s.data.Seen[1:]
		}
	}
	if err := s.save(); err != nil {
		s.data.Seen = before
		return err
	}
	s.reindexSeen()
	return nil
}

func (s *Store) reindexSeen() {
	s.seenIndex = make(map[string]bool, len(s.data.Seen))
	for _, id := range s.data.Seen {
		s.seenIndex[id] = true
	}
}

func (t Thread) Clone() Thread {
	if t.MR != nil {
		mr := *t.MR
		t.MR = &mr
	}
	t.Members = slices.Clone(t.Members)
	t.RunsHistory = slices.Clone(t.RunsHistory)
	if t.Run != nil {
		run := *t.Run
		run.Notes = slices.Clone(t.Run.Notes)
		run.Results = maps.Clone(t.Run.Results)
		if t.Run.Joint != nil {
			joint := *t.Run.Joint
			run.Joint = &joint
		}
		t.Run = &run
	}
	return t
}

func (s *Store) Thread(rootTS string) (Thread, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.data.Threads[rootTS]
	return t.Clone(), ok
}

func (s *Store) SetThread(rootTS string, t Thread) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Threads[rootTS] = t.Clone()
}

func (s *Store) UpdateThread(rootTS string, apply func(*Thread) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.data.Threads[rootTS]
	if !ok {
		return fmt.Errorf("thread %s is no longer known", rootTS)
	}
	next := current.Clone()
	if err := apply(&next); err != nil {
		return err
	}
	s.data.Threads[rootTS] = next
	if err := s.save(); err != nil {
		s.data.Threads[rootTS] = current
		return err
	}
	return nil
}

func (s *Store) Threads() map[string]Thread {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Thread, len(s.data.Threads))
	for k, v := range s.data.Threads {
		out[k] = v.Clone()
	}
	return out
}

func (s *Store) DropThread(rootTS string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Threads, rootTS)
}

func (t Thread) WindowGone() bool {
	return t.WindowID == ""
}

func (s *Store) Sweep(now time.Time, ttl, goneTTL time.Duration, alive func(Thread) bool) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := now.Add(-ttl)
	goneCutoff := now.Add(-goneTTL)
	var dropped []string
	for root, th := range s.data.Threads {
		switch {
		case th.WindowGone() || !alive(th):
			if th.UpdatedAt.Before(goneCutoff) {
				dropped = append(dropped, root)
			}
		case th.UpdatedAt.Before(cutoff):
			dropped = append(dropped, root)
		}
	}
	sort.Strings(dropped)
	for _, root := range dropped {
		delete(s.data.Threads, root)
		delete(s.data.SilentVerdicts, root)
	}
	return dropped
}

func (s *Store) AllowLaunch(now time.Time, limit int, window time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.recent(now, window)) < limit
}

func (s *Store) RecordLaunch(now time.Time, window time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Launches = append(s.recent(now, window), now)
}

func (s *Store) AllowTriage(now time.Time, limit int, window time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(prune(s.data.TriageLaunches, now, window)) < limit
}

func (s *Store) RecordTriage(now time.Time, window time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.TriageLaunches = append(prune(s.data.TriageLaunches, now, window), now)
}

func (s *Store) RecordSilent(root string, now time.Time, window time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for other, times := range s.data.SilentVerdicts {
		if kept := prune(times, now, window); len(kept) == 0 {
			delete(s.data.SilentVerdicts, other)
		} else {
			s.data.SilentVerdicts[other] = kept
		}
	}
	s.data.SilentVerdicts[root] = append(s.data.SilentVerdicts[root], now)
	return len(s.data.SilentVerdicts[root])
}

func (s *Store) ClearSilent(root string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.SilentVerdicts, root)
}

func (s *Store) SilentCount(root string, now time.Time, window time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(prune(s.data.SilentVerdicts[root], now, window))
}

func (s *Store) recent(now time.Time, window time.Duration) []time.Time {
	return prune(s.data.Launches, now, window)
}

func prune(launches []time.Time, now time.Time, window time.Duration) []time.Time {
	cutoff := now.Add(-window)
	kept := make([]time.Time, 0, len(launches))
	for _, at := range launches {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	return kept
}

func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.save()
}

func (s *Store) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	return nil
}
