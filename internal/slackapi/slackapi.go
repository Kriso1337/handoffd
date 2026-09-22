package slackapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Kriso1337/handoffd/internal/delivery"
	"github.com/Kriso1337/handoffd/internal/slackfetch"
)

const (
	EnvToken    = "HANDOFFD_SLACK_TOKEN"
	EnvCookie   = "HANDOFFD_SLACK_COOKIE"
	DefaultBase = "https://slack.com/api/"

	tailLimit   = 12
	textLimit   = 600
	pageLimit   = 1000
	httpTimeout = 20 * time.Second
)

func LoadToken(file, env string) (string, error) {
	if token := strings.TrimSpace(os.Getenv(env)); token != "" {
		return token, nil
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("slack token: set %s or put the token into %s: %w", env, file, err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("slack token file %s is empty", file)
	}
	return token, nil
}

func LoadCookie(file, env string) string {
	if cookie := strings.TrimSpace(os.Getenv(env)); cookie != "" {
		return cookie
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

type Client struct {
	http   *http.Client
	base   string
	token  string
	cookie string
	selfID string
}

func New(token, selfID string) *Client {
	return &Client{http: &http.Client{Timeout: httpTimeout}, base: DefaultBase, token: token, selfID: selfID}
}

func (c *Client) WithCookie(cookie string) *Client {
	cookie = strings.TrimSpace(cookie)
	if cookie != "" && !strings.HasPrefix(cookie, "xoxd-") {
		cookie = "xoxd-" + cookie
	}
	c.cookie = cookie
	return c
}

func (c *Client) WithBaseURL(base string) *Client {
	c.base = strings.TrimSuffix(base, "/") + "/"
	return c
}

type envelope struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

type message struct {
	TS       string `json:"ts"`
	User     string `json:"user"`
	BotID    string `json:"bot_id"`
	Subtype  string `json:"subtype"`
	Text     string `json:"text"`
	ThreadTS string `json:"thread_ts"`
}

func (m message) author() string {
	if m.User != "" {
		return m.User
	}
	return m.BotID
}

type repliesResponse struct {
	envelope
	Messages []message `json:"messages"`
}

type channelResponse struct {
	envelope
	Channel struct {
		IsIM   bool   `json:"is_im"`
		IsMPIM bool   `json:"is_mpim"`
		User   string `json:"user"`
		Name   string `json:"name"`
	} `json:"channel"`
}

type userResponse struct {
	envelope
	User struct {
		Profile struct {
			DisplayName string `json:"display_name"`
			RealName    string `json:"real_name"`
		} `json:"profile"`
	} `json:"user"`
}

type Identity struct {
	UserID string
	User   string
	TeamID string
	Team   string
	URL    string
}

type authResponse struct {
	envelope
	UserID string `json:"user_id"`
	User   string `json:"user"`
	TeamID string `json:"team_id"`
	Team   string `json:"team"`
	URL    string `json:"url"`
}

func (c *Client) AuthTest(ctx context.Context) (Identity, error) {
	var resp authResponse
	if err := c.call(ctx, "auth.test", url.Values{}, &resp); err != nil {
		return Identity{}, err
	}
	return Identity{UserID: resp.UserID, User: resp.User, TeamID: resp.TeamID, Team: resp.Team, URL: resp.URL}, nil
}

type channelListResponse struct {
	envelope
	Channels []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"channels"`
	Metadata struct {
		NextCursor string `json:"next_cursor"`
	} `json:"response_metadata"`
}

func (c *Client) FindChannel(ctx context.Context, name string) (string, error) {
	name = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(name)), "#")
	cursor := ""
	for page := 0; page < 20; page++ {
		params := url.Values{"types": {"public_channel,private_channel"}, "exclude_archived": {"true"}, "limit": {"1000"}}
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var resp channelListResponse
		if err := c.call(ctx, "conversations.list", params, &resp); err != nil {
			return "", err
		}
		for _, ch := range resp.Channels {
			if strings.ToLower(ch.Name) == name {
				return ch.ID, nil
			}
		}
		cursor = resp.Metadata.NextCursor
		if cursor == "" {
			break
		}
	}
	return "", nil
}

func (c *Client) Fetch(ctx context.Context, channel, ts, threadTS string) (slackfetch.Message, error) {
	root := threadTS
	if root == "" {
		root = ts
	}
	messages, err := c.replies(ctx, channel, root, "")
	if err != nil {
		return slackfetch.Message{}, fmt.Errorf("fetch %s/%s: %w", channel, ts, err)
	}
	target, ok := find(messages, ts)
	if !ok {
		return slackfetch.Message{}, fmt.Errorf("fetch %s/%s: message not found in thread %s", channel, ts, root)
	}
	if target.ThreadTS != "" && target.ThreadTS != root {
		root = target.ThreadTS
		if messages, err = c.replies(ctx, channel, root, ""); err != nil {
			return slackfetch.Message{}, fmt.Errorf("fetch %s/%s: %w", channel, ts, err)
		}
	}

	names := map[string]string{}
	msg := slackfetch.Message{
		AuthorID:   target.author(),
		AuthorName: c.displayName(ctx, names, target.User),
		IsBot:      target.BotID != "" || target.Subtype == "bot_message" || target.User == "",
		Subtype:    target.Subtype,
		Text:       target.Text,
		IsDM:       strings.HasPrefix(channel, "D"),
		ThreadSize: len(messages),
	}
	if root != ts {
		msg.ThreadTS = root
		if len(messages) > 0 && messages[0].TS == root {
			msg.RootAuthor = c.displayName(ctx, names, messages[0].User)
			msg.RootText = truncate(messages[0].Text, textLimit)
		}
	}
	msg.ChannelLabel = c.channelLabel(ctx, names, channel)
	start := max(0, len(messages)-tailLimit)
	for _, m := range messages {
		if m.TS != ts && m.User == c.selfID {
			msg.SelfInThread = true
		}
	}
	for _, m := range messages[start:] {
		if m.TS == ts {
			continue
		}
		msg.ThreadTail = append(msg.ThreadTail, slackfetch.Reply{
			TS: m.TS, AuthorID: m.author(), AuthorName: c.displayName(ctx, names, m.User), Text: truncate(m.Text, textLimit),
		})
	}
	return msg, nil
}

func (c *Client) Replies(ctx context.Context, channel, threadTS, since string) ([]slackfetch.Reply, error) {
	messages, err := c.replies(ctx, channel, threadTS, since)
	if err != nil {
		return nil, fmt.Errorf("replies %s/%s: %w", channel, threadTS, err)
	}
	var out []slackfetch.Reply
	for _, m := range messages {
		if m.TS == "" || m.TS == threadTS || !after(m.TS, since) {
			continue
		}
		out = append(out, slackfetch.Reply{TS: m.TS, AuthorID: m.author()})
	}
	return out, nil
}

func (c *Client) replies(ctx context.Context, channel, root, since string) ([]message, error) {
	params := url.Values{"channel": {channel}, "ts": {root}, "limit": {strconv.Itoa(pageLimit)}}
	if since != "" {
		params.Set("oldest", since)
		params.Set("inclusive", "false")
	}
	var resp repliesResponse
	if err := c.call(ctx, "conversations.replies", params, &resp); err != nil {
		return nil, err
	}
	return resp.Messages, nil
}

func (c *Client) channelLabel(ctx context.Context, names map[string]string, channel string) string {
	var resp channelResponse
	if err := c.call(ctx, "conversations.info", url.Values{"channel": {channel}}, &resp); err != nil {
		return channel
	}
	switch {
	case resp.Channel.IsIM:
		name := c.displayName(ctx, names, resp.Channel.User)
		if name == "" {
			name = resp.Channel.User
		}
		return "DM with " + name
	case resp.Channel.IsMPIM:
		return "group DM " + firstNonEmpty(resp.Channel.Name, channel)
	default:
		return "#" + firstNonEmpty(resp.Channel.Name, channel)
	}
}

func (c *Client) displayName(ctx context.Context, cache map[string]string, userID string) string {
	if !strings.HasPrefix(userID, "U") && !strings.HasPrefix(userID, "W") {
		return ""
	}
	if name, ok := cache[userID]; ok {
		return name
	}
	var resp userResponse
	name := ""
	if err := c.call(ctx, "users.info", url.Values{"user": {userID}}, &resp); err == nil {
		name = firstNonEmpty(resp.User.Profile.DisplayName, resp.User.Profile.RealName)
	}
	cache[userID] = name
	return name
}

type postResponse struct {
	envelope
	TS string `json:"ts"`
}

func (c *Client) Post(ctx context.Context, channel, threadTS, text string) (string, error) {
	params := url.Values{"channel": {channel}, "text": {text}}
	if threadTS != "" {
		params.Set("thread_ts", threadTS)
	}
	var resp postResponse
	if err := c.send(ctx, http.MethodPost, "chat.postMessage", params, &resp); err != nil {
		return "", err
	}
	if resp.TS == "" {
		return "", errors.New("chat.postMessage: no ts in response")
	}
	return resp.TS, nil
}

func (c *Client) call(ctx context.Context, method string, params url.Values, out interface{ status() (bool, string) }) error {
	return c.send(ctx, http.MethodGet, method, params, out)
}

func (c *Client) send(ctx context.Context, verb, method string, params url.Values, out interface{ status() (bool, string) }) error {
	var req *http.Request
	var err error
	if verb == http.MethodPost {
		req, err = http.NewRequestWithContext(ctx, verb, c.base+method, strings.NewReader(params.Encode()))
		if req != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	} else {
		req, err = http.NewRequestWithContext(ctx, verb, c.base+method+"?"+params.Encode(), nil)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if c.cookie != "" {
		req.Header.Set("Cookie", "d="+c.cookie)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("%s: read: %w", method, err)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("%s: %w: rate limited, retry after %ss", method, delivery.ErrNotAccepted, resp.Header.Get("Retry-After"))
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusRequestTimeout {
		return fmt.Errorf("%s: %w: HTTP %d: %s", method, delivery.ErrNotAccepted, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d: %s", method, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: parse: %w", method, err)
	}
	if ok, apiErr := out.status(); !ok {
		if apiErr == "" {
			apiErr = "ok=false"
		}
		return fmt.Errorf("%s: %w: %s", method, delivery.ErrNotAccepted, apiErr)
	}
	return nil
}

func (e envelope) status() (bool, string) {
	return e.OK, e.Error
}

func find(messages []message, ts string) (message, bool) {
	for _, m := range messages {
		if m.TS == ts {
			return m, true
		}
	}
	return message{}, false
}

func after(ts, than string) bool {
	if than == "" {
		return true
	}
	a, errA := strconv.ParseFloat(ts, 64)
	b, errB := strconv.ParseFloat(than, 64)
	if errA != nil || errB != nil {
		return ts > than
	}
	return a > b
}

func truncate(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
