package setup

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func answers(lines ...string) *bufio.Reader {
	return bufio.NewReader(strings.NewReader(strings.Join(lines, "\n") + "\n"))
}

func fakeEnv(in *bufio.Reader) Environment {
	return Environment{
		ReadSecret: func(string) (string, error) {
			line, _ := in.ReadString('\n')
			return strings.TrimSpace(line), nil
		},
		Verify: func(token, cookie string) (Identity, error) {
			if token == "xoxc-browser" && cookie == "" {
				return Identity{}, errors.New("auth.test: invalid_auth")
			}
			if !strings.HasPrefix(token, "xox") {
				return Identity{}, errors.New("auth.test: invalid_auth")
			}
			return Identity{UserID: "U0ALEX", User: "alex", Team: "Acme", URL: "https://acme.slack.com/"}, nil
		},
		FindChannel: func(_, _, name string) (string, error) {
			if name == "team-agents" {
				return "C0AGENTS", nil
			}
			return "", nil
		},
		HelperSuggestion: []string{"/usr/local/bin/slack-helper", "--profile", "work"},
		Terminals:        []string{"tmux", "wezterm"},
		Alerts:           true,
	}
}

func TestTokenModeBuildsEnglishConfigAndEnablesReviewExplicitly(t *testing.T) {
	in := answers(
		"1",
		"xoxp-secret",
		"",
		"",
		"team-agents",
		"",
		"Ada",
		"Alex",
		"Alex Smith",
		"",
		"Jira, GitLab",
		"Orbit",
		"Alex Twin (U02)",
		"",
		"ada, alex",
		"",
		"ada, halt ; ada stop",
		"",
		"",
		"mcp__slack__slack_thread",
		"",
		"work",
		"/srv/code",
		"gitlab.example.com",
		"",
		"yes",
		"no",
	)
	var out strings.Builder

	a, err := Ask(in, &out, nil, fakeEnv(in))

	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if a.SlackMode != SlackModeToken || a.Token != "xoxp-secret" || a.Cookie != "" {
		t.Errorf("slack = %+v", a)
	}
	if a.SelfUserID != "U0ALEX" || a.Workspace != "acme" || a.Channel != "C0AGENTS" || a.ChannelName != "team-agents" {
		t.Errorf("identity = %+v", a)
	}
	if a.OwnerFull != "Alex Smith" || a.OwnerGenitive != "Alex" || a.OwnerDative != "Alex" || a.OwnerFullGenitive != "Alex Smith" {
		t.Errorf("persona = %+v", a)
	}
	if strings.Join(a.MentionWords, ",") != "ada" || strings.Join(a.TriageWords, ",") != "ada,alex" || a.MacroPhrases[0] != "ada, help" {
		t.Errorf("words = %+v", a)
	}
	if !a.ReviewEnabled || a.Alerts || a.Terminal != "tmux" || a.TmuxSession != "work" {
		t.Errorf("runtime = %+v", a)
	}
	for _, want := range []string{"authenticated as alex (U0ALEX) in Acme", "resolved #team-agents", "Enable automatic MR/PR review workflows"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "xoxp-secret") {
		t.Error("the token must never be echoed")
	}

	cfg := a.Config()
	if cfg["prompts_language"] != "en" || cfg["review_enabled"] != true {
		t.Errorf("config = %v", cfg)
	}
	if words := cfg["ack_words"].([]string); words[0] != "yes" {
		t.Errorf("ack_words = %v", words)
	}
	if tools := cfg["triage_tools"].([]string); len(tools) != 2 || tools[0] != "Write" {
		t.Errorf("triage_tools = %v", tools)
	}
}

func TestBrowserTokenAsksForCookieAndRetriesOnRejection(t *testing.T) {
	in := answers(
		"1",
		"bad-token",
		"xoxc-browser", "",
		"xoxc-browser", "xoxd-cookie",
		"", "", "team-agents", "",
		"Ada", "Alex", "", "", "", "", "",
		"", "", "", "",
		"", "", "",
		"wezterm",
		"", "", "",
		"no",
		"yes",
	)

	a, err := Ask(in, &strings.Builder{}, nil, fakeEnv(in))

	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if a.Token != "xoxc-browser" || a.Cookie != "xoxd-cookie" || a.Terminal != "wezterm" || a.ReviewEnabled || !a.Alerts {
		t.Errorf("answers = %+v", a)
	}
	if a.MacroPhrases[0] != "ada, help" || a.StopPhrases[0] != "ada, stop" {
		t.Errorf("derived phrases = %+v", a)
	}
}

func TestHelperModeKeepsExistingValuesAsDefaults(t *testing.T) {
	existing := map[string]any{
		"fetch_helper": []any{"python3", "/opt/helper.py"}, "self_user_id": "U0OLD", "slack_workspace": "old",
		"channel": "C0OLD", "channel_name": "agents", "persona": map[string]any{"agent": "Ada", "owner": "Alex", "owner_full": "Alex Smith"},
		"namesake": "Twin", "triage_tools": []any{"Write", "mcp__slack__slack_thread"}, "stop_phrases": []any{"ada, halt"},
		"terminal": "tmux", "tmux_session": "main", "review_enabled": true, "alert_command": []any{},
	}
	in := answers("", "", "", "", "", "", "", "", "", "", "", "", "", "", "", "", "", "", "", "", "", "", "", "", "")

	a, err := Ask(in, &strings.Builder{}, existing, fakeEnv(in))

	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if a.SlackMode != SlackModeHelper || strings.Join(a.FetchHelper, " ") != "python3 /opt/helper.py" {
		t.Errorf("slack = %+v", a)
	}
	if a.Agent != "Ada" || a.Owner != "Alex" || a.Namesake != "Twin" || !a.ReviewEnabled || a.Alerts {
		t.Errorf("defaults lost: %+v", a)
	}
}

func TestHelperSuggestionIsOfferedWithoutExistingHelper(t *testing.T) {
	in := answers("2", "")
	var out strings.Builder

	_, err := Ask(in, &out, nil, fakeEnv(in))

	if err == nil || !strings.Contains(err.Error(), "self_user_id") {
		t.Errorf("err = %v", err)
	}
	if !strings.Contains(out.String(), "[/usr/local/bin/slack-helper --profile work]") {
		t.Errorf("suggestion missing: %s", out.String())
	}
}

func TestWizardAcceptsEveryRegisteredTerminal(t *testing.T) {
	for _, name := range []string{"cmux", "herdr", "wezterm"} {
		var a Answers
		w := wizard{in: answers(name), out: &strings.Builder{}, existing: map[string]any{}}
		if err := w.terminal(&a); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if a.Terminal != name || a.TmuxSession != "" {
			t.Errorf("%s: %+v", name, a)
		}
	}
}

func TestWizardRejectsBadSlackAndPersonaAnswers(t *testing.T) {
	cases := map[string][]string{
		"mode":  {"3"},
		"token": {"1", "nope", "nope", "nope"},
		"agent": {"2", "python3 h.py", "U1", "acme", "agents", "C1", ""},
	}
	for name, lines := range cases {
		if _, err := Ask(answers(lines...), &strings.Builder{}, nil, fakeEnv(answers())); err == nil {
			t.Errorf("%s: bad input must be rejected", name)
		}
	}
}

func TestMergeWritesGivenKeysDropsDeprecatedOnesAndKeepsTheRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"tmux_session":"work","channel":"OLD","haiku_tools":["Write"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Merge(path, map[string]any{"channel": "C1"}); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	var doc map[string]any
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["tmux_session"] != "work" || doc["channel"] != "C1" {
		t.Errorf("config = %v", doc)
	}
	if _, ok := doc["haiku_tools"]; ok {
		t.Error("deprecated keys must be dropped")
	}
}

func TestWriteSecretIsPrivateAndSkipsEmptyValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "slack_token")
	if err := WriteSecret(path, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("empty secret must not create a file")
	}
	if err := WriteSecret(path, "xoxp-1"); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	raw, _ := os.ReadFile(path)
	if info.Mode().Perm() != 0o600 || string(raw) != "xoxp-1\n" {
		t.Errorf("mode %v content %q", info.Mode().Perm(), raw)
	}
}

func TestExistingReadsRawConfigOrEmpty(t *testing.T) {
	if got, err := Existing(filepath.Join(t.TempDir(), "absent.json")); err != nil || len(got) != 0 {
		t.Errorf("absent = %v, %v", got, err)
	}
}

func TestTriageToolsExtendReviewerAllowList(t *testing.T) {
	cfg := Answers{Agent: "Atlas", Owner: "Alex", TriageTools: []string{"mcp__slack__slack_thread"}}.Config()
	allowed := cfg["review_allowed_tools"].([]string)
	if allowed[len(allowed)-1] != "mcp__slack__slack_thread" || allowed[0] != "Read" {
		t.Errorf("review_allowed_tools = %v", allowed)
	}
	plain := Answers{Agent: "Atlas", Owner: "Alex"}.Config()
	if got := plain["macro_phrases"].([]string); got[0] != "atlas, help" {
		t.Errorf("phrases = %v", got)
	}
	if plain["review_enabled"] != false {
		t.Errorf("review must be disabled by default: %v", plain)
	}
}
