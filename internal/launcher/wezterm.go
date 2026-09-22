package launcher

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

type WezTerm struct {
	cmd        Commander
	bin        string
	self       string
	configPath string
	env        func(string) string
}

func NewWezTerm(cmd Commander, bin, self, configPath string) WezTerm {
	return WezTerm{cmd: cmd, bin: bin, self: self, configPath: configPath}
}

type wezPane struct {
	PaneID   int    `json:"pane_id"`
	TabID    int    `json:"tab_id"`
	Title    string `json:"title"`
	TabTitle string `json:"tab_title"`
	CWD      string `json:"cwd"`
	TTY      string `json:"tty_name"`
}

func (w WezTerm) Windows(ctx context.Context) ([]Window, error) {
	out, err := w.cmd.Run(ctx, w.bin, "cli", "list", "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("list wezterm panes: %w: %s", err, out)
	}
	var panes []wezPane
	if err := json.Unmarshal(out, &panes); err != nil {
		return nil, fmt.Errorf("list wezterm panes: parse: %w: %s", err, strings.TrimSpace(string(out)))
	}
	commands, known := w.commandsByTTY(ctx)
	windows := make([]Window, 0, len(panes))
	for _, p := range panes {
		name := p.TabTitle
		if name == "" {
			name = p.Title
		}
		command, found := commands[strings.TrimPrefix(p.TTY, "/dev/")]
		windows = append(windows, Window{
			ID: strconv.Itoa(p.PaneID), Name: name, Command: command,
			Occupancy: occupancy(false, known && found, command),
		})
	}
	return windows, nil
}

func (w WezTerm) commandsByTTY(ctx context.Context) (map[string]string, bool) {
	out, err := w.cmd.Run(ctx, "ps", "-A", "-o", "tty=,comm=")
	if err != nil {
		return nil, false
	}
	return foregroundByTTY(string(out)), true
}

func foregroundByTTY(listing string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] == "??" || fields[0] == "?" {
			continue
		}
		tty, command := fields[0], filepath.Base(strings.TrimPrefix(fields[1], "-"))
		if current, ok := out[tty]; ok && !shellCommands[current] {
			continue
		}
		out[tty] = command
	}
	return out
}

func (w WezTerm) Current(ctx context.Context) (Window, bool, error) {
	pane := getenv(w.env, "WEZTERM_PANE")
	if pane == "" {
		return Window{}, false, nil
	}
	return w.Find(ctx, pane)
}

func (w WezTerm) Rename(ctx context.Context, id, name string) error {
	if out, err := w.cmd.Run(ctx, w.bin, "cli", "set-tab-title", "--pane-id", id, name); err != nil {
		return fmt.Errorf("name wezterm tab of pane %s: %w: %s", id, err, out)
	}
	return nil
}

func (w WezTerm) Find(ctx context.Context, id string) (Window, bool, error) {
	windows, err := w.Windows(ctx)
	if err != nil {
		return Window{}, false, err
	}
	for _, win := range windows {
		if win.ID == id {
			return win, true, nil
		}
	}
	return Window{}, false, nil
}

func (w WezTerm) NewWindow(ctx context.Context, name, cwd, promptFile string) (string, error) {
	args := []string{"cli", "spawn"}
	if cwd != "" {
		args = append(args, "--cwd", cwd)
	}
	args = append(args, "--", w.self, "-config", w.configPath, "run-agent", promptFile)
	out, err := w.cmd.Run(ctx, w.bin, args...)
	if err != nil {
		return "", fmt.Errorf("spawn wezterm pane %s: %w: %s", name, err, out)
	}
	id := strings.TrimSpace(string(out))
	if _, err := strconv.Atoi(id); err != nil {
		return "", fmt.Errorf("spawn wezterm pane %s: unexpected pane id %q", name, id)
	}
	if err := w.Rename(ctx, id, name); err != nil {
		return id, err
	}
	return id, nil
}

func (w WezTerm) HoldOnExit(context.Context, string) error {
	return nil
}

func (w WezTerm) CapturePane(ctx context.Context, id string) (string, error) {
	out, err := w.cmd.Run(ctx, w.bin, "cli", "get-text", "--pane-id", id, "--start-line", "-400")
	if err != nil {
		return "", fmt.Errorf("get text of wezterm pane %s: %w: %s", id, err, out)
	}
	return string(out), nil
}

func (w WezTerm) KillWindow(ctx context.Context, id string) error {
	if out, err := w.cmd.Run(ctx, w.bin, "cli", "kill-pane", "--pane-id", id); err != nil {
		return fmt.Errorf("kill wezterm pane %s: %w: %s", id, err, out)
	}
	return nil
}

func (w WezTerm) SendPrompt(ctx context.Context, id, text string) error {
	if strings.ContainsAny(text, "\n\r") {
		return fmt.Errorf("send prompt to %s: text must be single-line", id)
	}
	if out, err := w.cmd.Run(ctx, w.bin, "cli", "send-text", "--pane-id", id, "--no-paste", text+"\r"); err != nil {
		return fmt.Errorf("send prompt to wezterm pane %s: %w: %s", id, err, out)
	}
	return nil
}
