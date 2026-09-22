package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kriso1337/handoffd/internal/config"
	"github.com/Kriso1337/handoffd/internal/promptfile"
	"github.com/Kriso1337/handoffd/internal/session"
)

func cfgWith(claude, codex string) config.Config {
	cfg := config.Defaults()
	cfg.ClaudeBin = claude
	cfg.CodexBin = codex
	return cfg
}

func present(paths ...string) func(string) error {
	return func(path string) error {
		for _, p := range paths {
			if p == path {
				return nil
			}
		}
		return os.ErrNotExist
	}
}

func noLookup(string) (string, error) { return "", errors.New("not in PATH") }

func TestDetectFindsConfiguredBinaries(t *testing.T) {
	got := Detect(cfgWith("/u/claude", "/u/codex"), noLookup, present("/u/claude", "/u/codex"))

	if len(got) != 2 || got[0].Kind != Claude || got[1].Kind != Codex {
		t.Errorf("detected = %+v", got)
	}
}

func TestDetectFallsBackToPATH(t *testing.T) {
	lookup := func(name string) (string, error) {
		if name == "codex" {
			return "/opt/homebrew/bin/codex", nil
		}
		return "", errors.New("no")
	}

	got := Detect(cfgWith("/missing/claude", "/missing/codex"), lookup, present())

	if len(got) != 1 || got[0].Kind != Codex || got[0].Bin != "/opt/homebrew/bin/codex" {
		t.Errorf("detected = %+v", got)
	}
}

func TestResolveUsesConfiguredAgent(t *testing.T) {
	cfg := cfgWith("/u/claude", "/u/codex")
	cfg.Agent = "codex"

	got, err := Resolve(cfg, []Installed{{Claude, "/u/claude"}, {Codex, "/u/codex"}})

	if err != nil || got.Kind != Codex || got.Bin != "/u/codex" {
		t.Errorf("resolved = %+v, err = %v", got, err)
	}
}

func TestResolveRefusesConfiguredAgentThatIsNotInstalled(t *testing.T) {
	cfg := cfgWith("/u/claude", "/u/codex")
	cfg.Agent = "codex"

	if _, err := Resolve(cfg, []Installed{{Claude, "/u/claude"}}); err == nil || !strings.Contains(err.Error(), "codex") {
		t.Errorf("err = %v", err)
	}
}

func TestResolvePicksTheOnlyInstalledAgent(t *testing.T) {
	got, err := Resolve(cfgWith("", ""), []Installed{{Codex, "/u/codex"}})

	if err != nil || got.Kind != Codex {
		t.Errorf("resolved = %+v, err = %v", got, err)
	}
}

func TestResolveDemandsChoiceWhenSeveralAreInstalled(t *testing.T) {
	_, err := Resolve(cfgWith("", ""), []Installed{{Claude, "/u/claude"}, {Codex, "/u/codex"}})

	if err == nil || !strings.Contains(err.Error(), "setup-agent") {
		t.Errorf("err = %v, must point at setup-agent", err)
	}
}

func TestResolveFailsWithoutAnyAgent(t *testing.T) {
	if _, err := Resolve(cfgWith("", ""), nil); err == nil {
		t.Error("no agent must be an error")
	}
}

func TestChooseReadsNumberFromInput(t *testing.T) {
	detected := []Installed{{Claude, "/u/claude"}, {Codex, "/u/codex"}}
	var out strings.Builder

	got, err := Choose(detected, strings.NewReader("2\n"), &out)

	if err != nil || got.Kind != Codex {
		t.Errorf("chosen = %+v, err = %v", got, err)
	}
	if !strings.Contains(out.String(), "1) claude") || !strings.Contains(out.String(), "2) codex") {
		t.Errorf("menu = %q", out.String())
	}
}

func TestChooseRejectsBadAnswer(t *testing.T) {
	detected := []Installed{{Claude, "/u/claude"}, {Codex, "/u/codex"}}

	for _, answer := range []string{"", "3\n", "x\n"} {
		if _, err := Choose(detected, strings.NewReader(answer), &strings.Builder{}); err == nil {
			t.Errorf("answer %q must be rejected", answer)
		}
	}
}

func TestWriteConfigSetsAgentAndKeepsOtherKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"tmux_session": "main", "repo_aliases": {"legacy-service-a": "service-a"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := WriteConfig(path, Codex); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	var doc map[string]any
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("config is not JSON: %v", err)
	}
	if doc["agent"] != "codex" || doc["tmux_session"] != "main" {
		t.Errorf("config = %v", doc)
	}
	if aliases, _ := doc["repo_aliases"].(map[string]any); aliases["legacy-service-a"] != "service-a" {
		t.Errorf("nested keys lost: %v", doc)
	}
}

func TestWriteConfigCreatesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")

	if err := WriteConfig(path, Claude); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `"agent": "claude"`) {
		t.Errorf("config = %s", raw)
	}
}

func TestClaudeCommandCarriesSessionHooksAndToolPolicy(t *testing.T) {
	argv := Command(Installed{Claude, "/u/claude"}, promptfile.Payload{
		Prompt:       "review",
		Model:        "claude-opus-5",
		AddDirs:      []string{"/wt2", "/wt3"},
		AllowedTools: []string{"Write", "mcp__slack__thread"},
		DeniedTools:  []string{"Bash(git push:*)"},
		SessionID:    "33333333-3333-3333-3333-333333333333",
		Settings:     `{"hooks":{}}`,
	})

	got := strings.Join(argv, " ")
	for _, want := range []string{
		"/u/claude ", "--model claude-opus-5", "--session-id 33333333-3333-3333-3333-333333333333",
		`--settings {"hooks":{}}`, "--add-dir /wt2 --add-dir /wt3", "--allowedTools Write,mcp__slack__thread",
		"--disallowedTools Bash(git push:*)", "-- review",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("argv %q missing %q", got, want)
		}
	}
	if argv[len(argv)-1] != "review" || argv[len(argv)-2] != "--" {
		t.Errorf("prompt must come last after --: %v", argv)
	}
}

func TestClaudeCommandSkipsEmptyOptions(t *testing.T) {
	argv := Command(Installed{Claude, "/u/claude"}, promptfile.Payload{Prompt: "p"})

	if len(argv) != 3 || argv[1] != "--" {
		t.Errorf("argv = %v", argv)
	}
}

func TestCodexWithoutReviewerPolicyKeepsTheDefaultSandbox(t *testing.T) {
	argv := strings.Join(Command(Installed{Codex, "/u/codex"}, promptfile.Payload{Prompt: "help"}), " ")

	if strings.Contains(argv, "--sandbox") {
		t.Errorf("help window must not be sandboxed read-only: %q", argv)
	}
}

func TestCodexCommandUsesItsOwnFlagsAndIgnoresClaudeOnlyOptions(t *testing.T) {
	argv := Command(Installed{Codex, "/u/codex"}, promptfile.Payload{
		Prompt:       "review",
		Model:        "gpt-5.6-sol",
		AddDirs:      []string{"/wt2"},
		AllowedTools: []string{"Write"},
		DeniedTools:  []string{"Bash(git push:*)"},
		SessionID:    "33333333-3333-3333-3333-333333333333",
		Settings:     `{"hooks":{}}`,
	})

	got := strings.Join(argv, " ")
	for _, want := range []string{"/u/codex ", "--model gpt-5.6-sol", "--add-dir /wt2", "--sandbox read-only", "-- review"} {
		if !strings.Contains(got, want) {
			t.Errorf("argv %q missing %q", got, want)
		}
	}
	for _, leaked := range []string{"--session-id", "--settings", "--allowedTools", "--disallowedTools"} {
		if strings.Contains(got, leaked) {
			t.Errorf("codex must not get Claude flag %s: %q", leaked, got)
		}
	}
}

func TestSupportsToolDenialOnlyForClaude(t *testing.T) {
	if !(Installed{Kind: Claude}).SupportsToolDenial() || (Installed{Kind: Codex}).SupportsToolDenial() {
		t.Error("tool denial is a Claude Code feature")
	}
}

func TestInstallCodexHooksMergesAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".codex", "hooks.json")
	groups := session.Groups("/u/handoffd", "/cfg", session.CodexEvents)

	first, err := InstallCodexHooks(path, groups)
	if err != nil {
		t.Fatalf("InstallCodexHooks: %v", err)
	}
	second, err := InstallCodexHooks(path, groups)
	if err != nil {
		t.Fatalf("second InstallCodexHooks: %v", err)
	}

	if !first || second {
		t.Errorf("changed = %v then %v, want true then false", first, second)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "/u/handoffd") {
		t.Errorf("hooks.json = %s", raw)
	}
}

func TestHookSettingsOnlyForClaude(t *testing.T) {
	if (Installed{Kind: Codex}).PerSessionHooks() {
		t.Error("codex has no per-session hook settings, hooks live in ~/.codex/hooks.json")
	}
	if !(Installed{Kind: Claude}).PerSessionHooks() {
		t.Error("claude takes hooks via --settings")
	}
}

func TestNamesListsDetectedKinds(t *testing.T) {
	got := Names([]Installed{{Kind: Claude, Bin: "/bin/claude"}, {Kind: Codex, Bin: "/bin/codex"}})

	if len(got) != 2 || got[0] != "claude" || got[1] != "codex" {
		t.Errorf("names = %v", got)
	}
	if got := Names(nil); len(got) != 0 {
		t.Errorf("names of nothing = %v", got)
	}
}
