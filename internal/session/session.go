package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kriso1337/handoffd/internal/journal"
)

type State string

const (
	Unknown State = ""
	Working State = "working"
	Blocked State = "blocked"
	Idle    State = "idle"
	Ended   State = "ended"
)

type Record struct {
	SessionID     string    `json:"session_id"`
	State         State     `json:"state"`
	Event         string    `json:"event"`
	Detail        string    `json:"detail,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
	StartedAt     time.Time `json:"started_at,omitzero"`
	FirstActionAt time.Time `json:"first_action_at,omitzero"`
	BlockedMS     int64     `json:"blocked_ms,omitempty"`
	BlockedCount  int       `json:"blocked_count,omitempty"`
	Turns         int       `json:"turns,omitempty"`
}

func (r Record) Stats(now time.Time) journal.SessionStats {
	stats := journal.SessionStats{BlockedMS: r.BlockedMS, BlockedCount: r.BlockedCount, Turns: r.Turns}
	if !r.StartedAt.IsZero() {
		stats.DurationMS = now.Sub(r.StartedAt).Milliseconds()
		if !r.FirstActionAt.IsZero() {
			stats.FirstActionMS = r.FirstActionAt.Sub(r.StartedAt).Milliseconds()
		}
	}
	return stats
}

func (r Record) BlockedFor(now time.Time) time.Duration {
	if r.State != Blocked {
		return 0
	}
	return now.Sub(r.UpdatedAt)
}

type HookInput struct {
	SessionID        string `json:"session_id"`
	Event            string `json:"hook_event_name"`
	ToolName         string `json:"tool_name"`
	NotificationType string `json:"notification_type"`
}

func Parse(r io.Reader) (HookInput, error) {
	var in HookInput
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		return HookInput{}, fmt.Errorf("parse hook input: %w", err)
	}
	return in, nil
}

func Transition(in HookInput, now time.Time) (Record, bool) {
	rec := Record{SessionID: in.SessionID, Event: in.Event, UpdatedAt: now}
	switch in.Event {
	case "SessionStart", "UserPromptSubmit", "PostToolUse", "PostToolUseFailure", "PermissionDenied", "ElicitationResult":
		rec.State = Working
	case "PreToolUse":
		if in.ToolName != "AskUserQuestion" {
			return Record{}, false
		}
		rec.State, rec.Detail = Blocked, in.ToolName
	case "PermissionRequest":
		rec.State, rec.Detail = Blocked, in.ToolName
	case "Elicitation":
		rec.State = Blocked
	case "Notification":
		switch in.NotificationType {
		case "permission_prompt", "elicitation_dialog", "elicitation_url_dialog":
			rec.State, rec.Detail = Blocked, in.NotificationType
		case "idle_prompt":
			rec.State = Idle
		default:
			return Record{}, false
		}
	case "Stop", "Interrupt":
		rec.State = Idle
	case "SessionEnd":
		rec.State = Ended
	default:
		return Record{}, false
	}
	return rec, true
}

func Advance(prev Record, in HookInput, now time.Time) (Record, bool) {
	rec, ok := Transition(in, now)
	if !ok {
		return Record{}, false
	}
	rec.StartedAt, rec.FirstActionAt = prev.StartedAt, prev.FirstActionAt
	rec.BlockedMS, rec.BlockedCount, rec.Turns = prev.BlockedMS, prev.BlockedCount, prev.Turns
	if rec.StartedAt.IsZero() {
		rec.StartedAt = now
	}
	if rec.FirstActionAt.IsZero() && (in.Event == "PostToolUse" || in.Event == "PostToolUseFailure") {
		rec.FirstActionAt = now
	}
	if in.Event == "UserPromptSubmit" {
		rec.Turns++
	}
	switch {
	case prev.State == Blocked && rec.State != Blocked:
		rec.BlockedMS += now.Sub(prev.UpdatedAt).Milliseconds()
	case prev.State != Blocked && rec.State == Blocked:
		rec.BlockedCount++
	}
	return rec, true
}

type Store struct {
	dir string
}

func NewStore(dir string) Store {
	return Store{dir: dir}
}

func (s Store) path(id string) (string, error) {
	if id == "" || id != filepath.Base(id) || strings.ContainsAny(id, "/\\") || strings.HasPrefix(id, ".") {
		return "", fmt.Errorf("unsafe session id %q", id)
	}
	return filepath.Join(s.dir, id+".json"), nil
}

func (s Store) Read(id string) (Record, bool, error) {
	path, err := s.path(id)
	if err != nil {
		return Record{}, false, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("read session %s: %w", id, err)
	}
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Record{}, false, fmt.Errorf("parse session %s: %w", id, err)
	}
	return rec, true, nil
}

func (s Store) Write(rec Record) error {
	path, err := s.path(rec.SessionID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create session dir: %w", err)
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write session: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace session: %w", err)
	}
	return nil
}

const EnvSessionID = "HANDOFFD_SESSION_ID"

func Apply(store Store, sessionID string, input io.Reader, now time.Time) (Record, bool, error) {
	if sessionID == "" {
		return Record{}, false, nil
	}
	in, err := Parse(input)
	if err != nil {
		return Record{}, false, err
	}
	in.SessionID = sessionID
	prev, _, err := store.Read(sessionID)
	if err != nil {
		return Record{}, false, err
	}
	rec, ok := Advance(prev, in, now)
	if !ok {
		return Record{}, false, nil
	}
	if err := store.Write(rec); err != nil {
		return Record{}, false, err
	}
	return rec, true, nil
}

const (
	hookTimeoutSeconds = 5
	hookCommandSuffix  = " hook"
	hookCommandMarker  = "handoffd"
)

type Event struct {
	Name    string
	Matcher string
}

var ClaudeEvents = []Event{
	{"SessionStart", ""},
	{"UserPromptSubmit", ""},
	{"PreToolUse", "AskUserQuestion"},
	{"PermissionRequest", ""},
	{"PermissionDenied", ""},
	{"PostToolUse", ""},
	{"PostToolUseFailure", ""},
	{"Notification", ""},
	{"Elicitation", ""},
	{"ElicitationResult", ""},
	{"Stop", ""},
	{"SessionEnd", ""},
}

var CodexEvents = []Event{
	{"SessionStart", ""},
	{"UserPromptSubmit", ""},
	{"PermissionRequest", ""},
	{"PostToolUse", ""},
	{"Stop", ""},
	{"Interrupt", ""},
	{"SessionEnd", ""},
}

func HookCommand(selfBin, configPath string) string {
	return ShellQuote(selfBin) + " -config " + ShellQuote(configPath) + hookCommandSuffix
}

func Groups(selfBin, configPath string, events []Event) map[string][]any {
	handler := map[string]any{"type": "command", "command": HookCommand(selfBin, configPath), "timeout": hookTimeoutSeconds}
	hooks := map[string][]any{}
	for _, e := range events {
		group := map[string]any{"hooks": []any{handler}}
		if e.Matcher != "" {
			group["matcher"] = e.Matcher
		}
		hooks[e.Name] = []any{group}
	}
	return hooks
}

func SettingsJSON(selfBin, configPath string) (string, error) {
	raw, err := json.Marshal(map[string]any{"hooks": Groups(selfBin, configPath, ClaudeEvents)})
	if err != nil {
		return "", fmt.Errorf("encode hook settings: %w", err)
	}
	return string(raw), nil
}

func WithPermissions(settings string, allow, deny []string) (string, error) {
	doc := map[string]any{}
	if strings.TrimSpace(settings) != "" {
		if err := json.Unmarshal([]byte(settings), &doc); err != nil {
			return "", fmt.Errorf("parse settings: %w", err)
		}
	}
	permissions, _ := doc["permissions"].(map[string]any)
	if permissions == nil {
		permissions = map[string]any{}
	}
	if len(allow) > 0 {
		permissions["allow"] = allow
	}
	if len(deny) > 0 {
		permissions["deny"] = deny
	}
	if len(permissions) == 0 {
		return settings, nil
	}
	doc["permissions"] = permissions
	raw, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("encode settings: %w", err)
	}
	return string(raw), nil
}

func MergeHooksJSON(existing []byte, groups map[string][]any) ([]byte, error) {
	doc := map[string]any{}
	if len(strings.TrimSpace(string(existing))) > 0 {
		if err := json.Unmarshal(existing, &doc); err != nil {
			return nil, fmt.Errorf("parse hooks.json: %w", err)
		}
	}
	hooks, _ := doc["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	for event, list := range hooks {
		hooks[event] = withoutOurGroups(list)
	}
	for event, ours := range groups {
		kept, _ := hooks[event].([]any)
		hooks[event] = append(kept, ours...)
	}
	for event, list := range hooks {
		if items, _ := list.([]any); len(items) == 0 {
			delete(hooks, event)
		}
	}
	doc["hooks"] = hooks
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode hooks.json: %w", err)
	}
	return append(out, '\n'), nil
}

func withoutOurGroups(list any) []any {
	items, _ := list.([]any)
	kept := make([]any, 0, len(items))
	for _, item := range items {
		if !isOurGroup(item) {
			kept = append(kept, item)
		}
	}
	return kept
}

func isOurGroup(item any) bool {
	group, _ := item.(map[string]any)
	handlers, _ := group["hooks"].([]any)
	for _, h := range handlers {
		handler, _ := h.(map[string]any)
		command, _ := handler["command"].(string)
		if strings.Contains(command, hookCommandMarker) && strings.HasSuffix(command, hookCommandSuffix) {
			return true
		}
	}
	return false
}

func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
