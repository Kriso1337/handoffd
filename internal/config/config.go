package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Kriso1337/handoffd/internal/alert"
	"github.com/Kriso1337/handoffd/internal/launcher"
	"github.com/Kriso1337/handoffd/internal/mention"
	"github.com/Kriso1337/handoffd/internal/mrref"
	"github.com/Kriso1337/handoffd/internal/prompt"
)

type Config struct {
	LogPath            string            `json:"log_path"`
	Channel            string            `json:"channel"`
	ChannelName        string            `json:"channel_name"`
	SelfUserID         string            `json:"self_user_id"`
	SlackWorkspace     string            `json:"slack_workspace"`
	MentionWords       []string          `json:"mention_words"`
	TriageWords        []string          `json:"triage_words"`
	MacroPhrases       []string          `json:"macro_phrases"`
	StopPhrases        []string          `json:"stop_phrases"`
	ReviewEnabled      bool              `json:"review_enabled"`
	ReviewMarkers      []string          `json:"review_markers"`
	ReviewHeadMarkers  []string          `json:"review_head_markers"`
	ReviewWords        []string          `json:"review_words"`
	AckWords           []string          `json:"ack_words"`
	ProgressMarkers    map[string]string `json:"progress_markers"`
	Namesake           string            `json:"namesake"`
	PeerAgents         []string          `json:"peer_agents"`
	Persona            prompt.Persona    `json:"persona"`
	PromptsDir         string            `json:"prompts_dir"`
	PromptsLanguage    string            `json:"prompts_language"`
	Agent              string            `json:"agent"`
	MainModel          string            `json:"main_model"`
	TriageModel        string            `json:"triage_model"`
	ReviewAllowedTools []string          `json:"review_allowed_tools"`
	ReviewDeniedTools  []string          `json:"review_denied_tools"`
	PostLabel          string            `json:"post_label"`
	TriageTools        []string          `json:"triage_tools"`
	HaikuModel         string            `json:"haiku_model"`
	HaikuTools         []string          `json:"haiku_tools"`
	TriageTimeoutSec   int               `json:"triage_timeout_seconds"`
	TriageLimit        int               `json:"triage_limit"`
	TriageCooldownN    int               `json:"triage_cooldown_after"`
	TriageCooldownMin  int               `json:"triage_cooldown_minutes"`
	TriageExamples     int               `json:"triage_examples"`
	QueueSize          int               `json:"queue_size"`
	Terminal           string            `json:"terminal"`
	TmuxSession        string            `json:"tmux_session"`
	WorktreeDir        string            `json:"worktree_dir"`
	ProjectsDir        string            `json:"projects_dir"`
	RepoAliases        map[string]string `json:"repo_aliases"`
	GitLabHosts        []string          `json:"gitlab_hosts"`
	GitHubHosts        []string          `json:"github_hosts"`
	StatePath          string            `json:"state_path"`
	FetchHelper        []string          `json:"fetch_helper"`
	HelperEnv          map[string]string `json:"helper_env"`
	SlackTokenFile     string            `json:"slack_token_file"`
	SlackCookieFile    string            `json:"slack_cookie_file"`
	ClaudeBin          string            `json:"claude_bin"`
	CodexBin           string            `json:"codex_bin"`
	GlabBin            string            `json:"glab_bin"`
	GhBin              string            `json:"gh_bin"`
	TmuxBin            string            `json:"tmux_bin"`
	WezTermBin         string            `json:"wezterm_bin"`
	HerdrBin           string            `json:"herdr_bin"`
	CmuxBin            string            `json:"cmux_bin"`
	CmuxPasswordFile   string            `json:"cmux_password_file"`
	HerdrSession       string            `json:"herdr_session"`
	GitBin             string            `json:"git_bin"`
	OtherLimit         int               `json:"other_limit"`
	OtherWindowSeconds int               `json:"other_window_seconds"`
	WorktreeTTLDays    int               `json:"worktree_ttl_days"`
	ThreadTTLDays      int               `json:"thread_ttl_days"`
	GoneThreadTTLDays  int               `json:"gone_thread_ttl_days"`
	LastReadMaxAgeSec  int               `json:"last_read_max_age_seconds"`
	DeliverTimeoutSec  int               `json:"deliver_timeout_seconds"`
	StaleHours         int               `json:"stale_hours"`
	ThreadPollSec      int               `json:"thread_poll_seconds"`
	ThreadPollHours    int               `json:"thread_poll_window_hours"`
	ThreadPollLimit    int               `json:"thread_poll_limit"`
	AlertCommand       []string          `json:"alert_command"`
	AlertCooldownMin   int               `json:"alert_cooldown_minutes"`
	BlockedAlertMin    int               `json:"blocked_alert_minutes"`
	SlackLogStaleHours int               `json:"slack_log_stale_hours"`
	WatchdogSec        int               `json:"watchdog_seconds"`
	CommitteeMarker    string            `json:"committee_marker"`
	CommitteeAgents    []string          `json:"committee_agents"`
	AgentModels        map[string]string `json:"agent_models"`
	CostPerTriageUSD   float64           `json:"cost_per_triage_usd"`
	CostPerWindowUSD   float64           `json:"cost_per_window_usd"`
	SeenCapacity       int               `json:"seen_capacity"`
}

var (
	ReviewerAllowedTools = []string{
		"Read", "Grep", "Glob",
		"Bash(git status:*)", "Bash(git log:*)", "Bash(git diff:*)", "Bash(git show:*)", "Bash(git blame:*)",
		"Bash(git fetch:*)", "Bash(git branch:*)", "Bash(git rev-parse:*)",
		"Bash(glab mr view:*)", "Bash(glab mr diff:*)",
		"Bash(gh pr view:*)", "Bash(gh pr diff:*)", "Bash(gh pr checks:*)",
		"Bash(handoffd post:*)",
	}
	ReviewerDeniedTools = []string{
		"Bash(git push:*)", "Bash(glab mr merge:*)", "Bash(glab mr approve:*)", "Bash(glab mr update:*)", "Bash(glab mr close:*)",
		"Bash(glab mr note:*)", "Bash(glab api:*)",
		"Bash(gh pr merge:*)", "Bash(gh pr review:*)", "Bash(gh pr close:*)", "Bash(gh pr edit:*)",
		"Bash(gh pr comment:*)", "Bash(gh api:*)",
		"mcp__slack-agent-bridge__post_to_agent_channel", "mcp__slack-agent-bridge__edit_agent_channel_message",
	}
)

func Defaults() Config {
	home, _ := os.UserHomeDir()
	return Config{
		LogPath: SlackLogPath(runtime.GOOS, home),
		Persona: prompt.Persona{
			MainModel:   "Opus",
			TriageModel: "Haiku",
			ThreadTool:  "Slack MCP",
		},
		ReviewMarkers:      mention.ReviewMarkersEN,
		ReviewHeadMarkers:  mention.ReviewHeadMarkersEN,
		ReviewWords:        mention.ReviewWordsEN,
		AckWords:           mention.AckWordsEN,
		ProgressMarkers:    mention.DefaultProgressMarkers,
		PromptsDir:         filepath.Join(home, ".config/handoffd/prompts"),
		PromptsLanguage:    "en",
		ReviewAllowedTools: ReviewerAllowedTools,
		ReviewDeniedTools:  ReviewerDeniedTools,
		HaikuModel:         "claude-haiku-4-5-20251001",
		TriageTools:        []string{"Write"},
		Terminal:           "tmux",
		TmuxSession:        "main",
		WorktreeDir:        filepath.Join(".claude", "worktrees"),
		ProjectsDir:        filepath.Join(home, "projects"),
		RepoAliases:        map[string]string{},
		StatePath:          filepath.Join(home, ".local/state/handoffd/state.json"),
		SlackTokenFile:     filepath.Join(home, ".config/handoffd/slack_token"),
		SlackCookieFile:    filepath.Join(home, ".config/handoffd/slack_cookie"),
		ClaudeBin:          Binary("claude", filepath.Join(home, ".local/bin/claude")),
		CodexBin:           Binary("codex", filepath.Join(home, ".local/bin/codex")),
		GlabBin:            Binary("glab", "/opt/homebrew/bin/glab", "/usr/local/bin/glab"),
		GhBin:              Binary("gh", "/opt/homebrew/bin/gh", "/usr/local/bin/gh", "/usr/bin/gh"),
		TmuxBin:            Binary("tmux", "/opt/homebrew/bin/tmux", "/usr/local/bin/tmux", "/usr/bin/tmux"),
		WezTermBin:         Binary("wezterm", "/Applications/WezTerm.app/Contents/MacOS/wezterm", "/usr/bin/wezterm"),
		HerdrBin:           Binary("herdr", "/opt/homebrew/bin/herdr", "/usr/local/bin/herdr"),
		CmuxBin:            Binary("cmux", "/opt/homebrew/bin/cmux", "/usr/local/bin/cmux"),
		CmuxPasswordFile:   filepath.Join(home, ".config/handoffd/cmux_password"),
		GitBin:             Binary("git", "/usr/bin/git"),
		TriageTimeoutSec:   150,
		TriageLimit:        12,
		TriageCooldownN:    3,
		TriageCooldownMin:  60,
		TriageExamples:     6,
		QueueSize:          64,
		OtherLimit:         5,
		OtherWindowSeconds: 600,
		WorktreeTTLDays:    14,
		ThreadTTLDays:      30,
		GoneThreadTTLDays:  7,
		LastReadMaxAgeSec:  120,
		DeliverTimeoutSec:  900,
		StaleHours:         6,
		ThreadPollSec:      30,
		ThreadPollHours:    72,
		ThreadPollLimit:    10,
		AlertCommand:       alert.DefaultCommand(runtime.GOOS),
		AlertCooldownMin:   60,
		BlockedAlertMin:    15,
		SlackLogStaleHours: 2,
		WatchdogSec:        60,
		CommitteeMarker:    "[committee]",
		AgentModels:        map[string]string{},
		SeenCapacity:       500,
	}
}

func SlackLogPaths(goos, home string) []string {
	switch goos {
	case "darwin":
		return []string{
			filepath.Join(home, "Library/Containers/com.tinyspeck.slackmacgap/Data/Library",
				"Application Support/Slack/logs/default/webapp-console.log"),
			filepath.Join(home, "Library/Application Support/Slack/logs/default/webapp-console.log"),
		}
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return []string{filepath.Join(appData, "Slack", "logs", "default", "webapp-console.log")}
		}
		return []string{filepath.Join(home, "AppData", "Roaming", "Slack", "logs", "default", "webapp-console.log")}
	default:
		return []string{filepath.Join(home, ".config/Slack/logs/default/webapp-console.log")}
	}
}

func SlackLogPath(goos, home string) string {
	paths := SlackLogPaths(goos, home)
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return paths[0]
}

func (c Config) TerminalBin() string {
	switch c.Terminal {
	case "wezterm":
		return c.WezTermBin
	case "herdr":
		return c.HerdrBin
	case "cmux":
		return c.CmuxBin
	default:
		return c.TmuxBin
	}
}

func (c Config) TerminalSession() string {
	if c.Terminal == "herdr" {
		return c.HerdrSession
	}
	return c.TmuxSession
}

func Binary(name string, fallbacks ...string) string {
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	for _, candidate := range fallbacks {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if len(fallbacks) > 0 {
		return fallbacks[0]
	}
	return name
}

func Load(path string) (Config, error) {
	cfg, err := Read(path)
	if err != nil {
		return cfg, err
	}
	return cfg, cfg.Validate()
}

func Read(path string) (Config, error) {
	cfg := Defaults()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	return cfg, nil
}

func (c Config) Validate() error {
	switch {
	case c.LogPath == "":
		return errors.New("log_path is empty")
	case c.Channel == "":
		return errors.New("channel is empty: run `handoffd init`")
	case c.SelfUserID == "":
		return errors.New("self_user_id is empty: run `handoffd init`")
	case c.SlackWorkspace == "":
		return errors.New("slack_workspace is empty: run `handoffd init`")
	case !launcher.Supported(c.Terminal):
		return fmt.Errorf("terminal %q is not supported (%s)", c.Terminal, strings.Join(launcher.Names(), ", "))
	case launcher.NeedsSession(c.Terminal) && c.TmuxSession == "":
		return errors.New("tmux_session is empty")
	case c.WorktreeDir == "" || filepath.IsAbs(c.WorktreeDir):
		return errors.New("worktree_dir must be a path relative to the repository")
	case c.ProjectsDir == "":
		return errors.New("projects_dir is empty")
	case c.StatePath == "":
		return errors.New("state_path is empty")
	case len(c.FetchHelper) == 0 && c.SlackTokenFile == "":
		return errors.New("neither fetch_helper nor slack_token_file is set: run `handoffd init`")
	case c.OtherLimit <= 0:
		return errors.New("other_limit must be positive")
	case c.OtherWindowSeconds <= 0:
		return errors.New("other_window_seconds must be positive")
	case c.SeenCapacity <= 0:
		return errors.New("seen_capacity must be positive")
	case c.Agent != "" && c.Agent != "claude" && c.Agent != "codex":
		return fmt.Errorf("agent %q is not supported (claude, codex)", c.Agent)
	case len(c.CommitteeAgents) == 1:
		return errors.New("committee_agents needs at least two agents or none")
	case c.TriageTimeoutSec <= 0:
		return errors.New("triage_timeout_seconds must be positive")
	case c.TriageLimit <= 0:
		return errors.New("triage_limit must be positive")
	case c.QueueSize <= 0:
		return errors.New("queue_size must be positive")
	case c.ThreadTTLDays <= 0:
		return errors.New("thread_ttl_days must be positive")
	case c.GoneThreadTTLDays <= 0:
		return errors.New("gone_thread_ttl_days must be positive")
	case c.DeliverTimeoutSec <= 0:
		return errors.New("deliver_timeout_seconds must be positive")
	case c.Persona.Owner == "" || c.Persona.Agent == "":
		return errors.New("persona.owner and persona.agent are required: run `handoffd init`")
	case c.PromptsLanguage != "en":
		return fmt.Errorf("prompts_language %q is not supported (en)", c.PromptsLanguage)
	}
	return nil
}

func (c Config) Rules() mention.Rules {
	rules := mention.Rules{
		MentionWords:    c.MentionWords,
		TriageWords:     c.TriageWords,
		MacroPhrases:    c.MacroPhrases,
		StopPhrases:     c.StopPhrases,
		AckWords:        c.AckWords,
		ProgressMarkers: c.ProgressMarkers,
	}
	if c.ReviewEnabled {
		rules.ReviewMarkers = c.ReviewMarkers
		rules.ReviewHeadMarkers = c.ReviewHeadMarkers
		rules.ReviewWords = c.ReviewWords
	}
	return rules
}

func (c Config) Label() string {
	if c.PostLabel != "" {
		return c.PostLabel
	}
	if c.Persona.Agent == "" {
		return ""
	}
	return "🤖 " + c.Persona.Agent + ": "
}

func (c Config) ModelFor(agent string) string {
	if model, ok := c.AgentModels[agent]; ok {
		return model
	}
	if agent == "" || agent == c.Agent {
		return c.MainModel
	}
	return ""
}

func (c Config) NamesakeName() string {
	return c.Namesake
}

func (c Config) TriageToolList() []string {
	if len(c.HaikuTools) > 0 {
		return c.HaikuTools
	}
	return c.TriageTools
}

func (c Config) TriageModelName() string {
	if c.TriageModel != "" {
		return c.TriageModel
	}
	return c.HaikuModel
}

func (c Config) OtherWindow() time.Duration {
	return time.Duration(c.OtherWindowSeconds) * time.Second
}

func (c Config) TriageCooldownWindow() time.Duration {
	return time.Duration(c.TriageCooldownMin) * time.Minute
}

func (c Config) TriageTimeout() time.Duration {
	return time.Duration(c.TriageTimeoutSec) * time.Second
}

func (c Config) WorktreeTTL() time.Duration {
	return time.Duration(c.WorktreeTTLDays) * 24 * time.Hour
}

func (c Config) ThreadTTL() time.Duration {
	return time.Duration(c.ThreadTTLDays) * 24 * time.Hour
}

func (c Config) GoneThreadTTL() time.Duration {
	return time.Duration(c.GoneThreadTTLDays) * 24 * time.Hour
}

func (c Config) LastReadMaxAge() time.Duration {
	return time.Duration(c.LastReadMaxAgeSec) * time.Second
}

func (c Config) ThreadPollInterval() time.Duration {
	return time.Duration(c.ThreadPollSec) * time.Second
}

func (c Config) ThreadPollWindow() time.Duration {
	return time.Duration(c.ThreadPollHours) * time.Hour
}

func (c Config) AlertCooldown() time.Duration {
	return time.Duration(c.AlertCooldownMin) * time.Minute
}

func (c Config) BlockedAlertAfter() time.Duration {
	return time.Duration(c.BlockedAlertMin) * time.Minute
}

func (c Config) SlackLogStaleAfter() time.Duration {
	return time.Duration(c.SlackLogStaleHours) * time.Hour
}

func (c Config) WatchdogInterval() time.Duration {
	return time.Duration(c.WatchdogSec) * time.Second
}

func (c Config) StaleAfter() time.Duration {
	return time.Duration(c.StaleHours) * time.Hour
}

func (c Config) DeliverTimeout() time.Duration {
	return time.Duration(c.DeliverTimeoutSec) * time.Second
}

func (c Config) RepoDir(mr mrref.MR) string {
	dir := mr.Project
	for _, key := range []string{mr.Host + "/" + mr.Path(), mr.Path(), mr.Project} {
		if alias, ok := c.RepoAliases[key]; ok {
			dir = alias
			break
		}
	}
	if filepath.IsAbs(dir) {
		return dir
	}
	return filepath.Join(c.ProjectsDir, dir)
}

func (c Config) Allows(mr mrref.MR) bool {
	hosts := c.GitLabHosts
	if mr.IsGitHub() {
		hosts = c.GitHubHosts
	}
	if len(hosts) == 0 {
		return true
	}
	for _, allowed := range hosts {
		if strings.EqualFold(allowed, mr.Host) {
			return true
		}
	}
	return false
}

func (c Config) HostsKey(mr mrref.MR) string {
	if mr.IsGitHub() {
		return "github_hosts"
	}
	return "gitlab_hosts"
}
