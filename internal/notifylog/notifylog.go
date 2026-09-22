package notifylog

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const blockStart = "Store: NEW_NOTIFICATION {"

type Notification struct {
	ID       string `json:"id"`
	TeamID   string `json:"teamId"`
	Channel  string `json:"channel"`
	MsgTS    string `json:"msg"`
	ThreadTS string `json:"thread_ts"`
	Silent   bool   `json:"silent"`
	Source   string `json:"source,omitempty"`
}

func (n Notification) Complete() bool {
	return n.ID != "" && n.Channel != "" && n.MsgTS != ""
}

func (n Notification) RootTS() string {
	if n.ThreadTS != "" {
		return n.ThreadTS
	}
	return n.MsgTS
}

type Scanner struct {
	collecting bool
	lines      []string
}

func NewScanner() *Scanner {
	return &Scanner{}
}

func (s *Scanner) Feed(raw string) (Notification, bool) {
	line := strings.TrimRight(raw, " \t\r\n")

	if !s.collecting {
		s.startIfHeader(line)
		return Notification{}, false
	}

	if isLogEntry(line) {
		s.reset()
		s.startIfHeader(line)
		return Notification{}, false
	}

	if line == "}" {
		block := strings.Join(append(s.lines, "}"), "\n")
		s.reset()
		return decode(block)
	}

	s.lines = append(s.lines, line)
	return Notification{}, false
}

func (s *Scanner) startIfHeader(line string) {
	if strings.Contains(line, blockStart) {
		s.collecting = true
		s.lines = []string{"{"}
	}
}

func (s *Scanner) reset() {
	s.collecting = false
	s.lines = nil
}

func decode(block string) (Notification, bool) {
	var n Notification
	if err := json.Unmarshal([]byte(block), &n); err != nil {
		return Notification{}, false
	}
	if !n.Complete() {
		return Notification{}, false
	}
	return n, true
}

func isLogEntry(line string) bool {
	return strings.HasPrefix(line, "[") && strings.Contains(line, "] ")
}

const (
	SourceLastRead   = "last_read"
	SourceReplay     = "replay"
	SourceThreadPoll = "thread_poll"
)

var lastReadPattern = regexp.MustCompile(`\[SET-LAST-READ\] \((T[A-Z0-9]+)\) markLastRead ([CDG][A-Z0-9]+):(\d+)\.(\d+)`)

type LastReadScanner struct {
	maxAge time.Duration
	now    func() time.Time
}

func NewLastReadScanner(maxAge time.Duration, now func() time.Time) *LastReadScanner {
	return &LastReadScanner{maxAge: maxAge, now: now}
}

func (s *LastReadScanner) Feed(raw string) (Notification, bool) {
	if s.maxAge <= 0 {
		return Notification{}, false
	}
	m := lastReadPattern.FindStringSubmatch(raw)
	if m == nil {
		return Notification{}, false
	}
	seconds, err := strconv.ParseInt(m[3], 10, 64)
	if err != nil {
		return Notification{}, false
	}
	age := s.now().Sub(time.Unix(seconds, 0))
	if age < 0 || age > s.maxAge {
		return Notification{}, false
	}
	ts := m[3] + "." + m[4]
	return Notification{
		ID:      m[1] + "_" + ts,
		TeamID:  m[1],
		Channel: m[2],
		MsgTS:   ts,
		Source:  SourceLastRead,
	}, true
}
