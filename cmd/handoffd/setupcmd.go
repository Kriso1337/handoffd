package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kriso1337/handoffd/internal/agent"
	"github.com/Kriso1337/handoffd/internal/config"
	"github.com/Kriso1337/handoffd/internal/session"
	"github.com/Kriso1337/handoffd/internal/setup"
	"github.com/Kriso1337/handoffd/internal/slackapi"
)

func initialize(configPath string, in io.Reader) error {
	existing, err := setup.Existing(configPath)
	if err != nil {
		return err
	}
	defaults, err := config.Read(configPath)
	if err != nil {
		return err
	}
	reader, ok := in.(*bufio.Reader)
	if !ok {
		reader = bufio.NewReader(in)
	}
	fmt.Printf("handoffd init — config %s (Enter keeps the value in brackets)\n", configPath)
	answers, err := setup.Ask(reader, os.Stdout, existing, setupEnvironment(defaults, reader))
	if err != nil {
		return err
	}
	if err := setup.Merge(configPath, answers.Config()); err != nil {
		return err
	}
	fmt.Printf("config written: %s\n", configPath)
	if answers.SlackMode == setup.SlackModeToken {
		if err := setup.WriteSecret(defaults.SlackTokenFile, answers.Token); err != nil {
			return err
		}
		if err := setup.WriteSecret(defaults.SlackCookieFile, answers.Cookie); err != nil {
			return err
		}
		if answers.Token != "" {
			fmt.Printf("slack token stored in %s (mode 0600)\n", defaults.SlackTokenFile)
		}
	}
	cfg, err := config.Read(configPath)
	if err != nil {
		return err
	}
	if err := setupAgent(cfg, configPath, nil, in); err != nil {
		return err
	}
	cfg, err = config.Read(configPath)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("config still incomplete: %w", err)
	}
	if len(cfg.FetchHelper) == 0 {
		if _, err := slackapi.LoadToken(cfg.SlackTokenFile, slackapi.EnvToken); err != nil {
			fmt.Printf("no helper configured: the built-in Slack client needs a user token (xoxp-…) with channels:history, groups:history, im:history, mpim:history, channels:read, groups:read, im:read, mpim:read, users:read, chat:write\n  put it into %s (chmod 600) or export %s before `make install`; a browser token (xoxc-…) also needs the d cookie in %s\n", cfg.SlackTokenFile, slackapi.EnvToken, cfg.SlackCookieFile)
		}
	}
	fmt.Println("done: make install starts the daemon, `handoffd status` shows its state")
	return nil
}

func setupEnvironment(cfg config.Config, reader *bufio.Reader) setup.Environment {
	env := setup.Environment{
		ReadSecret: func(prompt string) (string, error) { return readSecret(reader, prompt) },
		Verify: func(token, cookie string) (setup.Identity, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			id, err := slackapi.New(token, "").WithCookie(cookie).AuthTest(ctx)
			if err != nil {
				return setup.Identity{}, err
			}
			return setup.Identity{UserID: id.UserID, User: id.User, Team: id.Team, URL: id.URL}, nil
		},
		FindChannel: func(token, cookie, name string) (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			return slackapi.New(token, "").WithCookie(cookie).FindChannel(ctx, name)
		},
		HelperSuggestion: helperSuggestion(),
		Alerts:           len(cfg.AlertCommand) > 0 && binaryPath(cfg.AlertCommand[0], filepath.Base(cfg.AlertCommand[0])) != "",
	}
	for _, t := range []struct{ name, bin string }{{"tmux", cfg.TmuxBin}, {"wezterm", cfg.WezTermBin}, {"herdr", cfg.HerdrBin}, {"cmux", cfg.CmuxBin}} {
		if binaryPath(t.bin, t.name) != "" {
			env.Terminals = append(env.Terminals, t.name)
		}
	}
	return env
}

func helperSuggestion() []string {
	return strings.Fields(os.Getenv("HANDOFFD_SLACK_HELPER"))
}

func readSecret(reader *bufio.Reader, prompt string) (string, error) {
	fmt.Print(prompt)
	echoOff := exec.Command("stty", "-echo")
	echoOff.Stdin = os.Stdin
	hidden := echoOff.Run() == nil
	line, err := reader.ReadString('\n')
	if hidden {
		echoOn := exec.Command("stty", "echo")
		echoOn.Stdin = os.Stdin
		_ = echoOn.Run()
	}
	if err != nil && line == "" && err != io.EOF {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func setupAgent(cfg config.Config, configPath string, args []string, in io.Reader) error {
	detected := detectAgents(cfg)
	if len(args) > 0 {
		cfg.Agent = args[0]
	}
	chosen, err := agent.Resolve(cfg, detected)
	if err != nil && cfg.Agent == "" && len(detected) > 1 {
		chosen, err = agent.Choose(detected, in, os.Stdout)
	}
	if err != nil {
		return err
	}
	if err := agent.WriteConfig(configPath, chosen.Kind); err != nil {
		return err
	}
	fmt.Printf("agent: %s (%s) written to %s\n", chosen.Kind, chosen.Bin, configPath)

	if chosen.Kind != agent.Codex {
		return nil
	}
	hooksPath := filepath.Join(codexHome(), "hooks.json")
	changed, err := agent.InstallCodexHooks(hooksPath, session.Groups(selfPath(), configPath, session.CodexEvents))
	if err != nil {
		return err
	}
	if changed {
		fmt.Printf("session state hooks added to %s\n", hooksPath)
		fmt.Println("Codex runs only trusted hooks: open `codex`, type /hooks and trust the handoffd hook.")
		fmt.Println("Until then session state stays unknown and thread continuations reach the window without a dialog check.")
	}
	return nil
}

func codexHome() string {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return home
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".codex"
	}
	return filepath.Join(home, ".codex")
}
