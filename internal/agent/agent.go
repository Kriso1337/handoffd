package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Kriso1337/handoffd/internal/config"
	"github.com/Kriso1337/handoffd/internal/promptfile"
	"github.com/Kriso1337/handoffd/internal/session"
)

type Kind string

const (
	Claude Kind = "claude"
	Codex  Kind = "codex"
)

var kinds = []Kind{Claude, Codex}

type Installed struct {
	Kind Kind
	Bin  string
}

func (i Installed) SupportsToolDenial() bool {
	return i.Kind == Claude
}

func (i Installed) PerSessionHooks() bool {
	return i.Kind == Claude
}

func configuredBin(cfg config.Config, kind Kind) string {
	switch kind {
	case Claude:
		return cfg.ClaudeBin
	case Codex:
		return cfg.CodexBin
	}
	return ""
}

func Detect(cfg config.Config, lookPath func(string) (string, error), stat func(string) error) []Installed {
	var out []Installed
	for _, kind := range kinds {
		if bin := configuredBin(cfg, kind); bin != "" && stat(bin) == nil {
			out = append(out, Installed{Kind: kind, Bin: bin})
			continue
		}
		if bin, err := lookPath(string(kind)); err == nil {
			out = append(out, Installed{Kind: kind, Bin: bin})
		}
	}
	return out
}

func Names(detected []Installed) []string {
	out := make([]string, 0, len(detected))
	for _, in := range detected {
		out = append(out, string(in.Kind))
	}
	return out
}

func Resolve(cfg config.Config, detected []Installed) (Installed, error) {
	if cfg.Agent != "" {
		for _, in := range detected {
			if string(in.Kind) == cfg.Agent {
				return in, nil
			}
		}
		return Installed{}, fmt.Errorf("agent %q is set in config but its binary was not found (%s)", cfg.Agent, configuredBin(cfg, Kind(cfg.Agent)))
	}
	switch len(detected) {
	case 0:
		return Installed{}, errors.New("no agent CLI found (claude, codex): set claude_bin/codex_bin or install one")
	case 1:
		return detected[0], nil
	}
	names := make([]string, 0, len(detected))
	for _, in := range detected {
		names = append(names, string(in.Kind))
	}
	return Installed{}, fmt.Errorf("several agents are installed (%s): pick one with `handoffd setup-agent` or the agent key in config",
		strings.Join(names, ", "))
}

func Choose(detected []Installed, in io.Reader, out io.Writer) (Installed, error) {
	fmt.Fprintln(out, "Several agent CLIs found. Which one should the watcher drive?")
	for i, agent := range detected {
		fmt.Fprintf(out, "  %d) %s  (%s)\n", i+1, agent.Kind, agent.Bin)
	}
	fmt.Fprint(out, "Number: ")
	reader, ok := in.(*bufio.Reader)
	if !ok {
		reader = bufio.NewReader(in)
	}
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return Installed{}, errors.New("no agent chosen")
	}
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(detected) {
		return Installed{}, fmt.Errorf("expected a number from 1 to %d", len(detected))
	}
	return detected[n-1], nil
}

func WriteConfig(path string, kind Kind) error {
	doc := map[string]any{}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("read config %s: %w", path, err)
	default:
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	doc["agent"] = string(kind)
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		return fmt.Errorf("write config %s: %w", path, err)
	}
	return nil
}

func InstallCodexHooks(hooksPath string, groups map[string][]any) (bool, error) {
	existing, err := os.ReadFile(hooksPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("read %s: %w", hooksPath, err)
	}
	merged, err := session.MergeHooksJSON(existing, groups)
	if err != nil {
		return false, err
	}
	if string(merged) == string(existing) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o700); err != nil {
		return false, fmt.Errorf("create %s: %w", filepath.Dir(hooksPath), err)
	}
	if err := os.WriteFile(hooksPath, merged, 0o644); err != nil {
		return false, fmt.Errorf("write %s: %w", hooksPath, err)
	}
	return true, nil
}

func Command(in Installed, p promptfile.Payload) []string {
	argv := []string{in.Bin}
	if p.Model != "" {
		argv = append(argv, "--model", p.Model)
	}
	if in.Kind == Claude {
		if p.SessionID != "" {
			argv = append(argv, "--session-id", p.SessionID)
		}
		if p.Settings != "" {
			argv = append(argv, "--settings", p.Settings)
		}
	}
	for _, dir := range p.AddDirs {
		argv = append(argv, "--add-dir", dir)
	}
	if in.Kind == Codex && len(p.DeniedTools) > 0 {
		argv = append(argv, "--sandbox", "read-only")
	}
	if in.Kind == Claude {
		if len(p.AllowedTools) > 0 {
			argv = append(argv, "--allowedTools", strings.Join(p.AllowedTools, ","))
		}
		if len(p.DeniedTools) > 0 {
			argv = append(argv, "--disallowedTools", strings.Join(p.DeniedTools, ","))
		}
	}
	return append(argv, "--", p.Prompt)
}
