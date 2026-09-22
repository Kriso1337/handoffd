package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kriso1337/handoffd/internal/config"
	"github.com/Kriso1337/handoffd/internal/linkq"
	"github.com/Kriso1337/handoffd/internal/state"
)

var linkNow = time.Date(2026, 9, 15, 17, 18, 0, 0, time.UTC)

func TestParseLinkNeedsAThread(t *testing.T) {
	if _, err := parseLink([]string{"review"}); err == nil || !strings.Contains(err.Error(), "--thread") {
		t.Errorf("err = %v, want the missing thread named", err)
	}
}

func TestParseLinkRefusesAnUnknownKind(t *testing.T) {
	if _, err := parseLink([]string{"--thread", "1789156662.321049", "--kind", "deploy"}); err == nil {
		t.Fatal("an unknown kind must be refused")
	}
}

func TestParseLinkTakesEveryKindNotOnlyReview(t *testing.T) {
	for _, kind := range []string{"review", "help", "other"} {
		opts, err := parseLink([]string{"--thread", "1789156662.321049", "--kind", kind})
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if opts.kind != kind {
			t.Errorf("%s: kind = %q", kind, opts.kind)
		}
	}
}

func TestParseLinkReadsTheMRFromTheRequestText(t *testing.T) {
	opts, err := parseLink([]string{
		"--thread", "1789156662.321049",
		"review https://gitlab.example.com/team-a/service-a/-/merge_requests/899",
	})

	if err != nil {
		t.Fatalf("parseLink: %v", err)
	}
	mr, ok := opts.refs.Primary()
	if !ok || mr.IID != 899 || mr.Project != "service-a" {
		t.Errorf("refs = %+v", opts.refs)
	}
	if !strings.Contains(opts.subject, "review") {
		t.Errorf("subject = %q, want the request kept", opts.subject)
	}
}

func TestParseLinkTakesALabelForThreadsWithoutAnMR(t *testing.T) {
	opts, err := parseLink([]string{"--thread", "1.1", "--kind", "help", "--label", "migrations", "question"})

	if err != nil {
		t.Fatal(err)
	}
	if opts.label != "migrations" {
		t.Errorf("label = %q", opts.label)
	}
}

func TestParseLinkTakesForceAndWait(t *testing.T) {
	opts, err := parseLink([]string{"--thread", "1.1", "--force", "--wait", "3s"})

	if err != nil {
		t.Fatal(err)
	}
	if !opts.force || opts.wait != 3*time.Second {
		t.Errorf("opts = %+v", opts)
	}
	plain, err := parseLink([]string{"--thread", "1.1"})
	if err != nil {
		t.Fatal(err)
	}
	if plain.force || plain.wait != linkWaitDefault {
		t.Errorf("opts = %+v, want no takeover and the default wait", plain)
	}
}

func TestLinkRequestCarriesTheWindowAndTheMR(t *testing.T) {
	opts, err := parseLink([]string{
		"--thread", "1789156662.321049",
		"review https://gitlab.example.com/team-a/service-a/-/merge_requests/899",
	})
	if err != nil {
		t.Fatal(err)
	}

	req := linkRequest(config.Config{Channel: "C100HOME"}, opts, "@26", linkNow)

	if req.ID == "" || !req.At.Equal(linkNow) {
		t.Errorf("envelope = %+v", req)
	}
	if req.WindowID != "@26" || req.ThreadTS != "1789156662.321049" || req.Kind != "review" {
		t.Errorf("request = %+v", req)
	}
	if req.Channel != "C100HOME" {
		t.Errorf("channel = %q, want the home channel by default", req.Channel)
	}
	mr, ok := req.Refs.Primary()
	if !ok || mr.IID != 899 {
		t.Errorf("refs = %+v, want the MR from the request", req.Refs)
	}
	if !strings.Contains(req.Subject, "review") {
		t.Errorf("subject = %q", req.Subject)
	}
	if req.Worktree == "" {
		t.Error("a linked window works where it stands; its directory must be recorded")
	}
	if err := req.Validate(); err != nil {
		t.Errorf("request must be valid for the watcher: %v", err)
	}
}

func TestLinkRequestKeepsAnExplicitChannelAndTakeover(t *testing.T) {
	opts, err := parseLink([]string{"--thread", "1.1", "--channel", "C200OTHER", "--kind", "help", "--force", "question"})
	if err != nil {
		t.Fatal(err)
	}

	req := linkRequest(config.Config{Channel: "C100HOME"}, opts, "@7", linkNow)

	if req.Channel != "C200OTHER" || !req.Force {
		t.Errorf("request = %+v", req)
	}
}

func TestSubmitLinkWaitsForTheReceiptTheWatcherWrites(t *testing.T) {
	dir := linkq.New(filepath.Join(t.TempDir(), "links"))
	req := linkq.Request{ID: "r1", At: linkNow, ThreadTS: "1.1", Kind: "other", WindowID: "@26"}
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = dir.WriteReceipt(linkq.Receipt{ID: "r1", Status: linkq.StatusLinked, WindowName: "sky/OPS-1718", SessionID: "sess-1"})
	}()

	rc, err := submitLink(context.Background(), io.Discard, dir, req, time.Second, nil)

	if err != nil {
		t.Fatalf("submitLink: %v", err)
	}
	if rc.WindowName != "sky/OPS-1718" {
		t.Errorf("receipt = %+v", rc)
	}
	if _, _, err := dir.Pending(); err != nil {
		t.Errorf("Pending: %v", err)
	}
}

func TestSubmitLinkReportsPendingWhenTheWatcherStaysSilent(t *testing.T) {
	dir := linkq.New(filepath.Join(t.TempDir(), "links"))
	req := linkq.Request{ID: "r1", At: linkNow, ThreadTS: "1.1", Kind: "other", WindowID: "@26"}

	var out bytes.Buffer

	_, err := submitLink(context.Background(), &out, dir, req, 20*time.Millisecond, nil)

	if !errors.Is(err, errLinkPending) {
		t.Fatalf("err = %v, want the request left pending", err)
	}
	if !strings.Contains(out.String(), "r1") || !strings.Contains(out.String(), "20ms") {
		t.Errorf("output = %q, want the caller told what happens next", out.String())
	}
	pending, _, readErr := dir.Pending()
	if readErr != nil || len(pending) != 1 {
		t.Errorf("the request must stay queued for the watcher: %+v %v", pending, readErr)
	}
}

func TestSubmitLinkAppliesLocallyWhenTheWatcherIsDown(t *testing.T) {
	dir := linkq.New(filepath.Join(t.TempDir(), "links"))
	req := linkq.Request{ID: "r1", At: linkNow, ThreadTS: "1.1", Kind: "other", WindowID: "@26"}
	local := func(context.Context, linkq.Request) (linkq.Receipt, error) {
		return linkq.Receipt{ID: "r1", Status: linkq.StatusLinked, WindowName: "sky/local-1718"}, nil
	}

	rc, err := submitLink(context.Background(), io.Discard, dir, req, time.Second, local)

	if err != nil || rc.WindowName != "sky/local-1718" {
		t.Fatalf("submitLink = (%+v, %v)", rc, err)
	}
	pending, _, _ := dir.Pending()
	if len(pending) != 0 {
		t.Errorf("a locally applied request must not be queued as well: %+v", pending)
	}
}

func TestReportLinkPrintsTheWindowAndTheSession(t *testing.T) {
	var out bytes.Buffer

	err := reportLink(&out, linkq.Receipt{
		Status: linkq.StatusLinked, WindowID: "@115", WindowName: "sky/OPS-1718", SessionID: "sess-1",
	}, "1789480920.729569")

	if err != nil {
		t.Fatalf("reportLink: %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "@115") || !strings.Contains(text, "sky/OPS-1718") ||
		!strings.Contains(text, "1789480920.729569") || !strings.Contains(text, "sess-1") {
		t.Errorf("output = %q", text)
	}
}

func TestReportLinkTurnsARejectionIntoAnError(t *testing.T) {
	var out bytes.Buffer

	err := reportLink(&out, linkq.Receipt{Status: linkq.StatusRejected, Reason: "thread is held by window @116"}, "1.1")

	if err == nil || !strings.Contains(err.Error(), "@116") {
		t.Fatalf("err = %v, want the reason surfaced", err)
	}
}

func linkConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Defaults()
	cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
	return cfg
}

func TestLocalLinkStandsDownWhileAWriterHoldsTheState(t *testing.T) {
	cfg := linkConfig(t)
	held, ok, err := state.Acquire(claimPath(cfg))
	if err != nil || !ok {
		t.Fatalf("Acquire = (%v, %v)", ok, err)
	}
	defer held.Release()

	local, err := localLink(cfg, nil)

	if err != nil {
		t.Fatalf("localLink: %v", err)
	}
	if local != nil {
		t.Error("the process that holds the state applies the request, this one queues it")
	}
}

func TestLocalLinkAppliesTheRequestWhenNoWriterHoldsTheState(t *testing.T) {
	cfg := linkConfig(t)

	local, err := localLink(cfg, nil)

	if err != nil {
		t.Fatalf("localLink: %v", err)
	}
	if local == nil {
		t.Fatal("with no writer to race there is nobody else to apply the request")
	}
}

func TestLocalLinkReleasesTheStateItClaimed(t *testing.T) {
	cfg := linkConfig(t)
	local, err := localLink(cfg, nil)
	if err != nil || local == nil {
		t.Fatalf("localLink: %v", err)
	}

	if _, err := local(context.Background(), linkq.Request{}); err == nil {
		t.Fatal("an invalid request must not be applied")
	}

	next, ok, err := state.Acquire(claimPath(cfg))
	if err != nil || !ok {
		t.Fatalf("Acquire = (%v, %v), want the claim released once the request is done", ok, err)
	}
	next.Release()
}

func TestTheWatcherRefusesToStartWhileAnotherWriterHoldsTheState(t *testing.T) {
	cfg := linkConfig(t)
	held, ok, err := state.Acquire(claimPath(cfg))
	if err != nil || !ok {
		t.Fatalf("Acquire = (%v, %v)", ok, err)
	}
	defer held.Release()

	claim, err := claimState(cfg)

	if err == nil {
		claim.Release()
		t.Fatal("two watchers must not write the same state")
	}
	if !strings.Contains(err.Error(), cfg.StatePath) {
		t.Errorf("err = %v, want the state it could not claim", err)
	}
}

func TestTheWatcherClaimsAFreeState(t *testing.T) {
	cfg := linkConfig(t)

	claim, err := claimState(cfg)

	if err != nil {
		t.Fatalf("claimState: %v", err)
	}
	claim.Release()
}
