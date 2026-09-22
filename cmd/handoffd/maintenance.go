package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Kriso1337/handoffd/internal/config"
	"github.com/Kriso1337/handoffd/internal/deadletter"
	"github.com/Kriso1337/handoffd/internal/journal"
	"github.com/Kriso1337/handoffd/internal/session"
	"github.com/Kriso1337/handoffd/internal/state"
	"github.com/Kriso1337/handoffd/internal/worktree"
)

func prune(cfg config.Config, dryRun bool) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	store, err := state.Open(cfg.StatePath, cfg.SeenCapacity)
	if err != nil {
		return err
	}
	worktrees := worktree.New(gitRunner{bin: cfg.GitBin, timeout: gitCommandTimeout}, cfg.RepoDir, nil).WithDir(cfg.WorktreeDir)
	pruneStale(context.Background(), cfg, worktrees, store, log, dryRun)
	return nil
}

func pruneStale(
	ctx context.Context,
	cfg config.Config,
	worktrees worktree.Preparer,
	store *state.Store,
	log *slog.Logger,
	dryRun bool,
) {
	keep := map[string]bool{}
	for _, thread := range store.Threads() {
		if thread.Worktree != "" {
			keep[thread.Worktree] = true
		}
	}
	report, err := worktrees.Prune(ctx, worktree.PruneOptions{
		ProjectsDir: cfg.ProjectsDir,
		Now:         time.Now(),
		TTL:         cfg.WorktreeTTL(),
		Keep:        keep,
		DryRun:      dryRun,
	})
	if err != nil {
		log.Warn("worktree prune incomplete", "error", err)
	}
	verb := "stale worktree removed"
	if dryRun {
		verb = "stale worktree would be removed"
	}
	for _, path := range report.Removed {
		log.Info(verb, "path", path)
	}
	for _, kept := range report.Kept {
		log.Warn("stale worktree kept", "path", kept.Path, "reason", kept.Reason)
	}
}

func replay(cfg config.Config) error {
	pending, err := deadletter.New(deadLetterPath(cfg)).Peek()
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		fmt.Println("dead letters: nothing to replay")
		return nil
	}
	for _, r := range pending {
		fmt.Printf("  %s %s/%s — %s\n", r.At.Local().Format("01-02 15:04"), r.Notification.Channel, r.Notification.MsgTS, r.Reason)
	}
	raw, err := os.ReadFile(pidPath(cfg))
	if err != nil {
		return fmt.Errorf("watcher pid unknown (%w); is it running?", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return fmt.Errorf("pid file %s: %w", pidPath(cfg), err)
	}
	if err := syscall.Kill(pid, syscall.SIGUSR1); err != nil {
		return fmt.Errorf("signal watcher pid %d: %w", pid, err)
	}
	fmt.Printf("asked watcher (pid %d) to replay %d dead letter(s)\n", pid, len(pending))
	return nil
}

func hook(cfg config.Config) error {
	now := time.Now()
	rec, ok, err := session.Apply(session.NewStore(sessionDir(cfg)), os.Getenv(session.EnvSessionID), os.Stdin, now)
	if err != nil {
		fmt.Fprintln(os.Stderr, "handoffd hook:", err)
		return nil
	}
	if !ok || rec.State != session.Ended {
		return nil
	}
	stats := rec.Stats(now)
	err = journal.NewWriter(journalPath(cfg)).Append(journal.Entry{
		At: now.UTC(), ID: "session_" + rec.SessionID, Source: journal.SourceHook,
		Kind: "session", Action: journal.ActionSession, Reason: rec.Event, SessionID: rec.SessionID, Session: &stats,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "handoffd hook:", err)
	}
	return nil
}
