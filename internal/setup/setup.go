package setup

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kriso1337/handoffd/internal/config"
	"github.com/Kriso1337/handoffd/internal/launcher"
	"github.com/Kriso1337/handoffd/internal/mention"
)

const (
	SlackModeToken  = "token"
	SlackModeHelper = "helper"

	verifyAttempts = 3
)

type Identity struct {
	UserID string
	User   string
	Team   string
	URL    string
}

type Environment struct {
	ReadSecret       func(prompt string) (string, error)
	Verify           func(token, cookie string) (Identity, error)
	FindChannel      func(token, cookie, name string) (string, error)
	HelperSuggestion []string
	Terminals        []string
	Alerts           bool
	TokenScopes      string
}

type Answers struct {
	SlackMode         string
	Token             string
	Cookie            string
	FetchHelper       []string
	SelfUserID        string
	Workspace         string
	Channel           string
	ChannelName       string
	Agent             string
	Owner             string
	OwnerGenitive     string
	OwnerDative       string
	OwnerFull         string
	OwnerFullGenitive string
	Approvers         string
	Capabilities      string
	Namesake          string
	PeerAgents        []string
	MentionWords      []string
	TriageWords       []string
	MacroPhrases      []string
	StopPhrases       []string
	MainModel         string
	TriageModel       string
	TriageTools       []string
	Terminal          string
	TmuxSession       string
	ProjectsDir       string
	GitLabHosts       []string
	GitHubHosts       []string
	ReviewEnabled     bool
	Alerts            bool
}

type wizard struct {
	in       *bufio.Reader
	out      io.Writer
	existing map[string]any
	env      Environment
}

func Existing(path string) (map[string]any, error) {
	doc := map[string]any{}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return doc, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	return doc, nil
}

func Ask(in io.Reader, out io.Writer, existing map[string]any, env Environment) (Answers, error) {
	reader, ok := in.(*bufio.Reader)
	if !ok {
		reader = bufio.NewReader(in)
	}
	if existing == nil {
		existing = map[string]any{}
	}
	w := &wizard{in: reader, out: out, existing: existing, env: env}
	if w.env.ReadSecret == nil {
		w.env.ReadSecret = func(prompt string) (string, error) {
			fmt.Fprint(w.out, prompt)
			return w.line()
		}
	}
	var a Answers
	steps := []func(*Answers) error{w.slack, w.persona, w.words, w.models, w.terminal, w.repositories, w.review, w.notifications}
	for _, step := range steps {
		if err := step(&a); err != nil {
			return Answers{}, err
		}
	}
	return a, nil
}

func (w *wizard) section(title string) {
	fmt.Fprintf(w.out, "\n— %s\n", title)
}

func (w *wizard) line() (string, error) {
	text, err := w.in.ReadString('\n')
	if err != nil && text == "" && err != io.EOF {
		return "", err
	}
	return strings.TrimSpace(text), nil
}

func (w *wizard) ask(prompt, fallback string) string {
	if fallback != "" {
		fmt.Fprintf(w.out, "%s [%s]: ", prompt, fallback)
	} else {
		fmt.Fprintf(w.out, "%s: ", prompt)
	}
	text, _ := w.line()
	if text == "" {
		return fallback
	}
	return text
}

func (w *wizard) askList(prompt string, fallback []string) []string {
	return splitList(w.ask(prompt, strings.Join(fallback, ", ")))
}

func (w *wizard) askPhrases(prompt string, fallback []string) []string {
	return splitPhrases(w.ask(prompt+" (separate with ;)", strings.Join(fallback, " ; ")))
}

func (w *wizard) askYesNo(prompt string, fallback bool) bool {
	def := "no"
	if fallback {
		def = "yes"
	}
	answer := strings.ToLower(w.ask(prompt+" (yes/no)", def))
	return answer == "y" || answer == "yes"
}

func (w *wizard) str(key, fallback string) string {
	return str(w.existing[key], fallback)
}

func (w *wizard) list(key string) []string {
	return listOf(w.existing[key])
}

func (w *wizard) existingPersona() map[string]any {
	p, _ := w.existing["persona"].(map[string]any)
	if p == nil {
		p = map[string]any{}
	}
	return p
}

func (w *wizard) slack(a *Answers) error {
	w.section("Slack access")
	fmt.Fprintln(w.out, "  1) built-in client: a Slack token stored in ~/.config/handoffd (verified now)")
	fmt.Fprintln(w.out, "  2) external helper command that prints the message JSON (see README for the contract)")
	defaultMode := "1"
	if len(w.list("fetch_helper")) > 0 {
		defaultMode = "2"
	}
	switch w.ask("Slack access", defaultMode) {
	case "1", SlackModeToken:
		a.SlackMode = SlackModeToken
		if err := w.token(a); err != nil {
			return err
		}
	case "2", SlackModeHelper:
		a.SlackMode = SlackModeHelper
		fallback := w.list("fetch_helper")
		if len(fallback) == 0 {
			fallback = w.env.HelperSuggestion
		}
		a.FetchHelper = strings.Fields(w.ask("Slack helper command", strings.Join(fallback, " ")))
		if len(a.FetchHelper) == 0 {
			return errors.New("fetch_helper is required for the helper mode")
		}
	default:
		return errors.New("slack access: answer 1 or 2")
	}

	a.SelfUserID = w.ask("Owner's Slack user id (U…)", w.str("self_user_id", a.SelfUserID))
	if a.SelfUserID == "" {
		return errors.New("self_user_id is required")
	}
	a.Workspace = w.ask("Slack workspace (the part before .slack.com)", w.str("slack_workspace", a.Workspace))
	if a.Workspace == "" {
		return errors.New("slack_workspace is required")
	}
	a.ChannelName = strings.TrimPrefix(w.ask("Home channel name without #", w.str("channel_name", "")), "#")
	if a.ChannelName == "" {
		return errors.New("channel_name is required")
	}
	channelFallback := w.str("channel", "")
	if channelFallback == "" && a.Token != "" && w.env.FindChannel != nil {
		if id, err := w.env.FindChannel(a.Token, a.Cookie, a.ChannelName); err == nil && id != "" {
			channelFallback = id
			fmt.Fprintf(w.out, "  resolved #%s → %s\n", a.ChannelName, id)
		} else if err != nil {
			fmt.Fprintf(w.out, "  channel lookup failed: %v\n", err)
		}
	}
	a.Channel = w.ask("Home channel id (C…)", channelFallback)
	if a.Channel == "" {
		return errors.New("channel is required")
	}
	return nil
}

func (w *wizard) token(a *Answers) error {
	scopes := w.env.TokenScopes
	if scopes == "" {
		scopes = "channels:history, groups:history, im:history, mpim:history, channels:read, groups:read, im:read, mpim:read, users:read, chat:write"
	}
	fmt.Fprintf(w.out, "  Where to get it: a user token xoxp-… from a Slack app with scopes %s,\n", scopes)
	fmt.Fprintln(w.out, "  or a browser token xoxc-… (DevTools → Application → Local Storage → localConfig_v2) plus the cookie d (xoxd-…).")
	fmt.Fprintln(w.out, "  Enter keeps the token already stored, if any. Input is hidden.")
	for attempt := 1; attempt <= verifyAttempts; attempt++ {
		token, err := w.env.ReadSecret("  Slack token: ")
		if err != nil {
			return err
		}
		fmt.Fprintln(w.out)
		if token == "" {
			fmt.Fprintln(w.out, "  keeping the stored token")
			return nil
		}
		cookie := ""
		if strings.HasPrefix(token, "xoxc-") {
			if cookie, err = w.env.ReadSecret("  Browser cookie d (xoxd-…): "); err != nil {
				return err
			}
			fmt.Fprintln(w.out)
		}
		if w.env.Verify == nil {
			a.Token, a.Cookie = token, cookie
			return nil
		}
		id, err := w.env.Verify(token, cookie)
		if err != nil {
			fmt.Fprintf(w.out, "  ✗ %v\n", err)
			continue
		}
		fmt.Fprintf(w.out, "  ✓ authenticated as %s (%s) in %s\n", id.User, id.UserID, id.Team)
		a.Token, a.Cookie = token, cookie
		a.SelfUserID, a.Workspace = id.UserID, workspaceFromURL(id.URL)
		return nil
	}
	return fmt.Errorf("slack token was not accepted after %d attempts", verifyAttempts)
}

func workspaceFromURL(raw string) string {
	raw = strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://")
	host, _, _ := strings.Cut(raw, "/")
	return strings.TrimSuffix(host, ".slack.com")
}

func (w *wizard) persona(a *Answers) error {
	w.section("Persona")
	p := w.existingPersona()
	a.Agent = w.ask("Agent name (how people address it in Slack)", str(p["agent"], ""))
	if a.Agent == "" {
		return errors.New("agent name is required")
	}
	a.Owner = w.ask("Owner name (how people address them in Slack)", str(p["owner"], ""))
	if a.Owner == "" {
		return errors.New("owner name is required")
	}
	a.OwnerGenitive, a.OwnerDative = a.Owner, a.Owner
	a.OwnerFull = w.ask("Owner full name", str(p["owner_full"], a.Owner))
	a.OwnerFullGenitive = a.OwnerFull
	a.Approvers = w.ask("Who gives [GO] (as it should read after \"from\")", str(p["approvers"], a.Owner))
	a.Capabilities = w.ask("Data sources the agent can reach, comma-separated (shown to triage; e.g. Jira, GitLab, Grafana)", str(p["capabilities"], ""))
	a.PeerAgents = w.askList("Colleagues' agents, comma-separated", w.list("peer_agents"))
	a.Namesake = w.ask("Namesake — another person with the owner's name, e.g. \"Alex Smith (U02…)\" (empty = none)", w.str("namesake", ""))
	return nil
}

func (w *wizard) words(a *Answers) error {
	w.section("Trigger words (Enter keeps the derived defaults)")
	agent, owner := strings.ToLower(a.Agent), strings.ToLower(a.Owner)
	mention := w.list("mention_words")
	if len(mention) == 0 {
		mention = []string{agent}
	}
	triage := w.list("triage_words")
	if len(triage) == 0 {
		triage = []string{agent, owner}
	}
	macro, stop := derivedPhrases(agent)
	if existing := w.list("macro_phrases"); len(existing) > 0 {
		macro = existing
	}
	if existing := w.list("stop_phrases"); len(existing) > 0 {
		stop = existing
	}
	a.MentionWords = w.askList("Words that count as a mention in the home channel", mention)
	a.TriageWords = w.askList("Names that send other chats to triage", triage)
	a.MacroPhrases = w.askPhrases("Macro phrases that wake the agent immediately", macro)
	a.StopPhrases = w.askPhrases("Stop phrases that close the thread window", stop)
	if len(a.MentionWords) == 0 || len(a.StopPhrases) == 0 {
		return errors.New("mention words and stop phrases must not be empty")
	}
	return nil
}

func derivedPhrases(agent string) (macro, stop []string) {
	return []string{agent + ", help", agent + " help"}, []string{agent + ", stop", agent + " stop"}
}

func (w *wizard) models(a *Answers) error {
	w.section("Models and triage")
	a.MainModel = w.ask("Main model for agent windows (empty = agent default)", w.str("main_model", ""))
	a.TriageModel = w.ask("Triage model (cheap, decides whether to wake the agent)", w.str("triage_model", w.str("haiku_model", "claude-haiku-4-5-20251001")))
	tools := w.list("triage_tools")
	if len(tools) == 0 {
		tools = w.list("haiku_tools")
	}
	tools = without(tools, "Write")
	a.TriageTools = w.askList("Triage MCP tools for reading the thread, comma-separated (Write is added)", tools)
	return nil
}

func (w *wizard) terminal(a *Answers) error {
	w.section("Terminal")
	fallback := w.str("terminal", "")
	if fallback == "" && len(w.env.Terminals) > 0 {
		fallback = w.env.Terminals[0]
	}
	if fallback == "" {
		fallback = "tmux"
	}
	hint := strings.Join(launcher.Names(), ", ")
	if len(w.env.Terminals) > 0 {
		hint = "installed: " + strings.Join(w.env.Terminals, ", ")
	}
	a.Terminal = strings.ToLower(w.ask("Terminal ("+hint+")", fallback))
	if !launcher.Supported(a.Terminal) {
		return fmt.Errorf("terminal %q is not supported (%s)", a.Terminal, strings.Join(launcher.Names(), ", "))
	}
	if launcher.NeedsSession(a.Terminal) {
		a.TmuxSession = w.ask("tmux session that receives the windows", w.str("tmux_session", "main"))
	}
	return nil
}

func (w *wizard) repositories(a *Answers) error {
	w.section("Repositories")
	a.ProjectsDir = w.ask("Directory with repositories", w.str("projects_dir", ""))
	a.GitLabHosts = w.askList("Allowed GitLab hosts, comma-separated (empty = any)", w.list("gitlab_hosts"))
	a.GitHubHosts = w.askList("Allowed GitHub hosts, comma-separated (empty = any)", w.list("github_hosts"))
	return nil
}

func (w *wizard) review(a *Answers) error {
	w.section("Code review")
	enabled, _ := w.existing["review_enabled"].(bool)
	a.ReviewEnabled = w.askYesNo("Enable automatic MR/PR review workflows", enabled)
	return nil
}

func (w *wizard) notifications(a *Answers) error {
	w.section("Notifications")
	fallback := w.env.Alerts
	if existing, ok := w.existing["alert_command"].([]any); ok {
		fallback = len(existing) > 0
	}
	a.Alerts = w.askYesNo("Desktop notifications on dead letters, stuck sessions and a silent Slack log", fallback)
	return nil
}

func (a Answers) Config() map[string]any {
	cfg := map[string]any{
		"prompts_language": "en",
		"self_user_id":     a.SelfUserID,
		"slack_workspace":  a.Workspace,
		"channel":          a.Channel,
		"channel_name":     a.ChannelName,
		"mention_words":    a.MentionWords,
		"triage_words":     a.TriageWords,
		"macro_phrases":    a.MacroPhrases,
		"stop_phrases":     a.StopPhrases,
		"review_enabled":   a.ReviewEnabled,
		"persona": map[string]any{
			"owner":               a.Owner,
			"owner_genitive":      firstNonEmpty(a.OwnerGenitive, a.Owner),
			"owner_dative":        firstNonEmpty(a.OwnerDative, a.Owner),
			"owner_full":          firstNonEmpty(a.OwnerFull, a.Owner),
			"owner_full_genitive": firstNonEmpty(a.OwnerFullGenitive, a.OwnerFull, a.Owner),
			"agent":               a.Agent,
			"approvers":           firstNonEmpty(a.Approvers, a.Owner),
			"capabilities":        a.Capabilities,
		},
		"namesake":     a.Namesake,
		"peer_agents":  a.PeerAgents,
		"gitlab_hosts": a.GitLabHosts,
		"github_hosts": a.GitHubHosts,
		"main_model":   a.MainModel,
		"triage_model": a.TriageModel,
		"fetch_helper": a.FetchHelper,
	}
	if a.SlackMode == SlackModeToken {
		cfg["fetch_helper"] = []string{}
	}
	if len(a.MentionWords) == 0 {
		cfg["mention_words"] = []string{strings.ToLower(a.Agent)}
	}
	if len(a.TriageWords) == 0 {
		cfg["triage_words"] = []string{strings.ToLower(a.Agent), strings.ToLower(a.Owner)}
	}
	macro, stop := derivedPhrases(strings.ToLower(a.Agent))
	if len(a.MacroPhrases) == 0 {
		cfg["macro_phrases"] = macro
	}
	if len(a.StopPhrases) == 0 {
		cfg["stop_phrases"] = stop
	}
	cfg["review_words"] = mention.ReviewWordsEN
	cfg["ack_words"] = mention.AckWordsEN
	cfg["triage_tools"] = append([]string{"Write"}, without(a.TriageTools, "Write")...)
	if len(a.TriageTools) > 0 {
		cfg["review_allowed_tools"] = append(append([]string{}, config.ReviewerAllowedTools...), without(a.TriageTools, "Write")...)
	}
	if a.Terminal != "" {
		cfg["terminal"] = a.Terminal
	}
	if a.TmuxSession != "" {
		cfg["tmux_session"] = a.TmuxSession
	}
	if a.ProjectsDir != "" {
		cfg["projects_dir"] = a.ProjectsDir
	}
	if !a.Alerts {
		cfg["alert_command"] = []string{}
	}
	for _, key := range []string{"peer_agents", "gitlab_hosts", "github_hosts"} {
		if list, _ := cfg[key].([]string); len(list) == 0 {
			delete(cfg, key)
		}
	}
	for _, key := range []string{"main_model", "namesake"} {
		if cfg[key] == "" {
			delete(cfg, key)
		}
	}
	return cfg
}

func WriteSecret(path, value string) error {
	if value == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return os.Chmod(path, 0o600)
}

func Merge(path string, values map[string]any) error {
	doc, err := Existing(path)
	if err != nil {
		return err
	}
	for k, v := range values {
		doc[k] = v
	}
	delete(doc, "haiku_tools")
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		return fmt.Errorf("write config %s: %w", path, err)
	}
	return nil
}

func str(v any, fallback string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return fallback
}

func listOf(v any) []string {
	switch items := v.(type) {
	case []any:
		out := make([]string, 0, len(items))
		for _, item := range items {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return items
	}
	return nil
}

func splitList(value string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ';' }) {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func splitPhrases(value string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(value, func(r rune) bool { return r == ';' || r == '|' }) {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func without(items []string, skip string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if item != skip {
			out = append(out, item)
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
