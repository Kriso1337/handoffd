package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Kriso1337/handoffd/internal/agent"
	"github.com/Kriso1337/handoffd/internal/config"
	"github.com/Kriso1337/handoffd/internal/deadletter"
	"github.com/Kriso1337/handoffd/internal/journal"
	"github.com/Kriso1337/handoffd/internal/label"
	"github.com/Kriso1337/handoffd/internal/launcher"
	"github.com/Kriso1337/handoffd/internal/session"
	"github.com/Kriso1337/handoffd/internal/state"
	"github.com/Kriso1337/handoffd/internal/status"
)

func decisions(cfg config.Config, args []string) error {
	since := 24 * time.Hour
	asJSON := hasFlag(args, "--json")
	for i, arg := range args {
		if (arg == "--since" || arg == "-s") && i+1 < len(args) {
			d, err := time.ParseDuration(args[i+1])
			if err != nil {
				return fmt.Errorf("--since: %w", err)
			}
			since = d
		}
	}
	now := time.Now()
	history, err := journal.Read(journalPath(cfg), now.Add(-max(since, outcomeHistory)))
	if err != nil {
		return err
	}
	entries := journal.Since(history, now.Add(-since))
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		for _, e := range entries {
			if err := enc.Encode(e); err != nil {
				return err
			}
		}
		return nil
	}
	summary := journal.Summarize(entries)
	fmt.Printf("decisions for the last %s: %s\n", since, summary)
	if cost, ok := summary.Cost(cfg.CostPerTriageUSD, cfg.CostPerWindowUSD); ok {
		fmt.Println(cost)
	}
	outcomes := journal.Outcomes(history, now, cfg.StaleAfter())
	recent := make([]journal.Outcome, 0, len(outcomes))
	for _, o := range outcomes {
		if !o.OpenedAt.Before(now.Add(-since)) {
			recent = append(recent, o)
		}
	}
	fmt.Println(journal.SummarizeOutcomes(recent))
	if unfinished := journal.Unfinished(outcomes); len(unfinished) > 0 {
		fmt.Printf("\nunfinished (%d):\n", len(unfinished))
		for _, o := range unfinished {
			fmt.Printf("  %s %-9s %s %s %s/%s opened %s ago\n",
				o.OpenedAt.Local().Format("01-02 15:04"), o.Status, o.Kind, o.WindowName, o.Channel, o.Root, o.Age(now).Round(time.Minute))
		}
	}
	attention := journal.NeedsAttention(entries)
	if len(attention) == 0 {
		return nil
	}
	fmt.Printf("\nneeds attention (%d):\n", len(attention))
	for _, e := range attention {
		fmt.Printf("  %s %-8s %s %s/%s %s: %s — %s\n",
			e.At.Local().Format("01-02 15:04"), e.Action, e.Kind, e.Channel, e.MsgTS, e.AuthorName, e.Reason+e.Error, e.Text)
	}
	return nil
}

const outcomeHistory = 7 * 24 * time.Hour

func showStatus(cfg config.Config, configPath string, args []string) error {
	report, err := buildStatus(cfg, configPath)
	if err != nil {
		return err
	}
	switch {
	case hasFlag(args, "--json"):
		return json.NewEncoder(os.Stdout).Encode(report)
	case hasFlag(args, "--short"):
		fmt.Println(report.Short(cfg.BlockedAlertAfter()))
		return nil
	}
	fmt.Print(status.Render(report))
	return nil
}

func buildStatus(cfg config.Config, configPath string) (status.Report, error) {
	report := status.Report{Agent: cfg.Agent}
	if raw, err := os.ReadFile(pidPath(cfg)); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
			report.DaemonPID = pid
			report.DaemonAlive = syscall.Kill(pid, 0) == nil
		}
	}
	if report.Agent == "" {
		if runner, err := agent.Resolve(cfg, detectAgents(cfg)); err == nil {
			report.Agent = string(runner.Kind)
		}
	}
	if info, err := os.Stat(cfg.LogPath); err == nil {
		report.SlackLogAge = time.Since(info.ModTime())
		report.SlackLogStale = report.SlackLogAge > cfg.SlackLogStaleAfter()
	}

	now := time.Now()
	history, err := journal.Read(journalPath(cfg), now.Add(-outcomeHistory))
	if err != nil {
		return report, err
	}
	entries := journal.Since(history, now.Add(-24*time.Hour))
	if len(entries) > 0 {
		report.LastDecisionAge = time.Since(entries[len(entries)-1].At)
		summary := journal.Summarize(entries)
		report.Decisions24h = summary.String()
		if cost, ok := summary.Cost(cfg.CostPerTriageUSD, cfg.CostPerWindowUSD); ok {
			report.Decisions24h += "\n" + cost.String()
		}
	}
	outcomes := journal.Outcomes(history, now, cfg.StaleAfter())
	if len(outcomes) > 0 {
		report.Work = "work 7d: " + journal.SummarizeOutcomes(outcomes).String()
	}
	for _, o := range journal.Unfinished(outcomes) {
		report.Unfinished = append(report.Unfinished, status.Unfinished{Root: o.Root, Kind: o.Kind, WindowName: o.WindowName, Status: o.Status, Age: o.Age(now)})
	}
	pending, err := deadletter.New(deadLetterPath(cfg)).Peek()
	if err != nil {
		return report, err
	}
	report.DeadLetters = len(pending)

	store, err := state.Open(cfg.StatePath, cfg.SeenCapacity)
	if err != nil {
		return report, err
	}
	ctx := context.Background()
	windows, err := terminal(cfg, configPath).Windows(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, cfg.Terminal+":", err)
	}
	live := map[string]launcher.Window{}
	for _, w := range windows {
		if w.MayHoldAgent() {
			live[w.ID] = w
		}
	}
	sessions := session.NewStore(sessionDir(cfg))
	roots := make([]string, 0)
	threads := store.Threads()
	for root := range threads {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	for _, root := range roots {
		th := threads[root]
		entry := status.Thread{
			Root: root, Kind: th.Kind, Window: th.WindowID, WindowName: th.WindowName, Worktree: th.Worktree,
			Head: shortSHA(th.HeadSHA), Progress: th.Progress, ProgressSHA: shortSHA(th.ProgressSHA),
		}
		for _, m := range th.Members {
			label := m.Agent
			if m.Progress != "" {
				label += ":" + m.Progress
			}
			entry.Members = append(entry.Members, label)
		}
		if _, ok := live[th.WindowID]; ok && th.WindowID != "" {
			entry.Live = true
			if rec, ok, _ := sessions.Read(th.SessionID); ok {
				entry.Session = string(rec.State)
				entry.BlockedFor = rec.BlockedFor(now)
			}
		}
		report.Threads = append(report.Threads, entry)
	}
	return report, nil
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func labelDecision(cfg config.Config, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: handoffd label <id | ts | permalink> good|bad [note]")
	}
	ref, verdict, note := label.TimestampFromRef(args[0]), strings.ToLower(args[1]), strings.Join(args[2:], " ")
	entries, err := journal.Read(journalPath(cfg), time.Now().Add(-labelHistory))
	if err != nil {
		return err
	}
	entry, ok := findEntry(entries, ref)
	if !ok {
		return fmt.Errorf("no labelable decision for %q in the last %s", ref, labelHistory)
	}
	l := label.Label{
		At: time.Now().UTC(), ID: entry.ID, Verdict: verdict, React: journal.Reacted(entry.Action) == (verdict == label.Good),
		Kind: entry.Kind, Action: entry.Action, Text: entry.Text, Note: note, Source: label.SourceManual,
	}
	if err := label.New(labelPath(cfg)).Append(l); err != nil {
		return err
	}
	fmt.Printf("labelled %s %s (%s %s by %s): should have reacted = %v\n  %s\n",
		entry.ID, verdict, entry.Kind, entry.Action, entry.AuthorName, l.React, entry.Text)
	return nil
}

const labelHistory = 30 * 24 * time.Hour

func findEntry(entries []journal.Entry, ref string) (journal.Entry, bool) {
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if !journal.Labelable(e.Action) {
			continue
		}
		if e.ID == ref || e.MsgTS == ref || strings.HasSuffix(e.ID, "_"+ref) {
			return e, true
		}
	}
	return journal.Entry{}, false
}

func evaluate(cfg config.Config, args []string) error {
	since := labelHistory
	for i, arg := range args {
		if (arg == "--since" || arg == "-s") && i+1 < len(args) {
			d, err := time.ParseDuration(args[i+1])
			if err != nil {
				return fmt.Errorf("--since: %w", err)
			}
			since = d
		}
	}
	labels, err := label.Read(labelPath(cfg))
	if err != nil {
		return err
	}
	entries, err := journal.Read(journalPath(cfg), time.Now().Add(-since))
	if err != nil {
		return err
	}
	fmt.Print(label.Evaluate(labels, entries))
	return nil
}
