package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kriso1337/handoffd/internal/mrref"
)

func TestRepoDirDefaultsToProjectName(t *testing.T) {
	cfg := Config{ProjectsDir: "/projects"}

	got := cfg.RepoDir(mrref.MR{Host: "gitlab.example.com", Namespace: "team-a", Project: "service-a"})

	if got != filepath.Join("/projects", "service-a") {
		t.Errorf("repo dir = %q", got)
	}
}

func TestRepoDirPrefersMostSpecificAlias(t *testing.T) {
	cfg := Config{
		ProjectsDir: "/projects",
		RepoAliases: map[string]string{
			"service":                           "by-name",
			"team-b/service":                    "by-namespace",
			"gitlab.example.com/team-c/service": "by-host",
		},
	}

	cases := map[string]mrref.MR{
		"by-name":      {Host: "gitlab.example.com", Namespace: "team-a", Project: "service"},
		"by-namespace": {Host: "gitlab.example.com", Namespace: "team-b", Project: "service"},
		"by-host":      {Host: "gitlab.example.com", Namespace: "team-c", Project: "service"},
	}
	for want, mr := range cases {
		if got := cfg.RepoDir(mr); got != filepath.Join("/projects", want) {
			t.Errorf("RepoDir(%s/%s) = %q, want %q", mr.Namespace, mr.Project, got, want)
		}
	}
}

func TestRepoDirAcceptsAbsoluteAlias(t *testing.T) {
	cfg := Config{ProjectsDir: "/projects", RepoAliases: map[string]string{"service": "/srv/checkouts/service"}}

	if got := cfg.RepoDir(mrref.MR{Project: "service"}); got != "/srv/checkouts/service" {
		t.Errorf("repo dir = %q", got)
	}
}

func TestAnyHostAllowedWhenListIsEmpty(t *testing.T) {
	if !(Config{}).Allows(mrref.MR{Host: "gitlab.anywhere.com"}) {
		t.Error("empty allowlist must accept every host")
	}
}

func TestOnlyListedHostsAllowed(t *testing.T) {
	cfg := Config{GitLabHosts: []string{"gitlab.example.com"}}

	if !cfg.Allows(mrref.MR{Host: "GitLab.Example.com"}) {
		t.Error("listed host must be accepted regardless of case")
	}
	if cfg.Allows(mrref.MR{Host: "gitlab.other.com"}) {
		t.Error("unlisted host must be rejected")
	}
}

func complete() Config {
	cfg := Defaults()
	cfg.SelfUserID = "U1"
	cfg.Channel = "C1"
	cfg.SlackWorkspace = "acme"
	cfg.FetchHelper = []string{"python3", "helper.py"}
	cfg.Persona.Owner = "Ivan"
	cfg.Persona.Agent = "Ada"
	return cfg
}

func TestDefaultsCarryNoIdentityAndDemandInit(t *testing.T) {
	cfg := Defaults()
	if cfg.SelfUserID != "" || cfg.Channel != "" || len(cfg.MentionWords) != 0 || cfg.Persona.Owner != "" || len(cfg.FetchHelper) != 0 {
		t.Errorf("defaults must not carry a person's identity: %+v", cfg)
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "init") {
		t.Errorf("bare defaults must point at init: %v", err)
	}
}

func TestReviewWorkflowsAreOptIn(t *testing.T) {
	cfg := Defaults()
	if cfg.ReviewEnabled {
		t.Fatal("review workflows must be disabled by default")
	}
	rules := cfg.Rules()
	if len(rules.ReviewMarkers) != 0 || len(rules.ReviewHeadMarkers) != 0 || len(rules.ReviewWords) != 0 {
		t.Fatalf("disabled review workflows exposed review rules: %+v", rules)
	}

	cfg.ReviewEnabled = true
	rules = cfg.Rules()
	if len(rules.ReviewMarkers) == 0 || len(rules.ReviewHeadMarkers) == 0 || len(rules.ReviewWords) == 0 {
		t.Fatalf("enabled review workflows did not expose review rules: %+v", rules)
	}
}

func TestCompleteConfigValidates(t *testing.T) {
	if err := complete().Validate(); err != nil {
		t.Errorf("complete config must validate: %v", err)
	}
}

func TestSlackLogPathFollowsTheOS(t *testing.T) {
	cases := map[string]string{
		"darwin":  "/home/u/Library/Containers/com.tinyspeck.slackmacgap/Data/Library/Application Support/Slack/logs/default/webapp-console.log",
		"linux":   "/home/u/.config/Slack/logs/default/webapp-console.log",
		"freebsd": "/home/u/.config/Slack/logs/default/webapp-console.log",
	}
	for goos, want := range cases {
		if got := SlackLogPath(goos, "/home/u"); got != want {
			t.Errorf("%s: %q, want %q", goos, got, want)
		}
	}
}

func TestBinaryPrefersPATHThenFallbacks(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "tmux")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	if got := Binary("tmux", "/nowhere/tmux"); got != fake {
		t.Errorf("PATH lookup = %q, want %q", got, fake)
	}
	if got := Binary("definitely-missing-binary", "/nowhere/a", fake); got != fake {
		t.Errorf("existing fallback = %q, want %q", got, fake)
	}
	if got := Binary("definitely-missing-binary", "/nowhere/a"); got != "/nowhere/a" {
		t.Errorf("last resort = %q, want the first fallback", got)
	}
}

func TestReadDoesNotValidate(t *testing.T) {
	cfg, err := Read(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil || cfg.LogPath == "" {
		t.Errorf("Read = %+v, %v", cfg, err)
	}
}

func TestValidateRejectsUnknownPromptsLanguage(t *testing.T) {
	cfg := complete()
	cfg.PromptsLanguage = "de"
	if err := cfg.Validate(); err == nil {
		t.Error("unsupported language must be rejected")
	}
}

func TestLoadKeepsDefaultsForOmittedPersonaFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"persona": {"owner": "Ivan", "agent": "Ada"}, "main_model": "claude-opus-5"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Read(path)

	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if cfg.Persona.Owner != "Ivan" || cfg.Persona.Agent != "Ada" {
		t.Errorf("persona = %+v", cfg.Persona)
	}
	if cfg.Persona.ThreadTool == "" || cfg.Persona.TriageModel == "" {
		t.Errorf("omitted persona fields must keep defaults: %+v", cfg.Persona)
	}
	if cfg.MainModel != "claude-opus-5" {
		t.Errorf("main model = %q", cfg.MainModel)
	}
}

func TestValidateRejectsEmptyPersona(t *testing.T) {
	cfg := complete()
	cfg.Persona.Agent = ""

	if err := cfg.Validate(); err == nil {
		t.Error("persona without agent name must be rejected")
	}
}

func TestTriageModelFallsBackToLegacyHaikuKey(t *testing.T) {
	cfg := Config{HaikuModel: "claude-haiku-4-5-20251001"}

	if cfg.TriageModelName() != "claude-haiku-4-5-20251001" {
		t.Errorf("triage model = %q", cfg.TriageModelName())
	}
	cfg.TriageModel = "gpt-5.6-mini"
	if cfg.TriageModelName() != "gpt-5.6-mini" {
		t.Errorf("triage_model must win: %q", cfg.TriageModelName())
	}
}

func TestValidateRejectsUnknownAgent(t *testing.T) {
	cfg := complete()
	cfg.Agent = "gemini"

	if err := cfg.Validate(); err == nil {
		t.Error("unknown agent must be rejected")
	}
	for _, ok := range []string{"", "claude", "codex"} {
		cfg.Agent = ok
		if err := cfg.Validate(); err != nil {
			t.Errorf("agent %q must validate: %v", ok, err)
		}
	}
}

func TestValidateKnowsTerminals(t *testing.T) {
	cfg := complete()
	cfg.Terminal = "zellij"
	if err := cfg.Validate(); err == nil {
		t.Error("unknown terminal must be rejected")
	}
	cfg.Terminal = "wezterm"
	cfg.TmuxSession = ""
	if err := cfg.Validate(); err != nil {
		t.Errorf("wezterm needs no tmux session: %v", err)
	}
}

func TestWorktreeDirMustBeRelative(t *testing.T) {
	cfg := complete()
	cfg.WorktreeDir = "/tmp/trees"
	if err := cfg.Validate(); err == nil {
		t.Error("absolute worktree_dir must be rejected")
	}
}

func TestGitHubHostsAreCheckedSeparately(t *testing.T) {
	cfg := Config{GitLabHosts: []string{"gitlab.example.com"}}
	pr := mrref.MR{Host: "github.com", Forge: mrref.ForgeGitHub}

	if !cfg.Allows(pr) {
		t.Error("without github_hosts any GitHub host is allowed")
	}
	cfg.GitHubHosts = []string{"github.example.com"}
	if cfg.Allows(pr) || !cfg.Allows(mrref.MR{Host: "GitHub.Example.com", Forge: mrref.ForgeGitHub}) {
		t.Error("github_hosts must gate GitHub links case-insensitively")
	}
	if cfg.HostsKey(pr) != "github_hosts" || cfg.HostsKey(mrref.MR{}) != "gitlab_hosts" {
		t.Error("hosts key must name the forge list")
	}
}

func TestSlackLogPathsOfferBothMacInstallLayouts(t *testing.T) {
	paths := SlackLogPaths("darwin", "/home/u")

	if len(paths) != 2 {
		t.Fatalf("paths = %v, a mac has both the App Store container and the plain DMG layout", paths)
	}
	if !strings.Contains(paths[0], "Library/Containers/com.tinyspeck.slackmacgap") {
		t.Errorf("paths[0] = %q, want the container layout", paths[0])
	}
	if paths[1] != "/home/u/Library/Application Support/Slack/logs/default/webapp-console.log" {
		t.Errorf("paths[1] = %q, want the DMG layout", paths[1])
	}
}

func TestSlackLogPathPicksTheLayoutThatExists(t *testing.T) {
	home := t.TempDir()
	dmg := filepath.Join(home, "Library/Application Support/Slack/logs/default")
	if err := os.MkdirAll(dmg, 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dmg, "webapp-console.log")
	if err := os.WriteFile(log, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if got := SlackLogPath("darwin", home); got != log {
		t.Errorf("path = %q, want the layout that is actually on disk", got)
	}
}

func TestSlackLogPathFallsBackToTheFirstCandidate(t *testing.T) {
	home := t.TempDir()

	want := SlackLogPaths("darwin", home)[0]
	if got := SlackLogPath("darwin", home); got != want {
		t.Errorf("path = %q, want %q when neither layout exists", got, want)
	}
}
