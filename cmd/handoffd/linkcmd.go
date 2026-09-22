package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kriso1337/handoffd/internal/config"
	"github.com/Kriso1337/handoffd/internal/forge"
	"github.com/Kriso1337/handoffd/internal/launcher"
	"github.com/Kriso1337/handoffd/internal/linkq"
	"github.com/Kriso1337/handoffd/internal/mrref"
	"github.com/Kriso1337/handoffd/internal/state"
)

const (
	linkWaitDefault = 10 * time.Second
	linkPoll        = 200 * time.Millisecond
)

var errLinkPending = errors.New("link pending")

type linkOptions struct {
	thread  string
	kind    string
	refs    mrref.Refs
	subject string
	channel string
	label   string
	force   bool
	wait    time.Duration
}

func parseLink(args []string) (linkOptions, error) {
	fs := flag.NewFlagSet("link", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	thread := fs.String("thread", "", "thread root ts the window takes over")
	kind := fs.String("kind", "review", "review | help | other")
	channel := fs.String("channel", "", "channel the thread lives in (defaults to the home channel)")
	label := fs.String("label", "", "what to call the window when the thread has no MR")
	force := fs.Bool("force", false, "take the thread over from the window that holds it")
	wait := fs.Duration("wait", linkWaitDefault, "how long to wait for the watcher")
	if err := fs.Parse(args); err != nil {
		return linkOptions{}, err
	}
	root := strings.TrimSpace(*thread)
	if root == "" {
		return linkOptions{}, fmt.Errorf("link needs --thread <root ts>")
	}
	if _, ok := linkq.ParseKind(*kind); !ok {
		return linkOptions{}, fmt.Errorf("kind %q is not supported (review, help, other)", *kind)
	}
	text := strings.TrimSpace(strings.Join(fs.Args(), " "))
	return linkOptions{
		thread: root, kind: *kind, refs: mrref.Extract(text), subject: text,
		channel: strings.TrimSpace(*channel), label: strings.TrimSpace(*label),
		force: *force, wait: *wait,
	}, nil
}

func link(cfg config.Config, configPath string, args []string, out io.Writer) error {
	opts, err := parseLink(args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.wait+terminalCommandTimeout)
	defer cancel()

	windows := terminal(cfg, configPath)
	window, ok, err := windows.Current(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("link runs inside a %s window; this process is not in one", cfg.Terminal)
	}

	local, err := localLink(cfg, windows)
	if err != nil {
		return err
	}
	req := linkRequest(cfg, opts, window.ID, time.Now().UTC())
	receipt, err := submitLink(ctx, out, linkq.New(linksPath(cfg)), req, opts.wait, local)
	if err != nil {
		return err
	}
	return reportLink(out, receipt, opts.thread)
}

func submitLink(
	ctx context.Context,
	out io.Writer,
	dir linkq.Dir,
	req linkq.Request,
	wait time.Duration,
	local func(context.Context, linkq.Request) (linkq.Receipt, error),
) (linkq.Receipt, error) {
	if local != nil {
		return local(ctx, req)
	}
	if _, err := dir.Write(req); err != nil {
		return linkq.Receipt{}, err
	}
	receipt, ok, err := linkq.Wait(ctx, dir, req.ID, wait, linkPoll)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return linkq.Receipt{}, err
	}
	if !ok {
		fmt.Fprintf(out, "pending %s: no receipt within %s, the watcher applies the request when it catches up\n", req.ID, wait)
		return linkq.Receipt{}, errLinkPending
	}
	return receipt, nil
}

func localLink(cfg config.Config, windows launcher.Backend) (func(context.Context, linkq.Request) (linkq.Receipt, error), error) {
	claim, ok, err := state.Acquire(claimPath(cfg))
	if err != nil || !ok {
		return nil, err
	}
	return func(ctx context.Context, req linkq.Request) (linkq.Receipt, error) {
		defer func() { _ = claim.Release() }()
		store, err := state.Open(cfg.StatePath, cfg.SeenCapacity)
		if err != nil {
			return linkq.Receipt{}, err
		}
		forges := forge.New(commander{timeout: forgeCommandTimeout}, binaryPath(cfg.GlabBin, "glab"), binaryPath(cfg.GhBin, "gh"))
		return linkq.Apply(ctx, req, linkq.Deps{
			Windows: windows, Revisions: worktrees(cfg, forges), ReviewEnabled: cfg.ReviewEnabled, Allows: cfg.Allows,
			Store: store, Now: time.Now, NewID: newSessionID,
		})
	}, nil
}

func reportLink(out io.Writer, receipt linkq.Receipt, root string) error {
	if receipt.Status != linkq.StatusLinked {
		return fmt.Errorf("rejected: %s", receipt.Reason)
	}
	fmt.Fprintf(out, "window %s linked to thread %s as %s\n", receipt.WindowID, root, receipt.WindowName)
	fmt.Fprintf(out, "session %s\n", receipt.SessionID)
	return nil
}

func linkRequest(cfg config.Config, opts linkOptions, windowID string, now time.Time) linkq.Request {
	channel := opts.channel
	if channel == "" {
		channel = cfg.Channel
	}
	return linkq.Request{
		ID: linkq.NewID(), At: now, ThreadTS: opts.thread, Kind: opts.kind, Refs: opts.refs,
		Channel: channel, Label: opts.label, Subject: opts.subject,
		WindowID: windowID, Worktree: workingDir(), Force: opts.force,
	}
}

func claimPath(cfg config.Config) string {
	return cfg.StatePath + ".lock"
}

func linksPath(cfg config.Config) string {
	return filepath.Join(filepath.Dir(cfg.StatePath), "links")
}

func workingDir() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	return dir
}

func linkedSession(cfg config.Config, configPath string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), terminalCommandTimeout)
	defer cancel()
	window, ok, err := terminal(cfg, configPath).Current(ctx)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errNoSession
	}
	store, err := state.Open(cfg.StatePath, cfg.SeenCapacity)
	if err != nil {
		return "", err
	}
	for _, thread := range store.Threads() {
		if thread.WindowID == window.ID && thread.SessionID != "" {
			return thread.SessionID, nil
		}
	}
	return "", errNoSession
}
