package promptfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteThenReadRoundTrip(t *testing.T) {
	dir := New(filepath.Join(t.TempDir(), "prompts"))

	path, err := dir.Write(Payload{Prompt: "New mention", AddDirs: []string{"/repo/wt2"}})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if got.Prompt != "New mention" {
		t.Errorf("prompt = %q", got.Prompt)
	}
	if len(got.AddDirs) != 1 || got.AddDirs[0] != "/repo/wt2" {
		t.Errorf("add_dirs = %v", got.AddDirs)
	}
}

func TestWriteCreatesDistinctFiles(t *testing.T) {
	dir := New(filepath.Join(t.TempDir(), "prompts"))

	first, err := dir.Write(Payload{Prompt: "a"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	second, err := dir.Write(Payload{Prompt: "b"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if first == second {
		t.Errorf("both prompts landed in %q", first)
	}
}

func TestWriteKeepsFilePrivate(t *testing.T) {
	dir := New(filepath.Join(t.TempDir(), "prompts"))

	path, err := dir.Write(Payload{Prompt: "thread secret"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", info.Mode().Perm())
	}
}

func TestReadRejectsMissingFile(t *testing.T) {
	if _, err := Read(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Error("want error for missing prompt file")
	}
}

func TestReadRejectsEmptyPrompt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(path, []byte(`{"add_dirs":["/x"]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Read(path); err == nil {
		t.Error("want error when prompt is empty")
	}
}

func TestWriteThenReadKeepsSessionAndToolPolicy(t *testing.T) {
	dir := New(t.TempDir())
	in := Payload{
		Prompt:      "review",
		DeniedTools: []string{"Bash(git push:*)"},
		SessionID:   "33333333-3333-3333-3333-333333333333",
		Settings:    `{"hooks":{}}`,
	}
	path, err := dir.Write(in)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := Read(path)

	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.SessionID != in.SessionID || got.Settings != in.Settings || len(got.DeniedTools) != 1 {
		t.Errorf("payload = %+v", got)
	}
}
