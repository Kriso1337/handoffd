package launcher

import (
	"strings"
	"testing"
)

func TestRegistryOpensEveryAdvertisedTerminal(t *testing.T) {
	for _, name := range Names() {
		backend, err := Open(name, Deps{
			Commander: &fakeCmd{}, Bin: "/bin/" + name, Session: "main",
			Self: "/usr/local/bin/handoffd", ConfigPath: "/cfg/config.json",
		})
		if err != nil {
			t.Errorf("Open(%s): %v", name, err)
		}
		if backend == nil {
			t.Errorf("Open(%s) returned no backend", name)
		}
	}
}

func TestRegistryRejectsAnUnknownTerminalAndNamesTheSupportedOnes(t *testing.T) {
	_, err := Open("kitty", Deps{Commander: &fakeCmd{}})

	if err == nil {
		t.Fatal("an unknown terminal must be refused")
	}
	for _, name := range Names() {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error must name %q: %v", name, err)
		}
	}
}

func TestOnlyTmuxRequiresASession(t *testing.T) {
	if !NeedsSession("tmux") {
		t.Error("tmux addresses windows through a session")
	}
	for _, name := range []string{"wezterm", "herdr", "kitty"} {
		if NeedsSession(name) {
			t.Errorf("%s must not require a tmux session", name)
		}
	}
}

func TestSupportedMatchesNames(t *testing.T) {
	for _, name := range Names() {
		if !Supported(name) {
			t.Errorf("%s is listed but not supported", name)
		}
	}
	if Supported("kitty") {
		t.Error("kitty is not a supported terminal")
	}
}
