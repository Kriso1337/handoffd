package launcher

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Kriso1337/handoffd/internal/session"
)

const cmuxCaptureLines = "400"

type Cmux struct {
	cmd        Commander
	bin        string
	self       string
	configPath string
	env        func(string) string
}

func NewCmux(cmd Commander, bin, self, configPath string) Cmux {
	return Cmux{cmd: cmd, bin: bin, self: self, configPath: configPath}
}

type cmuxWorkspace struct {
	ID          string `json:"id"`
	Ref         string `json:"ref"`
	Title       string `json:"title"`
	CustomTitle string `json:"custom_title"`
}

func (w cmuxWorkspace) name() string {
	if w.CustomTitle != "" {
		return w.CustomTitle
	}
	return w.Title
}

type cmuxWorkspaceList struct {
	Workspaces []cmuxWorkspace `json:"workspaces"`
}

func (c Cmux) run(ctx context.Context, what string, args ...string) (string, error) {
	out, runErr := c.cmd.Run(ctx, c.bin, args...)
	text := strings.TrimSpace(string(out))
	if message, failed := strings.CutPrefix(text, "Error:"); failed {
		return text, fmt.Errorf("%s: %s", what, strings.TrimSpace(message))
	}
	if runErr != nil {
		return text, fmt.Errorf("%s: %w: %s", what, runErr, text)
	}
	return text, nil
}

func cmuxNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not_found")
}

func (c Cmux) list(ctx context.Context) ([]cmuxWorkspace, error) {
	text, err := c.run(ctx, "list cmux workspaces", "workspace", "list", "--json")
	if err != nil {
		return nil, err
	}
	var listing cmuxWorkspaceList
	if err := json.Unmarshal([]byte(text), &listing); err != nil {
		return nil, fmt.Errorf("list cmux workspaces: parse: %w: %s", err, text)
	}
	return listing.Workspaces, nil
}

func (c Cmux) Windows(ctx context.Context) ([]Window, error) {
	workspaces, err := c.list(ctx)
	if err != nil {
		return nil, err
	}
	windows := make([]Window, 0, len(workspaces))
	for _, w := range workspaces {
		windows = append(windows, Window{ID: w.ID, Name: w.name(), Occupancy: OccupancyUnknown})
	}
	return windows, nil
}

func (c Cmux) Current(ctx context.Context) (Window, bool, error) {
	workspace := getenv(c.env, "CMUX_WORKSPACE_ID")
	if workspace == "" {
		return Window{}, false, nil
	}
	return c.Find(ctx, workspace)
}

func (c Cmux) Rename(ctx context.Context, id, name string) error {
	_, err := c.run(ctx, "rename cmux workspace "+id, "workspace", "rename", id, "--title", name)
	return err
}

func (c Cmux) Find(ctx context.Context, id string) (Window, bool, error) {
	workspaces, err := c.list(ctx)
	if err != nil {
		return Window{}, false, err
	}
	for _, w := range workspaces {
		if strings.EqualFold(w.ID, id) {
			return Window{ID: w.ID, Name: w.name(), Occupancy: OccupancyUnknown}, true, nil
		}
	}
	return Window{}, false, nil
}

func (c Cmux) NewWindow(ctx context.Context, name, cwd, promptFile string) (string, error) {
	command := strings.Join([]string{
		session.ShellQuote(c.self), "-config", session.ShellQuote(c.configPath), "run-agent", session.ShellQuote(promptFile),
	}, " ")
	args := []string{"workspace", "create", "--name", name, "--command", command}
	if cwd != "" {
		args = append(args, "--cwd", cwd)
	}
	text, err := c.run(ctx, "create cmux workspace "+name, args...)
	if err != nil {
		return "", err
	}
	ref := strings.TrimSpace(strings.TrimPrefix(text, "OK"))
	if ref == "" {
		return "", fmt.Errorf("create cmux workspace %s: no workspace in %q", name, text)
	}
	workspaces, err := c.list(ctx)
	if err != nil {
		return "", err
	}
	for _, w := range workspaces {
		if w.Ref == ref {
			return w.ID, nil
		}
	}
	return "", fmt.Errorf("create cmux workspace %s: %s is not in the workspace list", name, ref)
}

func (c Cmux) HoldOnExit(context.Context, string) error {
	return nil
}

func (c Cmux) CapturePane(ctx context.Context, id string) (string, error) {
	return c.run(ctx, "capture cmux workspace "+id, "capture-pane", "--workspace", id, "--lines", cmuxCaptureLines)
}

func (c Cmux) KillWindow(ctx context.Context, id string) error {
	_, err := c.run(ctx, "close cmux workspace "+id, "workspace", "close", id)
	if cmuxNotFound(err) {
		return nil
	}
	return err
}

func (c Cmux) SendPrompt(ctx context.Context, id, text string) error {
	if strings.ContainsAny(text, "\n\r") {
		return fmt.Errorf("send prompt to %s: text must be single-line", id)
	}
	if _, err := c.run(ctx, "send prompt to cmux workspace "+id, "send", "--workspace", id, text); err != nil {
		return err
	}
	_, err := c.run(ctx, "submit prompt in cmux workspace "+id, "send-key", "--workspace", id, "enter")
	return err
}
