package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Kriso1337/handoffd/internal/agent"
	"github.com/Kriso1337/handoffd/internal/alert"
	"github.com/Kriso1337/handoffd/internal/config"
	"github.com/Kriso1337/handoffd/internal/deadletter"
	"github.com/Kriso1337/handoffd/internal/forge"
	"github.com/Kriso1337/handoffd/internal/journal"
	"github.com/Kriso1337/handoffd/internal/label"
	"github.com/Kriso1337/handoffd/internal/launcher"
	"github.com/Kriso1337/handoffd/internal/linkq"
	"github.com/Kriso1337/handoffd/internal/mention"
	"github.com/Kriso1337/handoffd/internal/notifylog"
	"github.com/Kriso1337/handoffd/internal/outbox"
	"github.com/Kriso1337/handoffd/internal/pipeline"
	"github.com/Kriso1337/handoffd/internal/prompt"
	"github.com/Kriso1337/handoffd/internal/promptfile"
	"github.com/Kriso1337/handoffd/internal/session"
	"github.com/Kriso1337/handoffd/internal/slackapi"
	"github.com/Kriso1337/handoffd/internal/slackfetch"
	"github.com/Kriso1337/handoffd/internal/state"
	"github.com/Kriso1337/handoffd/internal/tailer"
	"github.com/Kriso1337/handoffd/internal/threadpoll"
	"github.com/Kriso1337/handoffd/internal/triage"
	"github.com/Kriso1337/handoffd/internal/waitq"
	"github.com/Kriso1337/handoffd/internal/watchdog"
	"github.com/Kriso1337/handoffd/internal/worktree"
)

const (
	pollInterval        = 300 * time.Millisecond
	verdictPollInterval = time.Second
	sessionPollInterval = 2 * time.Second
	handleRetryDelay    = 3 * time.Second
	outboxPollInterval  = time.Second
	linkPollInterval    = time.Second
	housekeepingPeriod  = time.Hour
)

func main() {
	configPath := flag.String("config", defaultConfigPath(), "path to config file")
	flag.Parse()

	err := run(flag.Args(), *configPath)
	switch {
	case err == nil, errors.Is(err, context.Canceled):
	case errors.Is(err, errPostPending), errors.Is(err, errLinkPending):
		os.Exit(exitPending)
	default:
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitRejected)
	}
}

func run(args []string, configPath string) error {
	command := "watch"
	if len(args) > 0 {
		command = args[0]
	}

	cfg, err := config.Read(configPath)
	if err != nil {
		return err
	}
	switch command {
	case "init":
		return initialize(configPath, bufio.NewReader(os.Stdin))
	case "setup-agent":
		return setupAgent(cfg, configPath, args[1:], bufio.NewReader(os.Stdin))
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	switch command {
	case "watch":
		return watch(cfg, configPath)
	case "run-agent":
		if len(args) < 2 {
			return errors.New("run-agent requires a prompt file")
		}
		return runAgent(cfg, args[1])
	case "hook":
		return hook(cfg)
	case "prune":
		return prune(cfg, hasFlag(args[1:], "-n", "--dry-run"))
	case "decisions":
		return decisions(cfg, args[1:])
	case "replay":
		return replay(cfg)
	case "status":
		return showStatus(cfg, configPath, args[1:])
	case "fetch":
		return fetchMessage(cfg, args[1:])
	case "terminal-check":
		return terminalCheck(cfg, configPath)
	case "label":
		return labelDecision(cfg, args[1:])
	case "post":
		return post(cfg, configPath, args[1:], os.Stdout)
	case "link":
		return link(cfg, configPath, args[1:], os.Stdout)
	case "eval":
		return evaluate(cfg, args[1:])
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}

func watch(cfg config.Config, configPath string) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	detected := detectAgents(cfg)
	runner, err := agent.Resolve(cfg, detected)
	if err != nil {
		return err
	}
	claim, err := claimState(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = claim.Release() }()
	store, err := state.Open(cfg.StatePath, cfg.SeenCapacity)
	if err != nil {
		return err
	}
	render, err := prompt.NewLocalized(cfg.Persona, cfg.PromptsDir, cfg.PromptsLanguage)
	if err != nil {
		return err
	}
	var hookSettings string
	if runner.PerSessionHooks() {
		if hookSettings, err = session.SettingsJSON(selfPath(), configPath); err != nil {
			return err
		}
	}

	stateDir := filepath.Dir(cfg.StatePath)
	forges := forge.New(commander{timeout: forgeCommandTimeout}, binaryPath(cfg.GlabBin, "glab"), binaryPath(cfg.GhBin, "gh"))
	worktrees := worktrees(cfg, forges)
	outboxDir := outbox.New(outboxPath(cfg))
	linkDir := linkq.New(linksPath(cfg))
	waitingDir := waitq.New(filepath.Join(stateDir, "waiting"))
	windows := terminal(cfg, configPath)
	prompts := promptfile.New(filepath.Join(stateDir, "prompts"))
	fetcher, err := slackSource(cfg)
	if err != nil {
		return err
	}
	verdicts := triage.NewWaiter(filepath.Join(stateDir, "verdicts"), cfg.TriageTimeout(), verdictPollInterval)
	if err := os.MkdirAll(filepath.Join(stateDir, "verdicts"), 0o700); err != nil {
		return fmt.Errorf("create verdict dir: %w", err)
	}

	alerts := alert.New(commander{timeout: alertCommandTimeout}, cfg.AlertCommand, cfg.AlertCooldown(), time.Now, log)
	if moved, ok := store.Recovered(); ok {
		log.Warn("state could not be read and was moved aside, starting empty", "quarantined", moved)
		alerts.Alert(context.Background(), "state:"+moved, "handoffd", "state unreadable, moved to "+moved)
	}
	sessions := session.NewStore(sessionDir(cfg))
	pipe := pipeline.New(cfg, pipeline.Deps{
		Alerts:         alerts,
		Labels:         label.New(labelPath(cfg)),
		Agents:         agent.Names(detected),
		Fetcher:        fetcher,
		Classifier:     mention.New(cfg.SelfUserID, cfg.Channel, cfg.Rules()),
		Worktrees:      worktrees,
		Windows:        windows,
		Prompts:        prompts,
		Verdicts:       verdicts,
		Sessions:       sessions,
		Journal:        journal.NewWriter(journalPath(cfg)),
		Inbox:          deadletter.New(inboxPath(cfg)),
		DeadLetters:    deadletter.New(deadLetterPath(cfg)),
		Outbox:         outboxDir,
		Links:          linkDir,
		Waiting:        waitingDir,
		Poster:         fetcher,
		Readback:       fetcher,
		Notes:          forges,
		Render:         render,
		Store:          store,
		Now:            time.Now,
		NewID:          newSessionID,
		Log:            log,
		HookSettings:   hookSettings,
		SelfBin:        selfPath(),
		ConfigPath:     configPath,
		PollInterval:   sessionPollInterval,
		RetryDelay:     handleRetryDelay,
		OutboxInterval: outboxPollInterval,
		LinkInterval:   linkPollInterval,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := writePID(pidPath(cfg)); err != nil {
		log.Warn("pid file not written, `replay` will not reach this process", "error", err)
	}
	defer os.Remove(pidPath(cfg))
	replays := make(chan os.Signal, 1)
	signal.Notify(replays, syscall.SIGUSR1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-replays:
				n, err := pipe.Replay(ctx)
				log.Info("dead letters queued for replay", "count", n, "error", err)
				retried, err := outboxDir.Retry()
				log.Info("pending outbound messages queued for retry", "count", retried, "error", err)
			}
		}
	}()

	housekeeping(ctx, cfg, pipe, worktrees, outboxDir, linkDir, store, log)
	go func() {
		ticker := time.NewTicker(housekeepingPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				housekeeping(ctx, cfg, pipe, worktrees, outboxDir, linkDir, store, log)
			}
		}
	}()

	pipe.ResumeWaiting(ctx)
	go pipe.Work(ctx)
	go pipe.RunOutbox(ctx)
	go pipe.RunLinks(ctx)
	if n := pipe.Recover(ctx); n > 0 {
		log.Info("admitted notifications recovered from the inbox", "count", n)
	}
	go threadpoll.New(store, fetcher, pipe, log, time.Now, cfg.ThreadPollInterval(), cfg.ThreadPollWindow(), cfg.ThreadPollLimit).Run(ctx)
	go watchdog.New(store, windows, sessions, alerts, watchdog.Options{
		LogPath: cfg.LogPath, StaleAfter: cfg.SlackLogStaleAfter(), BlockedAfter: cfg.BlockedAlertAfter(), Interval: cfg.WatchdogInterval(),
		CollectedAfter: cfg.BlockedAlertAfter(),
	}, time.Now, log).Run(ctx)

	log.Info("watching", "log", cfg.LogPath, "home", cfg.Channel, "terminal", cfg.Terminal, "session", cfg.TmuxSession,
		"agent", runner.Kind, "bin", runner.Bin, "thread_poll", cfg.ThreadPollInterval(), "alerts", alerts.Enabled())

	banners := notifylog.NewScanner()
	lastRead := notifylog.NewLastReadScanner(cfg.LastReadMaxAge(), time.Now)
	err = tailFile(ctx, cfg, func(line string) {
		if notification, ok := banners.Feed(line); ok {
			pipe.Enqueue(ctx, notification)
		}
		if notification, ok := lastRead.Feed(line); ok {
			pipe.Enqueue(ctx, notification)
		}
	})
	pipe.Wait()
	return err
}

func housekeeping(
	ctx context.Context,
	cfg config.Config,
	pipe *pipeline.Pipeline,
	worktrees worktree.Preparer,
	outboxDir outbox.Dir,
	linkDir linkq.Dir,
	store *state.Store,
	log *slog.Logger,
) {
	if err := pipe.Sweep(ctx); err != nil {
		log.Warn("thread sweep skipped", "error", err)
	}
	if n, err := outboxDir.Sweep(time.Now(), cfg.GoneThreadTTL()); err != nil {
		log.Warn("outbox sweep incomplete", "error", err)
	} else if n > 0 {
		log.Info("outbox files removed", "count", n)
	}
	if n, err := linkDir.Sweep(time.Now(), cfg.GoneThreadTTL()); err != nil {
		log.Warn("link request sweep incomplete", "error", err)
	} else if n > 0 {
		log.Info("link request files removed", "count", n)
	}
	pruneStale(ctx, cfg, worktrees, store, log, false)
}

func labelPath(cfg config.Config) string {
	return filepath.Join(filepath.Dir(cfg.StatePath), "labels.jsonl")
}

func journalPath(cfg config.Config) string {
	return filepath.Join(filepath.Dir(cfg.StatePath), "decisions.jsonl")
}

func deadLetterPath(cfg config.Config) string {
	return filepath.Join(filepath.Dir(cfg.StatePath), "deadletter.jsonl")
}

func inboxPath(cfg config.Config) string {
	return filepath.Join(filepath.Dir(cfg.StatePath), "inbox.jsonl")
}

func pidPath(cfg config.Config) string {
	return filepath.Join(filepath.Dir(cfg.StatePath), "watch.pid")
}

func writePID(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600)
}

const (
	checkBanner   = "handoffd terminal check: window is alive"
	checkEchoMark = "terminal check received: "
	checkTimeout  = 15 * time.Second

	terminalCommandTimeout = 15 * time.Second
	fetchCommandTimeout    = 60 * time.Second
	forgeCommandTimeout    = 60 * time.Second
	alertCommandTimeout    = 30 * time.Second
	gitCommandTimeout      = 180 * time.Second
	checkPoll              = 300 * time.Millisecond
)

const cmuxPasswordEnv = "CMUX_SOCKET_PASSWORD"

func cmuxPassword(cfg config.Config) []string {
	if strings.TrimSpace(os.Getenv(cmuxPasswordEnv)) != "" {
		return nil
	}
	raw, err := os.ReadFile(cfg.CmuxPasswordFile)
	if err != nil {
		return nil
	}
	password := strings.TrimSpace(string(raw))
	if password == "" {
		return nil
	}
	return append(os.Environ(), cmuxPasswordEnv+"="+password)
}

func claimState(cfg config.Config) (*state.Claim, error) {
	claim, ok, err := state.Acquire(claimPath(cfg))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("another handoffd is writing %s", cfg.StatePath)
	}
	return claim, nil
}

func worktrees(cfg config.Config, mrs worktree.MergeRequests) worktree.Preparer {
	git := gitRunner{bin: cfg.GitBin, timeout: gitCommandTimeout}
	return worktree.New(git, cfg.RepoDir, mrs).WithDir(cfg.WorktreeDir)
}

func terminal(cfg config.Config, configPath string) launcher.Backend {
	cmd := commander{timeout: terminalCommandTimeout}
	if cfg.Terminal == "cmux" {
		cmd.env = cmuxPassword(cfg)
	}
	backend, err := launcher.Open(cfg.Terminal, launcher.Deps{
		Commander: cmd, Bin: cfg.TerminalBin(), Session: cfg.TerminalSession(),
		Self: selfPath(), ConfigPath: configPath,
	})
	if err != nil {
		return launcher.New(cmd, cfg.TmuxBin, cfg.TmuxSession, selfPath(), configPath)
	}
	return backend
}

func detectAgents(cfg config.Config) []agent.Installed {
	return agent.Detect(cfg, exec.LookPath, func(path string) error {
		_, err := os.Stat(path)
		return err
	})
}

func binaryPath(configured, name string) string {
	if _, err := os.Stat(configured); err == nil {
		return configured
	}
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	return ""
}

func tailFile(ctx context.Context, cfg config.Config, onLine func(string)) error {
	return tailer.New(cfg.LogPath, pollInterval).Run(ctx, onLine)
}

type commander struct {
	env     []string
	timeout time.Duration
}

func (c commander) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := deadline(ctx, c.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if c.env != nil {
		cmd.Env = c.env
	}
	return cmd.CombinedOutput()
}

type gitRunner struct {
	bin     string
	timeout time.Duration
}

func (g gitRunner) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	ctx, cancel := deadline(ctx, g.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, g.bin, append([]string{"-C", dir}, args...)...)
	return cmd.CombinedOutput()
}

func deadline(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

type slackReader interface {
	pipeline.Fetcher
	pipeline.Poster
	threadpoll.Lister
}

func slackSource(cfg config.Config) (slackReader, error) {
	if len(cfg.FetchHelper) > 0 {
		return slackfetch.New(commander{env: helperEnv(cfg), timeout: fetchCommandTimeout}, cfg.FetchHelper), nil
	}
	token, err := slackapi.LoadToken(cfg.SlackTokenFile, slackapi.EnvToken)
	if err != nil {
		return nil, fmt.Errorf("fetch_helper is empty and the built-in Slack client has no token: %w", err)
	}
	return slackapi.New(token, cfg.SelfUserID).WithCookie(slackapi.LoadCookie(cfg.SlackCookieFile, slackapi.EnvCookie)), nil
}

func helperEnv(cfg config.Config) []string {
	env := append(os.Environ(), "HANDOFFD_SELF_USER_ID="+cfg.SelfUserID)
	for key, value := range cfg.HelperEnv {
		env = append(env, key+"="+value)
	}
	return env
}

func sessionDir(cfg config.Config) string {
	return filepath.Join(filepath.Dir(cfg.StatePath), "sessions")
}

func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func hasFlag(args []string, names ...string) bool {
	for _, arg := range args {
		for _, name := range names {
			if arg == name {
				return true
			}
		}
	}
	return false
}

func selfPath() string {
	path, err := os.Executable()
	if err != nil {
		return "handoffd"
	}
	return path
}

func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.json"
	}
	return filepath.Join(home, ".config", "handoffd", "config.json")
}
