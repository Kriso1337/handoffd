package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Kriso1337/handoffd/internal/agent"
	"github.com/Kriso1337/handoffd/internal/config"
	"github.com/Kriso1337/handoffd/internal/pipeline"
	"github.com/Kriso1337/handoffd/internal/promptfile"
	"github.com/Kriso1337/handoffd/internal/session"
)

func fetchMessage(cfg config.Config, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: handoffd fetch <channel> <ts> [thread_ts] | fetch --replies <channel> <thread_ts> <since_ts>")
	}
	source, err := slackSource(cfg)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if args[0] == "--replies" {
		if len(args) < 4 {
			return errors.New("usage: handoffd fetch --replies <channel> <thread_ts> <since_ts>")
		}
		replies, err := source.Replies(context.Background(), args[1], args[2], args[3])
		if err != nil {
			return err
		}
		return enc.Encode(replies)
	}
	threadTS := ""
	if len(args) > 2 {
		threadTS = args[2]
	}
	msg, err := source.Fetch(context.Background(), args[0], args[1], threadTS)
	if err != nil {
		return err
	}
	return enc.Encode(msg)
}

func runAgent(cfg config.Config, path string) error {
	payload, err := promptfile.Read(path)
	if err != nil {
		return err
	}
	if payload.Check {
		return runCheckWindow(payload)
	}
	if payload.Agent != "" {
		cfg.Agent = payload.Agent
	}
	runner, err := agent.Resolve(cfg, detectAgents(cfg))
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}

	env := os.Environ()
	if payload.SessionID != "" {
		env = append(env, session.EnvSessionID+"="+payload.SessionID)
	}
	return syscall.Exec(runner.Bin, agent.Command(runner, payload), env)
}

func runCheckWindow(payload promptfile.Payload) error {
	fmt.Println(checkBanner)
	fmt.Println(payload.Prompt)
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		fmt.Println(checkEchoMark + scanner.Text())
	}
	time.Sleep(checkTimeout)
	return nil
}

func terminalCheck(cfg config.Config, configPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*checkTimeout)
	defer cancel()
	windows := terminal(cfg, configPath)
	prompts := promptfile.New(filepath.Join(filepath.Dir(cfg.StatePath), "prompts"))
	file, err := prompts.Write(promptfile.Payload{Prompt: "type anything, this window closes by itself", Check: true})
	if err != nil {
		return err
	}
	name := "sky/check-" + time.Now().Format("1504")
	id, err := windows.NewWindow(ctx, name, cfg.ProjectsDir, file)
	if err != nil && id == "" {
		return fmt.Errorf("new window: %w", err)
	}
	if err != nil {
		fmt.Printf("new window %s opened with a warning: %v\n", id, err)
	}
	fmt.Printf("window %s (%s) opened in %s\n", id, name, cfg.Terminal)
	defer func() {
		if err := windows.KillWindow(context.Background(), id); err != nil {
			fmt.Printf("kill window: %v\n", err)
			return
		}
		fmt.Println("window killed")
	}()

	if err := waitForPane(ctx, windows, id, checkBanner); err != nil {
		return fmt.Errorf("window never printed the banner: %w", err)
	}
	window, found, err := windows.Find(ctx, id)
	if err != nil || !found {
		return fmt.Errorf("find window %s: found=%v err=%v", id, found, err)
	}
	fmt.Printf("found: name=%q command=%q occupancy=%s\n", window.Name, window.Command, window.Occupancy)
	if err := windows.SendPrompt(ctx, id, "ping from terminal-check"); err != nil {
		return fmt.Errorf("send prompt: %w", err)
	}
	if err := waitForPane(ctx, windows, id, checkEchoMark+"ping from terminal-check"); err != nil {
		return fmt.Errorf("window never echoed the prompt: %w", err)
	}
	fmt.Println("send-prompt and capture-pane work")
	if err := windows.HoldOnExit(ctx, id); err != nil {
		fmt.Printf("hold-on-exit unsupported: %v\n", err)
	}
	fmt.Println("terminal check passed")
	return nil
}

func waitForPane(ctx context.Context, windows pipeline.Windows, id, needle string) error {
	for {
		pane, err := windows.CapturePane(ctx, id)
		if err == nil && strings.Contains(pane, needle) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last error: %v)", ctx.Err(), err)
		case <-time.After(checkPoll):
		}
	}
}
