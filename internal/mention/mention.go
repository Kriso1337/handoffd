package mention

import (
	"regexp"
	"sort"
	"strings"

	"github.com/Kriso1337/handoffd/internal/mrref"
)

type Kind int

const (
	KindNone Kind = iota
	KindOther
	KindReview
	KindTriage
	KindHelp
	KindStop
)

func (k Kind) String() string {
	switch k {
	case KindReview:
		return "review"
	case KindOther:
		return "other"
	case KindTriage:
		return "triage"
	case KindHelp:
		return "help"
	case KindStop:
		return "stop"
	default:
		return "none"
	}
}

type Message struct {
	Channel      string
	ChannelLabel string
	TS           string
	ThreadTS     string
	AuthorID     string
	AuthorName   string
	Text         string
	IsDM         bool
	IsBot        bool
	Subtype      string
	SelfInThread bool
}

type Result struct {
	Kind         Kind
	Mentioned    bool
	Tagged       bool
	ReviewSignal bool
	MacroHit     bool
	Filtered     string
	Progress     string
	ProgressSHA  string
	Refs         mrref.Refs
}

const shortReplyWords = 3

var AckWordsEN = []string{
	"yes", "no", "ok", "okay", "k", "kk", "thanks", "thank", "you", "thx", "ty", "got", "it", "noted", "ack", "sure",
	"yep", "yup", "nope", "merging", "merged", "done", "great", "cool", "nice", "perfect", "awesome", "lol", "haha",
	"xd", "fine", "alright", "please", "pls", "plz", "hi", "hello", "hey", "bye", "good", "morning", "night", "evening",
	"i", "see", "will", "do", "on", "my", "way",
}

const (
	ProgressTaken       = "taken"
	ProgressReviewStart = "review_start"
	ProgressHeadChanged = "head_changed"
	ProgressReviewDone  = "review_done"
	ProgressDone        = "done"
)

var DefaultProgressMarkers = map[string]string{
	"[taken]":        ProgressTaken,
	"[review start]": ProgressReviewStart,
	"[head changed]": ProgressHeadChanged,
	"[review done]":  ProgressReviewDone,
	"[done]":         ProgressDone,
}

var (
	ReviewMarkersEN     = []string{"[handoff]", "[review"}
	ReviewHeadMarkersEN = []string{"[head changed]", "[review start]"}
	ReviewWordsEN       = []string{"review", "re-review", "take a look at the mr", "take a look at the pr"}
)

type Rules struct {
	MentionWords      []string
	TriageWords       []string
	MacroPhrases      []string
	StopPhrases       []string
	ReviewMarkers     []string
	ReviewHeadMarkers []string
	ReviewWords       []string
	AckWords          []string
	ProgressMarkers   map[string]string
}

var (
	systemSubtypes = map[string]bool{
		"bot_message": true, "channel_join": true, "channel_leave": true, "channel_topic": true,
		"channel_purpose": true, "channel_name": true, "channel_archive": true, "group_join": true,
		"group_leave": true, "pinned_item": true, "unpinned_item": true, "reminder_add": true,
		"tombstone": true, "joiner_notification": true, "sh_room_created": true, "huddle_thread": true,
	}
	slackMarkup = regexp.MustCompile(`<[^>]*>|:[a-z0-9_+-]+:`)
	urlPattern  = regexp.MustCompile(`https?://`)
	wordTrim    = regexp.MustCompile(`^[^\p{L}\p{N}+]+|[^\p{L}\p{N}+]+$`)
	hasLetter   = regexp.MustCompile(`\p{L}`)
	markerNoise = regexp.MustCompile(`^(?:\s|:[a-z0-9_+-]+:|[^\p{L}\p{N}\[])+`)
	namePrefix  = regexp.MustCompile(`^[\p{L}\p{N}_ .-]{1,40}:\s*`)
)

type Classifier struct {
	selfUserID   string
	homeChannel  string
	mentionWords []string
	triageWords  []string
	macroPhrases []string
	stopPhrases  []string
	markers      []string
	headMarkers  []string
	reviewWords  []string
	ackWords     map[string]bool
	progress     []progressMarker
}

type progressMarker struct {
	marker string
	phase  string
}

func New(selfUserID, homeChannel string, rules Rules) Classifier {
	ack := make(map[string]bool, len(rules.AckWords))
	for _, w := range lower(rules.AckWords) {
		ack[w] = true
	}
	return Classifier{
		selfUserID:   selfUserID,
		homeChannel:  homeChannel,
		mentionWords: lower(rules.MentionWords),
		triageWords:  lower(rules.TriageWords),
		macroPhrases: lower(rules.MacroPhrases),
		stopPhrases:  lower(rules.StopPhrases),
		markers:      lower(rules.ReviewMarkers),
		headMarkers:  lower(rules.ReviewHeadMarkers),
		reviewWords:  lower(rules.ReviewWords),
		ackWords:     ack,
		progress:     progressMarkers(rules.ProgressMarkers),
	}
}

func progressMarkers(markers map[string]string) []progressMarker {
	out := make([]progressMarker, 0, len(markers))
	for marker, phase := range markers {
		if marker = strings.ToLower(strings.TrimSpace(marker)); marker != "" && phase != "" {
			out = append(out, progressMarker{marker: marker, phase: phase})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].marker) != len(out[j].marker) {
			return len(out[i].marker) > len(out[j].marker)
		}
		return out[i].marker < out[j].marker
	})
	return out
}

func (c Classifier) Classify(m Message, threadIsReview bool) Result {
	lowered := strings.ToLower(m.Text)
	res := Result{
		MacroHit: containsAny(lowered, c.macroPhrases),
		Tagged:   c.tagged(m.Text),
		Refs:     mrref.Extract(m.Text),
	}

	if m.IsBot || systemSubtypes[m.Subtype] {
		return Result{Filtered: "bot or system message"}
	}
	if containsAny(lowered, c.stopPhrases) {
		return Result{Kind: KindStop, Mentioned: true}
	}
	if m.AuthorID == c.selfUserID && !res.MacroHit {
		marker, phase := c.progressMarker(lowered)
		sha := mrref.LineSHA(m.Text, marker)
		if sha == "" {
			sha = res.Refs.SHA
		}
		return Result{Progress: phase, ProgressSHA: sha, Refs: res.Refs}
	}
	if res.MacroHit {
		res.Kind = KindHelp
		res.Mentioned = true
		return res
	}

	if m.Channel == c.homeChannel {
		res.Mentioned = res.Tagged || containsAny(lowered, c.mentionWords)
		res.ReviewSignal = c.reviewSignal(lowered, res.Refs)
		switch {
		case threadIsReview:
			res.Kind = KindReview
		case !res.Mentioned:
			res.Kind = KindNone
		case res.ReviewSignal:
			res.Kind = KindReview
		default:
			res.Kind = KindOther
		}
		return res
	}

	tagged := res.Tagged
	res.Mentioned = tagged || containsAny(lowered, c.triageWords)
	res.ReviewSignal = c.reviewSignal(lowered, res.Refs)

	switch {
	case tagged && containsAny(lowered, c.reviewWords):
		res.Kind = KindReview
		res.ReviewSignal = true
	case res.Mentioned:
		res.Kind = KindTriage
	case c.Acknowledgement(m.Text):
		res.Filtered = "acknowledgement without link or question"
	case m.IsDM || m.SelfInThread:
		res.Kind = KindTriage
	}
	return res
}

func (c Classifier) Acknowledgement(text string) bool {
	if Pointed(text) {
		return false
	}
	words := strings.Fields(slackMarkup.ReplaceAllString(strings.ToLower(text), " "))
	if len(words) > shortReplyWords {
		return false
	}
	for _, w := range words {
		w = wordTrim.ReplaceAllString(w, "")
		if w != "" && !c.ackWords[w] && hasLetter.MatchString(w) {
			return false
		}
	}
	return true
}

func (c Classifier) Progress(lowered string) string {
	_, phase := c.progressMarker(lowered)
	return phase
}

func (c Classifier) progressMarker(lowered string) (string, string) {
	head := leadingMarker(lowered)
	if head == "" {
		return "", ""
	}
	for _, pm := range c.progress {
		if strings.HasPrefix(head, pm.marker) {
			return pm.marker, pm.phase
		}
	}
	return "", ""
}

func leadingMarker(lowered string) string {
	line, _, _ := strings.Cut(lowered, "\n")
	for attempt := 0; attempt < 3; attempt++ {
		line = markerNoise.ReplaceAllString(line, "")
		if strings.HasPrefix(line, "[") {
			return line
		}
		stripped := namePrefix.ReplaceAllString(line, "")
		if stripped == line {
			return ""
		}
		line = stripped
	}
	return ""
}

func Pointed(text string) bool {
	return urlPattern.MatchString(text) || strings.Contains(text, "?")
}

func (c Classifier) tagged(text string) bool {
	return strings.Contains(text, "<@"+c.selfUserID+">")
}

func (c Classifier) reviewSignal(lowered string, refs mrref.Refs) bool {
	if containsAny(lowered, c.headMarkers) {
		return true
	}
	if len(refs.MRs) == 0 {
		return false
	}
	return containsAny(lowered, c.markers) || containsAny(lowered, c.reviewWords)
}

func containsAny(lowered string, needles []string) bool {
	for _, n := range needles {
		if n != "" && strings.Contains(lowered, n) {
			return true
		}
	}
	return false
}

func lower(words []string) []string {
	out := make([]string, 0, len(words))
	for _, w := range words {
		if w = strings.ToLower(strings.TrimSpace(w)); w != "" {
			out = append(out, w)
		}
	}
	return out
}
