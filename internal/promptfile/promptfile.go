package promptfile

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type Payload struct {
	Prompt       string   `json:"prompt"`
	AddDirs      []string `json:"add_dirs"`
	Model        string   `json:"model"`
	AllowedTools []string `json:"allowed_tools"`
	DeniedTools  []string `json:"denied_tools,omitempty"`
	SessionID    string   `json:"session_id,omitempty"`
	Settings     string   `json:"settings,omitempty"`
	Agent        string   `json:"agent,omitempty"`
	Check        bool     `json:"check,omitempty"`
}

type Dir struct {
	path string
}

func New(path string) Dir {
	return Dir{path: path}
}

func (d Dir) Write(p Payload) (string, error) {
	if err := os.MkdirAll(d.path, 0o700); err != nil {
		return "", fmt.Errorf("create prompt dir %s: %w", d.path, err)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("encode prompt: %w", err)
	}
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return "", fmt.Errorf("name prompt file: %w", err)
	}
	path := filepath.Join(d.path, "prompt-"+hex.EncodeToString(suffix)+".json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", fmt.Errorf("write prompt file: %w", err)
	}
	return path, nil
}

func Read(path string) (Payload, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Payload{}, fmt.Errorf("read prompt file %s: %w", path, err)
	}
	var p Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return Payload{}, fmt.Errorf("parse prompt file %s: %w", path, err)
	}
	if p.Prompt == "" {
		return Payload{}, fmt.Errorf("prompt file %s carries no prompt", path)
	}
	return p, nil
}
