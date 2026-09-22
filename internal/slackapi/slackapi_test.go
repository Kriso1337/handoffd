package slackapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Kriso1337/handoffd/internal/delivery"
)

const (
	self  = "U100SELF"
	token = "xoxp-test"
)

type fakeSlack struct {
	mu       sync.Mutex
	calls    []string
	replies  map[string][]map[string]any
	channels map[string]map[string]any
	users    map[string]string
	cookies  []string
	status   int
	posted   []url.Values
}

func (f *fakeSlack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.URL.Path+"?"+r.URL.RawQuery)
	f.cookies = append(f.cookies, r.Header.Get("Cookie"))
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+token {
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "not_authed"})
		return
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	q := r.URL.Query()
	switch strings.TrimPrefix(r.URL.Path, "/") {
	case "conversations.replies":
		messages, ok := f.replies[q.Get("ts")]
		if !ok {
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "thread_not_found"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": messages})
	case "conversations.info":
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": f.channels[q.Get("channel")]})
	case "users.info":
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "user": map[string]any{"profile": map[string]any{"display_name": f.users[q.Get("user")]}}})
	case "chat.postMessage":
		if r.Method != http.MethodPost {
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "method_not_allowed"})
			return
		}
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		f.mu.Lock()
		f.posted = append(f.posted, form)
		f.mu.Unlock()
		if form.Get("channel") == "" || form.Get("text") == "" {
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "no_text"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1789050000.000123", "channel": form.Get("channel")})
	case "auth.test":
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "user_id": self, "user": "alex", "team_id": "T1", "team": "Acme", "url": "https://acme.slack.com/"})
	case "conversations.list":
		if q.Get("cursor") == "" {
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "channels": []map[string]any{{"id": "C1", "name": "general"}}, "response_metadata": map[string]any{"next_cursor": "p2"}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "channels": []map[string]any{{"id": "C0AGENTS", "name": "team-agents"}}, "response_metadata": map[string]any{"next_cursor": ""}})
	default:
		http.NotFound(w, r)
	}
}

func server(t *testing.T, f *fakeSlack) *Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return New(token, self).WithBaseURL(srv.URL)
}

func thread() map[string][]map[string]any {
	root := "1789047527.174689"
	return map[string][]map[string]any{
		root: {
			{"ts": root, "user": "U200PEER", "text": "[HANDOFF] review", "thread_ts": root},
			{"ts": "1789047600.000001", "user": self, "text": "[TAKEN]", "thread_ts": root},
			{"ts": "1789047700.000002", "user": "U200PEER", "text": "replied to findings " + strings.Repeat("x", 700), "thread_ts": root},
			{"ts": "1789047800.000003", "bot_id": "B1", "subtype": "bot_message", "text": "pipeline passed", "thread_ts": root},
		},
		"1789047700.000002": {
			{"ts": "1789047700.000002", "user": "U200PEER", "text": "replied to findings", "thread_ts": root},
		},
	}
}

func TestFetchBuildsMessageLikeTheHelper(t *testing.T) {
	f := &fakeSlack{replies: thread(), channels: map[string]map[string]any{"C100HOME": {"name": "team-agents"}}, users: map[string]string{"U200PEER": "teammate", self: "Atlas"}}
	c := server(t, f)

	got, err := c.Fetch(context.Background(), "C100HOME", "1789047700.000002", "1789047527.174689")

	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.AuthorID != "U200PEER" || got.AuthorName != "teammate" || got.IsBot || got.ChannelLabel != "#team-agents" || got.IsDM {
		t.Errorf("message = %+v", got)
	}
	if !got.SelfInThread || got.ThreadSize != 4 || got.ThreadTS != "1789047527.174689" {
		t.Errorf("thread facts = self=%v size=%d root=%q", got.SelfInThread, got.ThreadSize, got.ThreadTS)
	}
	if len(got.ThreadTail) != 3 || got.ThreadTail[0].Text != "[HANDOFF] review" || got.ThreadTail[1].AuthorName != "Atlas" || got.ThreadTail[2].AuthorID != "B1" {
		t.Errorf("tail = %+v", got.ThreadTail)
	}
	if len([]rune(got.Text)) < 700 {
		t.Error("the target text must not be truncated")
	}
	top, err := c.Fetch(context.Background(), "C100HOME", "1789047527.174689", "")
	if err != nil || top.RootText != "" || top.ThreadTS != "" {
		t.Errorf("a top-level message carries no root: %+v err=%v", top, err)
	}
}

func TestFetchRerootsARepliedMessageAndMarksBots(t *testing.T) {
	f := &fakeSlack{replies: thread(), channels: map[string]map[string]any{"D100PEER": {"is_im": true, "user": "U200PEER"}}, users: map[string]string{"U200PEER": "teammate"}}
	c := server(t, f)

	got, err := c.Fetch(context.Background(), "D100PEER", "1789047700.000002", "")

	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.ThreadTS != "1789047527.174689" || got.ThreadSize != 4 || !got.IsDM || got.ChannelLabel != "DM with teammate" {
		t.Errorf("message = %+v", got)
	}
	if got.RootAuthor != "teammate" || got.RootText != "[HANDOFF] review" {
		t.Errorf("root = %q / %q", got.RootAuthor, got.RootText)
	}
	bot, err := c.Fetch(context.Background(), "D100PEER", "1789047800.000003", "1789047527.174689")
	if err != nil || !bot.IsBot || bot.AuthorID != "B1" || bot.Subtype != "bot_message" {
		t.Errorf("bot message = %+v err=%v", bot, err)
	}
	calls := strings.Join(f.calls, "\n")
	if !strings.Contains(calls, "ts=1789047700.000002") || !strings.Contains(calls, "ts=1789047527.174689") {
		t.Errorf("a reply must be looked up by its own ts and then by the thread root: %v", f.calls)
	}
}

func TestRepliesReturnsOnlyNewerReplies(t *testing.T) {
	f := &fakeSlack{replies: thread()}
	c := server(t, f)

	got, err := c.Replies(context.Background(), "C100HOME", "1789047527.174689", "1789047600.000001")

	if err != nil {
		t.Fatalf("Replies: %v", err)
	}
	if len(got) != 2 || got[0].TS != "1789047700.000002" || got[1].AuthorID != "B1" {
		t.Errorf("replies = %+v", got)
	}
	if !strings.Contains(f.calls[0], "oldest=1789047600.000001") || !strings.Contains(f.calls[0], "inclusive=false") {
		t.Errorf("call = %s", f.calls[0])
	}
}

func TestErrorsFromSlackAreReported(t *testing.T) {
	c := server(t, &fakeSlack{replies: thread()})
	if _, err := c.Fetch(context.Background(), "C1", "9.9", ""); err == nil || !strings.Contains(err.Error(), "thread_not_found") {
		t.Errorf("err = %v", err)
	}
	if _, err := c.Fetch(context.Background(), "C1", "9.9", "1789047527.174689"); err == nil || !strings.Contains(err.Error(), "not found in thread") {
		t.Errorf("err = %v", err)
	}
	limited := server(t, &fakeSlack{status: http.StatusTooManyRequests})
	if _, err := limited.Replies(context.Background(), "C1", "1.1", ""); err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("err = %v", err)
	}
	wrong := New("bad", self).WithBaseURL(server(t, &fakeSlack{replies: thread()}).base)
	if _, err := wrong.Replies(context.Background(), "C1", "1789047527.174689", ""); err == nil || !strings.Contains(err.Error(), "not_authed") {
		t.Errorf("err = %v", err)
	}
}

func TestLoadTokenPrefersEnvThenFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "slack_token")
	t.Setenv(EnvToken, "")
	if _, err := LoadToken(file, EnvToken); err == nil {
		t.Error("missing token must be an error")
	}
	if err := os.WriteFile(file, []byte(" xoxp-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadToken(file, EnvToken); err != nil || got != "xoxp-from-file" {
		t.Errorf("token = %q err = %v", got, err)
	}
	t.Setenv(EnvToken, "xoxp-from-env")
	if got, _ := LoadToken(file, EnvToken); got != "xoxp-from-env" {
		t.Errorf("env must win, got %q", got)
	}
}

func TestBrowserTokensSendTheSessionCookie(t *testing.T) {
	f := &fakeSlack{replies: thread()}
	c := server(t, f).WithCookie("abc")

	if _, err := c.Replies(context.Background(), "C1", "1789047527.174689", ""); err != nil {
		t.Fatal(err)
	}
	if f.cookies[0] != "d=xoxd-abc" {
		t.Errorf("cookie header = %q", f.cookies[0])
	}
	plain := server(t, f)
	plain.Replies(context.Background(), "C1", "1789047527.174689", "")
	if f.cookies[1] != "" {
		t.Errorf("api tokens must not send a cookie, got %q", f.cookies[1])
	}
	file := filepath.Join(t.TempDir(), "slack_cookie")
	t.Setenv(EnvCookie, "")
	if got := LoadCookie(file, EnvCookie); got != "" {
		t.Errorf("missing cookie file must yield empty, got %q", got)
	}
	os.WriteFile(file, []byte("xoxd-file\n"), 0o600)
	if got := LoadCookie(file, EnvCookie); got != "xoxd-file" {
		t.Errorf("cookie = %q", got)
	}
}

func TestAuthTestAndChannelLookup(t *testing.T) {
	f := &fakeSlack{}
	c := server(t, f)

	id, err := c.AuthTest(context.Background())
	if err != nil || id.UserID != self || id.Team != "Acme" || id.URL != "https://acme.slack.com/" {
		t.Errorf("identity = %+v err = %v", id, err)
	}
	got, err := c.FindChannel(context.Background(), "#Team-Agents")
	if err != nil || got != "C0AGENTS" {
		t.Errorf("channel = %q err = %v", got, err)
	}
	if missing, err := c.FindChannel(context.Background(), "nope"); err != nil || missing != "" {
		t.Errorf("missing channel = %q err = %v", missing, err)
	}
	if !strings.Contains(strings.Join(f.calls, "\n"), "cursor=p2") {
		t.Errorf("lookup must follow pagination: %v", f.calls)
	}
}

func TestPostSendsAFormEncodedChatMessageIntoTheThread(t *testing.T) {
	f := &fakeSlack{}
	c := server(t, f).WithCookie("xoxd-cookie")

	ts, err := c.Post(context.Background(), "C100HOME", "1789047527.174689", "🤖 Atlas: [TAKEN] x")

	if err != nil || ts != "1789050000.000123" {
		t.Fatalf("Post = %q, %v", ts, err)
	}
	if len(f.posted) != 1 || f.posted[0].Get("thread_ts") != "1789047527.174689" || f.posted[0].Get("text") != "🤖 Atlas: [TAKEN] x" {
		t.Errorf("form = %+v", f.posted)
	}
	if f.cookies[0] != "d=xoxd-cookie" {
		t.Errorf("cookie = %q", f.cookies[0])
	}
	if _, err := c.Post(context.Background(), "C100HOME", "", ""); err == nil || !strings.Contains(err.Error(), "no_text") {
		t.Errorf("API error must surface, got %v", err)
	}
}

func TestARefusalFromSlackSaysTheMessageWasNotAccepted(t *testing.T) {
	c := server(t, &fakeSlack{})

	_, err := c.Post(context.Background(), "C100HOME", "", "")

	if !errors.Is(err, delivery.ErrNotAccepted) {
		t.Errorf("err = %v, an api refusal means nothing reached the thread", err)
	}
}

func TestAFailedSlackGatewayLeavesAcceptanceOpen(t *testing.T) {
	c := server(t, &fakeSlack{status: http.StatusBadGateway})

	_, err := c.Post(context.Background(), "C100HOME", "1789047527.174689", "text")

	if err == nil {
		t.Fatal("Post must report the failure")
	}
	if errors.Is(err, delivery.ErrNotAccepted) {
		t.Errorf("err = %v, a gateway failure cannot promise the message was not posted", err)
	}
}

func TestRateLimitingSaysTheMessageWasNotAccepted(t *testing.T) {
	c := server(t, &fakeSlack{status: http.StatusTooManyRequests})

	_, err := c.Post(context.Background(), "C100HOME", "1789047527.174689", "text")

	if !errors.Is(err, delivery.ErrNotAccepted) {
		t.Errorf("err = %v, a rate-limited call is refused outright", err)
	}
}
