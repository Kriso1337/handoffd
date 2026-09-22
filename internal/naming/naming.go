package naming

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Kriso1337/handoffd/internal/mention"
	"github.com/Kriso1337/handoffd/internal/mrref"
)

const maxLength = 20

type Input struct {
	Kind         mention.Kind
	Refs         mrref.Refs
	Author       string
	ChannelLabel string
	IsDM         bool
	At           time.Time
	Tag          string
}

func Window(in Input, taken func(string) bool) string {
	base := sanitize(stem(in))
	if in.Tag != "" {
		tag := trim(sanitize(in.Tag), maxLength/2)
		base = trim(base, maxLength-len([]rune(tag))-1) + "~" + tag
	}
	if taken == nil || !taken(base) {
		return base
	}
	for suffix := 2; suffix < 100; suffix++ {
		candidate := trim(base, maxLength-len(strconv.Itoa(suffix))-1) + "-" + strconv.Itoa(suffix)
		if !taken(candidate) {
			return candidate
		}
	}
	return base + "-x"
}

func stem(in Input) string {
	clock := in.At.Format("1504")
	switch in.Kind {
	case mention.KindReview:
		if len(in.Refs.JiraKeys) > 0 {
			return "rev/" + in.Refs.JiraKeys[0]
		}
		if mr, ok := in.Refs.Primary(); ok {
			return "rev/" + mr.Slug()
		}
		return "rev/" + clock
	case mention.KindTriage:
		return "tri/" + label(in, clock)
	case mention.KindHelp:
		return "help/" + label(in, clock)
	default:
		if in.Author != "" {
			return fmt.Sprintf("sky/%s-%s", in.Author, clock)
		}
		return "sky/" + clock
	}
}

func label(in Input, clock string) string {
	source := strings.TrimPrefix(in.ChannelLabel, "#")
	if in.IsDM || source == "" {
		source = in.Author
	}
	if source == "" {
		return clock
	}
	return source + "-" + clock
}

func sanitize(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '/' || r == '!' || r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return trim(b.String(), maxLength)
}

func trim(name string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(name)
	if len(runes) <= limit {
		return name
	}
	return string(runes[:limit])
}
