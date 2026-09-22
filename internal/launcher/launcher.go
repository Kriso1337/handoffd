package launcher

import (
	"context"
	"fmt"
	"os"
	"strings"
)

const fieldSeparator = "|"

var shellCommands = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true, "ksh": true,
}

type Commander interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type Window struct {
	ID        string
	Name      string
	Command   string
	Dead      bool
	Occupancy Occupancy
}

type Occupancy int

const (
	OccupancyUnknown Occupancy = iota
	OccupancyAgent
	OccupancyFree
)

func (o Occupancy) String() string {
	switch o {
	case OccupancyAgent:
		return "agent"
	case OccupancyFree:
		return "free"
	default:
		return "unknown"
	}
}

func occupancy(dead, known bool, command string) Occupancy {
	switch {
	case dead:
		return OccupancyFree
	case !known:
		return OccupancyUnknown
	case shellCommands[command]:
		return OccupancyFree
	default:
		return OccupancyAgent
	}
}

func (w Window) HoldsAgent() bool {
	return w.Occupancy == OccupancyAgent
}

func (w Window) MayHoldAgent() bool {
	return w.Occupancy != OccupancyFree
}

type Tmux struct {
	cmd        Commander
	bin        string
	session    string
	self       string
	configPath string
	env        func(string) string
}

func getenv(lookup func(string) string, key string) string {
	if lookup == nil {
		lookup = os.Getenv
	}
	return strings.TrimSpace(lookup(key))
}

func New(cmd Commander, bin, session, self, configPath string) Tmux {
	return Tmux{cmd: cmd, bin: bin, session: session, self: self, configPath: configPath}
}

func (t Tmux) Windows(ctx context.Context) ([]Window, error) {
	out, err := t.cmd.Run(ctx, t.bin, "list-windows", "-t", t.session,
		"-F", "#{window_id}"+fieldSeparator+"#{window_name}"+fieldSeparator+
			"#{pane_current_command}"+fieldSeparator+"#{pane_dead}")
	if err != nil {
		if sessionGone(out) {
			return nil, nil
		}
		return nil, fmt.Errorf("list tmux windows in %s: %w: %s", t.session, err, out)
	}

	listing := strings.TrimSpace(string(out))
	if listing == "" {
		return nil, nil
	}

	var windows []Window
	for _, line := range strings.Split(listing, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, fieldSeparator)
		if len(fields) < 4 {
			continue
		}
		dead := fields[3] == "1"
		windows = append(windows, Window{
			ID:        fields[0],
			Name:      fields[1],
			Command:   fields[2],
			Dead:      dead,
			Occupancy: occupancy(dead, true, fields[2]),
		})
	}
	if len(windows) == 0 {
		return nil, fmt.Errorf("list tmux windows in %s: no parsable rows in %q", t.session, listing)
	}
	return windows, nil
}

func sessionGone(out []byte) bool {
	text := strings.ToLower(string(out))
	for _, absent := range []string{"no server running", "can't find session", "session not found", "no such session"} {
		if strings.Contains(text, absent) {
			return true
		}
	}
	return false
}

func (t Tmux) Current(ctx context.Context) (Window, bool, error) {
	pane := getenv(t.env, "TMUX_PANE")
	if pane == "" {
		return Window{}, false, nil
	}
	out, err := t.cmd.Run(ctx, t.bin, "display-message", "-p", "-t", pane, "#{window_id}")
	if err != nil {
		if sessionGone(out) {
			return Window{}, false, nil
		}
		return Window{}, false, fmt.Errorf("resolve tmux window of %s: %w: %s", pane, err, out)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return Window{}, false, nil
	}
	return t.Find(ctx, id)
}

func (t Tmux) Rename(ctx context.Context, id, name string) error {
	if out, err := t.cmd.Run(ctx, t.bin, "rename-window", "-t", id, name); err != nil {
		return fmt.Errorf("rename tmux window %s: %w: %s", id, err, out)
	}
	if out, err := t.cmd.Run(ctx, t.bin, "set-option", "-w", "-t", id, "automatic-rename", "off"); err != nil {
		return fmt.Errorf("pin name of tmux window %s: %w: %s", id, err, out)
	}
	return nil
}

func (t Tmux) Find(ctx context.Context, id string) (Window, bool, error) {
	windows, err := t.Windows(ctx)
	if err != nil {
		return Window{}, false, err
	}
	for _, w := range windows {
		if w.ID == id {
			return w, true, nil
		}
	}
	return Window{}, false, nil
}

func (t Tmux) NewWindow(ctx context.Context, name, cwd, promptFile string) (string, error) {
	args := []string{"new-window", "-d", "-P", "-F", "#{window_id}", "-t", t.session, "-n", name}
	if cwd != "" {
		args = append(args, "-c", cwd)
	}
	args = append(args, t.self, "-config", t.configPath, "run-agent", promptFile)

	out, err := t.cmd.Run(ctx, t.bin, args...)
	if err != nil {
		return "", fmt.Errorf("create tmux window %s: %w: %s", name, err, out)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", fmt.Errorf("create tmux window %s: empty window id", name)
	}

	if out, err := t.cmd.Run(ctx, t.bin, "set-option", "-w", "-t", id, "automatic-rename", "off"); err != nil {
		return id, fmt.Errorf("pin name of tmux window %s: %w: %s", id, err, out)
	}
	return id, nil
}

func (t Tmux) HoldOnExit(ctx context.Context, id string) error {
	if out, err := t.cmd.Run(ctx, t.bin, "set-option", "-w", "-t", id, "remain-on-exit", "on"); err != nil {
		return fmt.Errorf("hold tmux window %s on exit: %w: %s", id, err, out)
	}
	return nil
}

func (t Tmux) CapturePane(ctx context.Context, id string) (string, error) {
	out, err := t.cmd.Run(ctx, t.bin, "capture-pane", "-p", "-S", "-400", "-t", id)
	if err != nil {
		return "", fmt.Errorf("capture pane of %s: %w: %s", id, err, out)
	}
	return string(out), nil
}

func (t Tmux) KillWindow(ctx context.Context, id string) error {
	if out, err := t.cmd.Run(ctx, t.bin, "kill-window", "-t", id); err != nil {
		return fmt.Errorf("kill tmux window %s: %w: %s", id, err, out)
	}
	return nil
}

func (t Tmux) SendPrompt(ctx context.Context, id, text string) error {
	if strings.ContainsAny(text, "\n\r") {
		return fmt.Errorf("send prompt to %s: text must be single-line", id)
	}
	if out, err := t.cmd.Run(ctx, t.bin, "send-keys", "-t", id, "-l", "--", text); err != nil {
		return fmt.Errorf("send prompt to %s: %w: %s", id, err, out)
	}
	if out, err := t.cmd.Run(ctx, t.bin, "send-keys", "-t", id, "Enter"); err != nil {
		return fmt.Errorf("submit prompt in %s: %w: %s", id, err, out)
	}
	return nil
}
