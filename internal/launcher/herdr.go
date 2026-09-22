package launcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Kriso1337/handoffd/internal/session"
)

const herdrCaptureLines = "400"

type Herdr struct {
	cmd        Commander
	bin        string
	session    string
	self       string
	configPath string
	env        func(string) string
}

func NewHerdr(cmd Commander, bin, sessionName, self, configPath string) Herdr {
	return Herdr{cmd: cmd, bin: bin, session: sessionName, self: self, configPath: configPath}
}

type herdrFailure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (f herdrFailure) Error() string {
	return f.Code + ": " + f.Message
}

type herdrPane struct {
	PaneID string `json:"pane_id"`
	Label  string `json:"label"`
	Title  string `json:"title"`
}

func (p herdrPane) name() string {
	if p.Label != "" {
		return p.Label
	}
	return p.Title
}

type herdrPaneList struct {
	Panes []herdrPane `json:"panes"`
}

type herdrPaneInfo struct {
	Pane herdrPane `json:"pane"`
}

type herdrWorkspace struct {
	RootPane herdrPane `json:"root_pane"`
}

type herdrProcessInfo struct {
	ProcessInfo struct {
		Foreground []struct {
			Name string `json:"name"`
		} `json:"foreground_processes"`
	} `json:"process_info"`
}

func (h Herdr) args(args ...string) []string {
	if h.session == "" {
		return args
	}
	return append([]string{"--session", h.session}, args...)
}

func (h Herdr) call(ctx context.Context, what string, result any, args ...string) error {
	out, runErr := h.cmd.Run(ctx, h.bin, h.args(args...)...)
	body := bytes.TrimSpace(out)
	if len(body) == 0 {
		if runErr != nil {
			return fmt.Errorf("%s: %w", what, runErr)
		}
		if result == nil {
			return nil
		}
		return fmt.Errorf("%s: empty response", what)
	}
	var reply struct {
		Result json.RawMessage `json:"result"`
		Error  *herdrFailure   `json:"error"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		if runErr != nil {
			return fmt.Errorf("%s: %w: %s", what, runErr, body)
		}
		return fmt.Errorf("%s: parse: %w: %s", what, err, body)
	}
	if reply.Error != nil {
		return fmt.Errorf("%s: %w", what, *reply.Error)
	}
	if runErr != nil {
		return fmt.Errorf("%s: %w: %s", what, runErr, body)
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(reply.Result, result); err != nil {
		return fmt.Errorf("%s: parse result: %w: %s", what, err, body)
	}
	return nil
}

func notFound(err error) bool {
	var failure herdrFailure
	return errors.As(err, &failure) && strings.HasSuffix(failure.Code, "not_found")
}

func (h Herdr) Windows(ctx context.Context) ([]Window, error) {
	var list herdrPaneList
	if err := h.call(ctx, "list herdr panes", &list, "pane", "list"); err != nil {
		return nil, err
	}
	windows := make([]Window, 0, len(list.Panes))
	for _, pane := range list.Panes {
		command, err := h.foreground(ctx, pane.PaneID)
		if err != nil {
			return nil, err
		}
		windows = append(windows, Window{
			ID: pane.PaneID, Name: pane.name(), Command: command,
			Occupancy: occupancy(false, true, command),
		})
	}
	return windows, nil
}

func (h Herdr) foreground(ctx context.Context, id string) (string, error) {
	var info herdrProcessInfo
	if err := h.call(ctx, "read herdr pane process "+id, &info, "pane", "process-info", "--pane", id); err != nil {
		return "", err
	}
	if len(info.ProcessInfo.Foreground) == 0 {
		return "", nil
	}
	return info.ProcessInfo.Foreground[0].Name, nil
}

func (h Herdr) Current(ctx context.Context) (Window, bool, error) {
	pane := getenv(h.env, "HERDR_PANE_ID")
	if pane == "" {
		return Window{}, false, nil
	}
	return h.Find(ctx, pane)
}

func (h Herdr) Rename(ctx context.Context, id, name string) error {
	return h.call(ctx, "name herdr pane "+id, nil, "pane", "rename", id, name)
}

func (h Herdr) Find(ctx context.Context, id string) (Window, bool, error) {
	var info herdrPaneInfo
	err := h.call(ctx, "get herdr pane "+id, &info, "pane", "get", id)
	if notFound(err) {
		return Window{}, false, nil
	}
	if err != nil {
		return Window{}, false, err
	}
	command, err := h.foreground(ctx, id)
	if err != nil {
		return Window{}, false, err
	}
	return Window{
		ID: info.Pane.PaneID, Name: info.Pane.name(), Command: command,
		Occupancy: occupancy(false, true, command),
	}, true, nil
}

func (h Herdr) NewWindow(ctx context.Context, name, cwd, promptFile string) (string, error) {
	args := []string{"workspace", "create", "--label", name}
	if cwd != "" {
		args = append(args, "--cwd", cwd)
	}
	var created herdrWorkspace
	if err := h.call(ctx, "create herdr workspace "+name, &created, args...); err != nil {
		return "", err
	}
	id := created.RootPane.PaneID
	if id == "" {
		return "", fmt.Errorf("create herdr workspace %s: empty pane id", name)
	}
	if err := h.Rename(ctx, id, name); err != nil {
		return id, err
	}
	command := strings.Join([]string{
		session.ShellQuote(h.self), "-config", session.ShellQuote(h.configPath), "run-agent", session.ShellQuote(promptFile),
	}, " ")
	if err := h.call(ctx, "run agent in herdr pane "+id, nil, "pane", "run", id, command); err != nil {
		return id, err
	}
	return id, nil
}

func (h Herdr) HoldOnExit(context.Context, string) error {
	return nil
}

func (h Herdr) CapturePane(ctx context.Context, id string) (string, error) {
	out, err := h.cmd.Run(ctx, h.bin, h.args("pane", "read", id, "--source", "recent", "--lines", herdrCaptureLines)...)
	if err != nil {
		return "", fmt.Errorf("read herdr pane %s: %w: %s", id, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (h Herdr) KillWindow(ctx context.Context, id string) error {
	err := h.call(ctx, "close herdr pane "+id, nil, "pane", "close", id)
	if notFound(err) {
		return nil
	}
	return err
}

func (h Herdr) SendPrompt(ctx context.Context, id, text string) error {
	if strings.ContainsAny(text, "\n\r") {
		return fmt.Errorf("send prompt to %s: text must be single-line", id)
	}
	if err := h.call(ctx, "send prompt to herdr pane "+id, nil, "pane", "send-text", id, text); err != nil {
		return err
	}
	return h.call(ctx, "submit prompt in herdr pane "+id, nil, "pane", "send-keys", id, "enter")
}
