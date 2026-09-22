package label

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	Good = "good"
	Bad  = "bad"

	SourceManual     = "manual"
	SourceStopPhrase = "stop_phrase"
)

type Label struct {
	At      time.Time `json:"at"`
	ID      string    `json:"id"`
	Verdict string    `json:"verdict"`
	React   bool      `json:"react"`
	Kind    string    `json:"kind,omitempty"`
	Action  string    `json:"action,omitempty"`
	Text    string    `json:"text,omitempty"`
	Note    string    `json:"note,omitempty"`
	Source  string    `json:"source"`
}

type Store struct {
	path string
	mu   sync.Mutex
}

func New(path string) *Store {
	return &Store{path: path}
}

func (s *Store) Append(l Label) error {
	if l.Verdict != Good && l.Verdict != Bad {
		return fmt.Errorf("label verdict %q: want %s or %s", l.Verdict, Good, Bad)
	}
	raw, err := json.Marshal(l)
	if err != nil {
		return fmt.Errorf("encode label: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create label dir: %w", err)
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open labels: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(raw, '\n')); err != nil {
		return fmt.Errorf("write label: %w", err)
	}
	return nil
}

func (s *Store) Recent(n int) ([]Label, error) {
	if n <= 0 {
		return nil, nil
	}
	labels, err := Read(s.path)
	if err != nil {
		return nil, err
	}
	latest := Latest(labels)
	var withText []Label
	for _, l := range latest {
		if l.Text != "" {
			withText = append(withText, l)
		}
	}
	if len(withText) > n {
		withText = withText[len(withText)-n:]
	}
	return withText, nil
}

func Read(path string) ([]Label, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open labels: %w", err)
	}
	defer f.Close()
	var out []Label
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var l Label
		if err := json.Unmarshal(scanner.Bytes(), &l); err != nil || l.ID == "" {
			continue
		}
		out = append(out, l)
	}
	if err := scanner.Err(); err != nil {
		return out, fmt.Errorf("read labels: %w", err)
	}
	return out, nil
}

func Latest(labels []Label) []Label {
	index := map[string]int{}
	var out []Label
	for _, l := range labels {
		if i, ok := index[l.ID]; ok {
			out[i] = l
			continue
		}
		index[l.ID] = len(out)
		out = append(out, l)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

var permalinkTS = regexp.MustCompile(`/p(\d{10})(\d{6})(?:\?|$)`)

func TimestampFromRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if m := permalinkTS.FindStringSubmatch(ref); m != nil {
		return m[1] + "." + m[2]
	}
	return ref
}
