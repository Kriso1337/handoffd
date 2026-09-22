package slackfetch

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Kriso1337/handoffd/internal/delivery"
)

type fakeCmd struct {
	call []string
	out  string
	err  error
}

func (f *fakeCmd) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.call = append([]string{name}, args...)
	return []byte(f.out), f.err
}

var helper = []string{"uv", "run", "--directory", "/cdc", "python", "/fetch.py"}

func TestFetchPassesChannelAndTimestamps(t *testing.T) {
	f := &fakeCmd{out: `{"author_id":"U200PEER","author_name":"teammate-b","text":"[HANDOFF] review"}`}

	got, err := New(f, helper).Fetch(context.Background(), "C100HOME", "1789049440.051109", "1789047527.174689")

	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.AuthorID != "U200PEER" || got.Text != "[HANDOFF] review" {
		t.Errorf("message = %+v", got)
	}
	call := strings.Join(f.call, " ")
	if !strings.HasPrefix(call, "uv run --directory /cdc python /fetch.py") {
		t.Errorf("helper invocation = %s", call)
	}
	if !strings.HasSuffix(call, "C100HOME 1789049440.051109 1789047527.174689") {
		t.Errorf("helper arguments = %s", call)
	}
}

func TestFetchFailsOnHelperError(t *testing.T) {
	f := &fakeCmd{out: "Traceback...", err: errors.New("exit 1")}

	if _, err := New(f, helper).Fetch(context.Background(), "C1", "1.1", ""); err == nil {
		t.Error("want error when helper exits non-zero")
	}
}

func TestFetchFailsOnReportedError(t *testing.T) {
	f := &fakeCmd{out: `{"error":"message not found"}`}

	_, err := New(f, helper).Fetch(context.Background(), "C1", "1.1", "")

	if err == nil {
		t.Fatal("want error when helper reports one")
	}
	if !strings.Contains(err.Error(), "message not found") {
		t.Errorf("err = %v", err)
	}
}

func TestFetchFailsOnUnparsableOutput(t *testing.T) {
	f := &fakeCmd{out: "not json"}

	if _, err := New(f, helper).Fetch(context.Background(), "C1", "1.1", ""); err == nil {
		t.Error("want error for unparsable helper output")
	}
}

func TestFetchFailsWithoutAuthor(t *testing.T) {
	f := &fakeCmd{out: `{"text":"hello"}`}

	if _, err := New(f, helper).Fetch(context.Background(), "C1", "1.1", ""); err == nil {
		t.Error("author is required to tell own messages from others")
	}
}

func TestFetchFailsWithoutHelper(t *testing.T) {
	if _, err := New(&fakeCmd{}, nil).Fetch(context.Background(), "C1", "1.1", ""); err == nil {
		t.Error("want error when helper is not configured")
	}
}

func TestFetchReadsBotFlagAndSubtype(t *testing.T) {
	cmd := &fakeCmd{out: `{"author_id":"B01BOT","author_name":"","is_bot":true,"subtype":"bot_message","text":"Pipeline passed","channel_label":"#ci","is_dm":false,"self_in_thread":false,"thread_size":1,"thread_tail":[]}`}

	got, err := New(cmd, []string{"python3", "helper.py"}).Fetch(context.Background(), "C1", "1.1", "")

	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !got.IsBot || got.Subtype != "bot_message" {
		t.Errorf("message = %+v", got)
	}
}

func TestPostSendsTextThroughTheHelperAndReturnsTS(t *testing.T) {
	f := &fakeCmd{out: `{"ts":"1789050000.000123"}`}

	ts, err := New(f, helper).Post(context.Background(), "C100HOME", "1789047527.174689", "🤖 Atlas: [TAKEN] x")

	if err != nil || ts != "1789050000.000123" {
		t.Fatalf("Post = %q, %v", ts, err)
	}
	call := strings.Join(f.call, " ")
	if !strings.HasSuffix(call, "--post C100HOME 1789047527.174689 🤖 Atlas: [TAKEN] x") {
		t.Errorf("helper arguments = %s", call)
	}

	f.out = `{"error":"channel_not_found"}`
	if _, err := New(f, helper).Post(context.Background(), "C100HOME", "", "x"); err == nil || !strings.Contains(err.Error(), "channel_not_found") {
		t.Errorf("helper error must surface, got %v", err)
	}
	f.out, f.err = "boom", errors.New("exit 1")
	if _, err := New(f, helper).Post(context.Background(), "C100HOME", "", "x"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("process failure must surface with output, got %v", err)
	}
	if _, err := New(f, nil).Post(context.Background(), "C100HOME", "", "x"); err == nil {
		t.Error("missing helper must be an error")
	}
}

func TestAHelperRefusalFromSlackSaysTheMessageWasNotAccepted(t *testing.T) {
	f := &fakeCmd{out: `{"error":"channel_not_found"}`}

	_, err := New(f, helper).Post(context.Background(), "C100HOME", "1789047527.174689", "text")

	if !errors.Is(err, delivery.ErrNotAccepted) {
		t.Errorf("err = %v, slack refusing the call means nothing reached the thread", err)
	}
}

func TestAHelperThatDiedLeavesAcceptanceOpen(t *testing.T) {
	f := &fakeCmd{err: errors.New("signal: killed")}

	_, err := New(f, helper).Post(context.Background(), "C100HOME", "1789047527.174689", "text")

	if err == nil {
		t.Fatal("Post must report the failure")
	}
	if errors.Is(err, delivery.ErrNotAccepted) {
		t.Errorf("err = %v, a helper killed mid-call cannot promise the message was not posted", err)
	}
}
