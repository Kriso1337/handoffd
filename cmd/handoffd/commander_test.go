package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Kriso1337/handoffd/internal/config"
)

func TestCommanderKillsACommandThatOutlivesItsDeadline(t *testing.T) {
	start := time.Now()

	_, err := commander{timeout: 100 * time.Millisecond}.Run(context.Background(), "/bin/sleep", "30")

	if err == nil {
		t.Fatal("a command past its deadline must fail, not hold the caller")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %s, the deadline did not stop the command", elapsed)
	}
}

func TestGitRunnerKillsACommandThatOutlivesItsDeadline(t *testing.T) {
	start := time.Now()

	_, err := gitRunner{bin: "/bin/sleep", timeout: 100 * time.Millisecond}.Run(context.Background(), t.TempDir(), "30")

	if err == nil {
		t.Fatal("a git command past its deadline must fail, not hold the caller")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %s, the deadline did not stop the command", elapsed)
	}
}

func TestCommanderWithoutADeadlineStillHonoursTheCallerContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()

	if _, err := (commander{}).Run(ctx, "/bin/sleep", "30"); err == nil {
		t.Fatal("a cancelled context must stop the command")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %s", elapsed)
	}
}

func TestCommanderRunsAQuickCommandToCompletion(t *testing.T) {
	out, err := commander{timeout: 5 * time.Second}.Run(context.Background(), "/bin/echo", "ok")

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(out) != "ok\n" {
		t.Errorf("out = %q", out)
	}
}

func TestCmuxPasswordComesFromTheFileWhenTheEnvIsEmpty(t *testing.T) {
	t.Setenv("CMUX_SOCKET_PASSWORD", "")
	file := filepath.Join(t.TempDir(), "cmux_password")
	if err := os.WriteFile(file, []byte(" s3cret \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := cmuxPassword(config.Config{CmuxPasswordFile: file})

	if !slices.Contains(env, "CMUX_SOCKET_PASSWORD=s3cret") {
		t.Errorf("env has no password from the file: %v", lastEntries(env))
	}
}

func TestCmuxPasswordKeepsAnEnvironmentThatAlreadyHasOne(t *testing.T) {
	t.Setenv("CMUX_SOCKET_PASSWORD", "from-env")
	file := filepath.Join(t.TempDir(), "cmux_password")
	if err := os.WriteFile(file, []byte("from-file"), 0o600); err != nil {
		t.Fatal(err)
	}

	if env := cmuxPassword(config.Config{CmuxPasswordFile: file}); env != nil {
		t.Errorf("env = %v, an inherited password must win and need no override", lastEntries(env))
	}
}

func TestCmuxPasswordIsAbsentWithoutAFile(t *testing.T) {
	t.Setenv("CMUX_SOCKET_PASSWORD", "")

	if env := cmuxPassword(config.Config{CmuxPasswordFile: filepath.Join(t.TempDir(), "missing")}); env != nil {
		t.Errorf("env = %v, want no override when there is no password", lastEntries(env))
	}
}

func TestHelperSuggestionComesFromEnvironment(t *testing.T) {
	t.Setenv("HANDOFFD_SLACK_HELPER", "/usr/local/bin/slack-helper --profile work")

	if got := helperSuggestion(); !slices.Equal(got, []string{"/usr/local/bin/slack-helper", "--profile", "work"}) {
		t.Errorf("helper suggestion = %v", got)
	}
}

func lastEntries(env []string) []string {
	if len(env) <= 3 {
		return env
	}
	return env[len(env)-3:]
}
