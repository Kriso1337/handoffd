package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kriso1337/handoffd/internal/launcher"
	"github.com/Kriso1337/handoffd/internal/linkq"
	"github.com/Kriso1337/handoffd/internal/mrref"
	"github.com/Kriso1337/handoffd/internal/outbox"
	"github.com/Kriso1337/handoffd/internal/slackfetch"
	"github.com/Kriso1337/handoffd/internal/state"
	"github.com/Kriso1337/handoffd/internal/worktree"
)

const linkedThreadTS = "1789480920.729569"

func linkRequest(id string) linkq.Request {
	return linkq.Request{
		ID: id, At: time.Date(2026, 9, 10, 13, 37, 0, 0, time.UTC), ThreadTS: linkedThreadTS,
		Kind: "other", Channel: channel, Label: "OPS", WindowID: "@115", Subject: "take this thread",
	}
}

func TestDrainLinksRenamesTheWindowAndRemembersTheThread(t *testing.T) {
	h := newHarness(t, slackfetch.Message{})
	h.liveWindow("@115", "help/Alex-Pro-1448")
	if _, err := h.links.Write(linkRequest("r1")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if n := h.pipe.DrainLinks(context.Background()); n != 1 {
		t.Fatalf("DrainLinks = %d, want 1", n)
	}

	if h.windows.renamed["@115"] != "sky/OPS-1338" {
		t.Errorf("renamed = %v", h.windows.renamed)
	}
	thread, ok := h.store.Thread(linkedThreadTS)
	if !ok || thread.WindowID != "@115" || thread.WindowName != "sky/OPS-1338" {
		t.Fatalf("thread = %+v, ok = %v", thread, ok)
	}
	rc, ok, err := h.links.ReadReceipt("r1")
	if err != nil || !ok || rc.Status != linkq.StatusLinked || rc.SessionID == "" {
		t.Errorf("receipt = %+v %v %v", rc, ok, err)
	}
}

func TestDrainLinksRejectsATakeoverAndLeavesTheHolderAlone(t *testing.T) {
	h := newHarness(t, slackfetch.Message{})
	h.liveWindows(
		launcher.Window{ID: "@115", Name: "help/Alex-Pro-1448", Command: "2.1.267", Occupancy: launcher.OccupancyAgent},
		launcher.Window{ID: "@116", Name: "sky/teammate-1716", Command: "2.1.267", Occupancy: launcher.OccupancyAgent},
	)
	h.store.SetThread(linkedThreadTS, state.Thread{WindowID: "@116", WindowName: "sky/teammate-1716", SessionID: "sess-held"})
	if _, err := h.links.Write(linkRequest("r1")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	h.pipe.DrainLinks(context.Background())

	rc, _, _ := h.links.ReadReceipt("r1")
	if rc.Status != linkq.StatusRejected {
		t.Fatalf("receipt = %+v", rc)
	}
	if thread, _ := h.store.Thread(linkedThreadTS); thread.WindowID != "@116" {
		t.Errorf("thread = %+v, want the holder untouched", thread)
	}
	if len(h.windows.renamed) != 0 {
		t.Errorf("rejected request renamed %v", h.windows.renamed)
	}
}

func TestDrainLinksClosesTheSessionOfAWindowItTakesOver(t *testing.T) {
	h := newHarness(t, slackfetch.Message{})
	h.liveWindows(
		launcher.Window{ID: "@115", Name: "help/Alex-Pro-1448", Command: "2.1.267", Occupancy: launcher.OccupancyAgent},
		launcher.Window{ID: "@116", Name: "sky/teammate-1716", Command: "2.1.267", Occupancy: launcher.OccupancyAgent},
	)
	h.store.SetThread(linkedThreadTS, state.Thread{WindowID: "@116", WindowName: "sky/teammate-1716", SessionID: "sess-held"})
	req := linkRequest("r1")
	req.Force = true
	if _, err := h.links.Write(req); err != nil {
		t.Fatalf("Write: %v", err)
	}

	h.pipe.DrainLinks(context.Background())

	rc, _, _ := h.links.ReadReceipt("r1")
	if rc.Status != linkq.StatusLinked || rc.WindowID != "@115" {
		t.Fatalf("receipt = %+v", rc)
	}
	var closed []string
	for _, e := range h.journal.entries {
		if e.SessionID == "sess-held" {
			closed = append(closed, e.Reason)
		}
	}
	if len(closed) != 1 || closed[0] != "relinked" {
		t.Errorf("journal entries for the released session = %v", closed)
	}
}

func TestDrainLinksQuarantinesAMalformedRequest(t *testing.T) {
	h := newHarness(t, slackfetch.Message{})
	h.liveWindow("@115", "help/Alex-Pro-1448")
	if err := os.MkdirAll(h.links.Path(), 0o700); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(h.links.Path(), "r2.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	h.pipe.DrainLinks(context.Background())

	if _, err := os.Stat(filepath.Join(h.links.Path(), "rejected", "r2.json")); err != nil {
		t.Errorf("malformed request must move to rejected/: %v", err)
	}
	rc, ok, _ := h.links.ReadReceipt("r2")
	if !ok || rc.Status != linkq.StatusRejected {
		t.Errorf("receipt = %+v %v", rc, ok)
	}
}

func linkedReviewRequest(id string) linkq.Request {
	req := linkRequest(id)
	req.Kind = "review"
	req.Refs = mrref.Extract(mrURL)
	req.Worktree = worktreePath
	return req
}

func TestALinkedReviewPinsTheProviderBaseWhenItStarts(t *testing.T) {
	h := newHarness(t, slackfetch.Message{})
	h.liveWindow("@115", "help/Alex-Pro-1448")
	h.worktrees.revision = worktree.Result{HeadSHA: oldSHA, BaseSHA: baseSHA}
	if _, err := h.links.Write(linkedReviewRequest("r1")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n := h.pipe.DrainLinks(context.Background()); n != 1 {
		t.Fatalf("DrainLinks = %d, want 1", n)
	}
	rc, _, _ := h.links.ReadReceipt("r1")

	h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseTaken, SHA: oldSHA, ThreadTS: linkedThreadTS, SessionID: rc.SessionID})
	h.post(t, outbox.Record{Kind: outbox.KindMarker, Phase: outbox.PhaseReviewStart, SHA: oldSHA, ThreadTS: linkedThreadTS, SessionID: rc.SessionID})

	th, _ := h.store.Thread(linkedThreadTS)
	if th.Run == nil {
		t.Fatal("no round after review start")
	}
	if th.Run.HeadSHA != oldSHA || th.Run.BaseSHA != baseSHA {
		t.Errorf("round after start = %+v, want the provider head and base pinned", th.Run)
	}
}

func TestALinkedReviewTheProviderCannotPinIsRejected(t *testing.T) {
	h := newHarness(t, slackfetch.Message{})
	h.liveWindow("@115", "help/Alex-Pro-1448")
	h.worktrees.revisionErr = errors.New("fetch refs/merge-requests/899/head failed")
	if _, err := h.links.Write(linkedReviewRequest("r1")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if n := h.pipe.DrainLinks(context.Background()); n != 0 {
		t.Fatalf("DrainLinks = %d, want the request rejected", n)
	}

	rc, _, _ := h.links.ReadReceipt("r1")
	if rc.Status != linkq.StatusRejected {
		t.Fatalf("receipt = %+v", rc)
	}
	if _, ok := h.store.Thread(linkedThreadTS); ok {
		t.Error("a review that cannot be pinned must not become a thread")
	}
}
