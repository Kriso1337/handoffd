package slackfetch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Kriso1337/handoffd/internal/delivery"
)

type Commander interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type Reply struct {
	TS         string `json:"ts"`
	AuthorID   string `json:"author_id"`
	AuthorName string `json:"author_name"`
	Text       string `json:"text"`
}

type Message struct {
	AuthorID     string  `json:"author_id"`
	AuthorName   string  `json:"author_name"`
	IsBot        bool    `json:"is_bot"`
	Subtype      string  `json:"subtype"`
	Text         string  `json:"text"`
	ThreadTS     string  `json:"thread_ts"`
	ChannelLabel string  `json:"channel_label"`
	IsDM         bool    `json:"is_dm"`
	SelfInThread bool    `json:"self_in_thread"`
	ThreadSize   int     `json:"thread_size"`
	ThreadTail   []Reply `json:"thread_tail"`
	RootAuthor   string  `json:"root_author_name"`
	RootText     string  `json:"root_text"`
	Error        string  `json:"error"`
}

type Fetcher struct {
	cmd    Commander
	helper []string
}

func New(cmd Commander, helper []string) Fetcher {
	return Fetcher{cmd: cmd, helper: helper}
}

func (f Fetcher) Fetch(ctx context.Context, channel, ts, threadTS string) (Message, error) {
	if len(f.helper) == 0 {
		return Message{}, fmt.Errorf("fetch %s/%s: helper is not configured", channel, ts)
	}

	args := append(append([]string{}, f.helper[1:]...), channel, ts, threadTS)
	out, err := f.cmd.Run(ctx, f.helper[0], args...)
	if err != nil {
		return Message{}, fmt.Errorf("fetch %s/%s: %w: %s", channel, ts, err, strings.TrimSpace(string(out)))
	}

	var msg Message
	if err := json.Unmarshal(out, &msg); err != nil {
		return Message{}, fmt.Errorf("fetch %s/%s: parse helper output: %w: %s", channel, ts, err, strings.TrimSpace(string(out)))
	}
	if msg.Error != "" {
		return Message{}, fmt.Errorf("fetch %s/%s: %s", channel, ts, msg.Error)
	}
	if msg.AuthorID == "" {
		return Message{}, fmt.Errorf("fetch %s/%s: helper returned no author", channel, ts)
	}
	return msg, nil
}

type replyList struct {
	Replies []Reply `json:"replies"`
	Error   string  `json:"error"`
}

func (f Fetcher) Replies(ctx context.Context, channel, threadTS, since string) ([]Reply, error) {
	if len(f.helper) == 0 {
		return nil, fmt.Errorf("replies %s/%s: helper is not configured", channel, threadTS)
	}
	args := append(append([]string{}, f.helper[1:]...), "--replies", channel, threadTS, since)
	out, err := f.cmd.Run(ctx, f.helper[0], args...)
	if err != nil {
		return nil, fmt.Errorf("replies %s/%s: %w: %s", channel, threadTS, err, strings.TrimSpace(string(out)))
	}
	var list replyList
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("replies %s/%s: parse helper output: %w: %s", channel, threadTS, err, strings.TrimSpace(string(out)))
	}
	if list.Error != "" {
		return nil, fmt.Errorf("replies %s/%s: %s", channel, threadTS, list.Error)
	}
	return list.Replies, nil
}

type posted struct {
	TS    string `json:"ts"`
	Error string `json:"error"`
}

func (f Fetcher) Post(ctx context.Context, channel, threadTS, text string) (string, error) {
	if len(f.helper) == 0 {
		return "", fmt.Errorf("post %s/%s: helper is not configured", channel, threadTS)
	}
	args := append(append([]string{}, f.helper[1:]...), "--post", channel, threadTS, text)
	out, err := f.cmd.Run(ctx, f.helper[0], args...)
	if err != nil {
		return "", fmt.Errorf("post %s/%s: %w: %s", channel, threadTS, err, strings.TrimSpace(string(out)))
	}
	var res posted
	if err := json.Unmarshal(out, &res); err != nil {
		return "", fmt.Errorf("post %s/%s: parse helper output: %w: %s", channel, threadTS, err, strings.TrimSpace(string(out)))
	}
	if res.Error != "" {
		return "", fmt.Errorf("post %s/%s: %w: %s", channel, threadTS, delivery.ErrNotAccepted, res.Error)
	}
	if res.TS == "" {
		return "", fmt.Errorf("post %s/%s: helper returned no ts", channel, threadTS)
	}
	return res.TS, nil
}
