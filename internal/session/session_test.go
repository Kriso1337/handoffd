package session

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const id = "33333333-3333-3333-3333-333333333333"

var now = time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

func hook(t *testing.T, fields map[string]any) string {
	t.Helper()
	fields["session_id"] = id
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestTransitionsFollowClaudeCodeLifecycle(t *testing.T) {
	cases := []struct {
		name  string
		input map[string]any
		want  State
	}{
		{"start", map[string]any{"hook_event_name": "SessionStart"}, Working},
		{"prompt", map[string]any{"hook_event_name": "UserPromptSubmit"}, Working},
		{"question", map[string]any{"hook_event_name": "PreToolUse", "tool_name": "AskUserQuestion"}, Blocked},
		{"permission", map[string]any{"hook_event_name": "PermissionRequest", "tool_name": "Bash"}, Blocked},
		{"permission notice", map[string]any{"hook_event_name": "Notification", "notification_type": "permission_prompt"}, Blocked},
		{"elicitation", map[string]any{"hook_event_name": "Elicitation"}, Blocked},
		{"tool done", map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash"}, Working},
		{"tool failed", map[string]any{"hook_event_name": "PostToolUseFailure", "tool_name": "Bash"}, Working},
		{"denied", map[string]any{"hook_event_name": "PermissionDenied", "tool_name": "Bash"}, Working},
		{"elicited", map[string]any{"hook_event_name": "ElicitationResult"}, Working},
		{"stop", map[string]any{"hook_event_name": "Stop"}, Idle},
		{"interrupt", map[string]any{"hook_event_name": "Interrupt"}, Idle},
		{"idle notice", map[string]any{"hook_event_name": "Notification", "notification_type": "idle_prompt"}, Idle},
		{"end", map[string]any{"hook_event_name": "SessionEnd"}, Ended},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, ok := Transition(parse(t, hook(t, tc.input)), now)
			if !ok {
				t.Fatal("event must be recognised")
			}
			if rec.State != tc.want {
				t.Errorf("state = %q, want %q", rec.State, tc.want)
			}
			if rec.SessionID != id {
				t.Errorf("session id = %q", rec.SessionID)
			}
		})
	}
}

func TestUnrelatedEventsAreIgnored(t *testing.T) {
	for _, input := range []map[string]any{
		{"hook_event_name": "Notification", "notification_type": "auth_success"},
		{"hook_event_name": "PreToolUse", "tool_name": "Bash"},
		{"hook_event_name": "PreCompact"},
	} {
		if _, ok := Transition(parse(t, hook(t, input)), now); ok {
			t.Errorf("event %v must not change state", input)
		}
	}
}

func TestBlockedRecordNamesTheTool(t *testing.T) {
	rec, _ := Transition(parse(t, hook(t, map[string]any{"hook_event_name": "PermissionRequest", "tool_name": "Bash"})), now)

	if rec.Detail != "Bash" {
		t.Errorf("detail = %q", rec.Detail)
	}
}

func TestApplyWritesRecordReadableByStore(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "sessions"))
	input := strings.NewReader(hook(t, map[string]any{"hook_event_name": "PermissionRequest", "tool_name": "Edit"}))

	if _, _, err := Apply(store, id, input, now); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	rec, ok, err := store.Read(id)
	if err != nil || !ok {
		t.Fatalf("Read: ok=%v err=%v", ok, err)
	}
	if rec.State != Blocked || rec.Detail != "Edit" || !rec.UpdatedAt.Equal(now) {
		t.Errorf("record = %+v", rec)
	}
}

func TestApplyIgnoresUnknownEventAndMalformedInputQuietly(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "sessions"))

	if _, _, err := Apply(store, id, strings.NewReader(hook(t, map[string]any{"hook_event_name": "PreCompact"})), now); err != nil {
		t.Errorf("unknown event must not error: %v", err)
	}
	if _, _, err := Apply(store, id, strings.NewReader("not json"), now); err == nil {
		t.Error("malformed input must be reported")
	}
	if _, ok, _ := store.Read(id); ok {
		t.Error("nothing must be written")
	}
}

func TestApplyIgnoresSessionsNotStartedByWatcher(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "sessions"))

	_, _, err := Apply(store, "", strings.NewReader(hook(t, map[string]any{"hook_event_name": "Stop"})), now)

	if err != nil {
		t.Errorf("foreign session must be ignored quietly: %v", err)
	}
	if _, ok, _ := store.Read(id); ok {
		t.Error("nothing must be written for a session without a watcher id")
	}
}

func TestApplyKeysRecordByWatcherSessionNotAgentSession(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "sessions"))
	input := strings.NewReader(`{"session_id":"thr_codex_123","hook_event_name":"Stop"}`)

	if _, _, err := Apply(store, id, input, now); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if _, ok, _ := store.Read(id); !ok {
		t.Error("record must be stored under the watcher session id")
	}
	if _, ok, _ := store.Read("thr_codex_123"); ok {
		t.Error("agent's own session id must not be used as key")
	}
}

func TestMergeHooksJSONKeepsForeignHooksAndStaysIdempotent(t *testing.T) {
	existing := []byte(`{
  "description": "user hooks",
  "hooks": {
    "PostToolUse": [{"matcher": "Edit|Write", "hooks": [{"type": "command", "command": "/Users/me/.codex/hooks/ide.py", "timeout": 30}]}],
    "Stop": [{"hooks": [{"type": "command", "command": "'/old/handoffd' -config '/c' hook", "timeout": 5}]}]
  }
}`)
	groups := Groups("/usr/local/bin/handoffd", "/cfg/config.json", CodexEvents)

	once, err := MergeHooksJSON(existing, groups)
	if err != nil {
		t.Fatalf("MergeHooksJSON: %v", err)
	}
	twice, err := MergeHooksJSON(once, groups)
	if err != nil {
		t.Fatalf("second MergeHooksJSON: %v", err)
	}

	if string(once) != string(twice) {
		t.Errorf("merge must be idempotent:\n%s\n---\n%s", once, twice)
	}
	var doc struct {
		Description string                      `json:"description"`
		Hooks       map[string][]map[string]any `json:"hooks"`
	}
	if err := json.Unmarshal(once, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Description != "user hooks" {
		t.Error("top-level metadata must survive")
	}
	if len(doc.Hooks["PostToolUse"]) != 2 {
		t.Errorf("PostToolUse groups = %d, want the user's hook plus ours", len(doc.Hooks["PostToolUse"]))
	}
	if len(doc.Hooks["Stop"]) != 1 {
		t.Errorf("Stop groups = %d, stale watcher hook must be replaced, not duplicated", len(doc.Hooks["Stop"]))
	}
	if _, ok := doc.Hooks["Notification"]; ok {
		t.Error("codex hook set must not carry Claude-only events")
	}
	if len(doc.Hooks["Interrupt"]) != 1 {
		t.Error("codex hook set must include Interrupt")
	}
}

func TestMergeHooksJSONStartsFromEmptyFile(t *testing.T) {
	out, err := MergeHooksJSON(nil, Groups("/bin/handoffd", "/c", CodexEvents))
	if err != nil {
		t.Fatalf("MergeHooksJSON: %v", err)
	}
	if !strings.Contains(string(out), `"SessionStart"`) {
		t.Errorf("out = %s", out)
	}
}

func TestReadReportsMissingSession(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "sessions"))

	_, ok, err := store.Read(id)

	if err != nil || ok {
		t.Errorf("ok=%v err=%v, want a quiet miss", ok, err)
	}
}

func TestReadRejectsUnsafeSessionID(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "sessions"))

	if _, _, err := store.Read("../etc/passwd"); err == nil {
		t.Error("path traversal in session id must be rejected")
	}
}

func TestHookSettingsCoverLifecycleEvents(t *testing.T) {
	raw, err := SettingsJSON("/usr/local/bin/handoffd", "/cfg/config.json")
	if err != nil {
		t.Fatalf("SettingsJSON: %v", err)
	}

	var settings struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		t.Fatalf("settings are not JSON: %v\n%s", err, raw)
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PermissionRequest", "PermissionDenied",
		"PostToolUse", "PostToolUseFailure", "Notification", "Elicitation", "ElicitationResult", "Stop", "SessionEnd"} {
		groups, ok := settings.Hooks[event]
		if !ok || len(groups) == 0 || len(groups[0].Hooks) == 0 {
			t.Errorf("event %s has no hook", event)
			continue
		}
		h := groups[0].Hooks[0]
		if h.Type != "command" || !strings.Contains(h.Command, "handoffd") || !strings.Contains(h.Command, " hook") {
			t.Errorf("event %s hook = %+v", event, h)
		}
		if h.Timeout <= 0 || h.Timeout > 10 {
			t.Errorf("event %s timeout = %d, hooks must stay short", event, h.Timeout)
		}
	}
	if settings.Hooks["PreToolUse"][0].Matcher != "AskUserQuestion" {
		t.Errorf("PreToolUse must only watch questions to the user, matcher = %q", settings.Hooks["PreToolUse"][0].Matcher)
	}
}

func TestSettingsQuotePathsForShell(t *testing.T) {
	raw, err := SettingsJSON("/Users/me/my bin/handoffd", "/Users/me/.config/handoffd/config.json")
	if err != nil {
		t.Fatalf("SettingsJSON: %v", err)
	}

	if !strings.Contains(raw, `'/Users/me/my bin/handoffd'`) {
		t.Errorf("binary path with spaces must be single-quoted: %s", raw)
	}
}

func parse(t *testing.T, raw string) HookInput {
	t.Helper()
	in, err := Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func TestAdvanceAccumulatesTimelineAcrossHooks(t *testing.T) {
	steps := []struct {
		input map[string]any
		after time.Duration
	}{
		{map[string]any{"hook_event_name": "SessionStart"}, 0},
		{map[string]any{"hook_event_name": "UserPromptSubmit"}, time.Second},
		{map[string]any{"hook_event_name": "PermissionRequest", "tool_name": "Bash"}, 10 * time.Second},
		{map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash"}, 70 * time.Second},
		{map[string]any{"hook_event_name": "PreToolUse", "tool_name": "AskUserQuestion"}, 90 * time.Second},
		{map[string]any{"hook_event_name": "SessionEnd"}, 120 * time.Second},
	}
	var rec Record
	for _, step := range steps {
		next, ok := Advance(rec, parse(t, hook(t, step.input)), now.Add(step.after))
		if !ok {
			t.Fatalf("step %v must be recognised", step.input)
		}
		rec = next
	}

	if !rec.StartedAt.Equal(now) {
		t.Errorf("started at = %s", rec.StartedAt)
	}
	if !rec.FirstActionAt.Equal(now.Add(70 * time.Second)) {
		t.Errorf("first action at = %s", rec.FirstActionAt)
	}
	if rec.BlockedMS != 90000 || rec.BlockedCount != 2 || rec.Turns != 1 || rec.State != Ended {
		t.Errorf("record = %+v", rec)
	}
	stats := rec.Stats(now.Add(120 * time.Second))
	if stats.DurationMS != 120000 || stats.FirstActionMS != 70000 || stats.BlockedMS != 90000 || stats.BlockedCount != 2 {
		t.Errorf("stats = %+v", stats)
	}
}

func TestApplyKeepsTimelineBetweenProcesses(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "sessions"))
	events := []map[string]any{
		{"hook_event_name": "SessionStart"},
		{"hook_event_name": "PermissionRequest", "tool_name": "Bash"},
		{"hook_event_name": "PostToolUse", "tool_name": "Bash"},
	}
	var (
		rec Record
		ok  bool
	)
	for i, ev := range events {
		var err error
		rec, ok, err = Apply(store, id, strings.NewReader(hook(t, ev)), now.Add(time.Duration(i)*time.Minute))
		if err != nil || !ok {
			t.Fatalf("Apply %v: ok=%v err=%v", ev, ok, err)
		}
	}

	if rec.BlockedMS != 60000 || rec.BlockedCount != 1 || !rec.StartedAt.Equal(now) {
		t.Errorf("record = %+v", rec)
	}
	if got := rec.BlockedFor(now.Add(time.Hour)); got != 0 {
		t.Errorf("a working session is not blocked, got %s", got)
	}
	blocked := Record{State: Blocked, UpdatedAt: now}
	if got := blocked.BlockedFor(now.Add(20 * time.Minute)); got != 20*time.Minute {
		t.Errorf("blocked for = %s", got)
	}
}

func TestWithPermissionsAddsReviewerProfileNextToHooks(t *testing.T) {
	base, err := SettingsJSON("/usr/local/bin/handoffd", "/cfg/config.json")
	if err != nil {
		t.Fatal(err)
	}

	got, err := WithPermissions(base, []string{"Read", "Bash(git log:*)"}, []string{"Bash(git push:*)"})
	if err != nil {
		t.Fatalf("WithPermissions: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("settings must stay valid JSON: %v", err)
	}
	if _, ok := doc["hooks"].(map[string]any)["SessionEnd"]; !ok {
		t.Error("hooks must survive the permission merge")
	}
	perms := doc["permissions"].(map[string]any)
	if allow := perms["allow"].([]any); len(allow) != 2 || allow[1] != "Bash(git log:*)" {
		t.Errorf("allow = %v", allow)
	}
	if deny := perms["deny"].([]any); len(deny) != 1 || deny[0] != "Bash(git push:*)" {
		t.Errorf("deny = %v", deny)
	}
}

func TestWithPermissionsLeavesSettingsAloneWithoutRules(t *testing.T) {
	if got, err := WithPermissions(`{"hooks":{}}`, nil, nil); err != nil || got != `{"hooks":{}}` {
		t.Errorf("got %q err %v", got, err)
	}
	got, err := WithPermissions("", nil, []string{"Bash(git push:*)"})
	if err != nil || !strings.Contains(got, `"deny":["Bash(git push:*)"]`) {
		t.Errorf("empty base must still carry the deny list: %q %v", got, err)
	}
	if _, err := WithPermissions("not json", nil, []string{"x"}); err == nil {
		t.Error("broken base settings must be reported")
	}
}
