package launcher

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

type Backend interface {
	Windows(ctx context.Context) ([]Window, error)
	Find(ctx context.Context, id string) (Window, bool, error)
	NewWindow(ctx context.Context, name, cwd, promptFile string) (string, error)
	Current(ctx context.Context) (Window, bool, error)
	Rename(ctx context.Context, id, name string) error
	KillWindow(ctx context.Context, id string) error
	CapturePane(ctx context.Context, id string) (string, error)
	HoldOnExit(ctx context.Context, id string) error
	SendPrompt(ctx context.Context, id, text string) error
}

type Deps struct {
	Commander  Commander
	Bin        string
	Session    string
	Self       string
	ConfigPath string
}

type terminal struct {
	open         func(Deps) Backend
	needsSession bool
}

var terminals = map[string]terminal{
	"tmux": {
		open:         func(d Deps) Backend { return New(d.Commander, d.Bin, d.Session, d.Self, d.ConfigPath) },
		needsSession: true,
	},
	"wezterm": {
		open: func(d Deps) Backend { return NewWezTerm(d.Commander, d.Bin, d.Self, d.ConfigPath) },
	},
	"herdr": {
		open: func(d Deps) Backend { return NewHerdr(d.Commander, d.Bin, d.Session, d.Self, d.ConfigPath) },
	},
	"cmux": {
		open: func(d Deps) Backend { return NewCmux(d.Commander, d.Bin, d.Self, d.ConfigPath) },
	},
}

func Names() []string {
	names := make([]string, 0, len(terminals))
	for name := range terminals {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func Supported(name string) bool {
	_, ok := terminals[name]
	return ok
}

func NeedsSession(name string) bool {
	return terminals[name].needsSession
}

func Open(name string, d Deps) (Backend, error) {
	t, ok := terminals[name]
	if !ok {
		return nil, fmt.Errorf("terminal %q is not supported (%s)", name, strings.Join(Names(), ", "))
	}
	return t.open(d), nil
}
