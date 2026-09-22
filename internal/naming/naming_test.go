package naming

import (
	"strings"
	"testing"
	"time"

	"github.com/Kriso1337/handoffd/internal/mention"
	"github.com/Kriso1337/handoffd/internal/mrref"
)

var at = time.Date(2026, 9, 10, 13, 38, 0, 0, time.UTC)

func TestReviewPrefersJiraKey(t *testing.T) {
	in := Input{
		Kind: mention.KindReview,
		Refs: mrref.Refs{
			JiraKeys: []string{"PRJ-8866"},
			MRs:      []mrref.MR{{Project: "service-a", IID: 899}},
		},
		At: at,
	}

	if got := Window(in, nil); got != "rev/PRJ-8866" {
		t.Errorf("name = %q", got)
	}
}

func TestReviewFallsBackToMergeRequestSlug(t *testing.T) {
	in := Input{
		Kind: mention.KindReview,
		Refs: mrref.Refs{MRs: []mrref.MR{{Project: "service-a", IID: 899}}},
		At:   at,
	}

	if got := Window(in, nil); got != "rev/service-a!899" {
		t.Errorf("name = %q", got)
	}
}

func TestOtherUsesAuthorAndTime(t *testing.T) {
	in := Input{Kind: mention.KindOther, Author: "peer", At: at}

	if got := Window(in, nil); got != "sky/peer-1338" {
		t.Errorf("name = %q", got)
	}
}

func TestTriageUsesChannelName(t *testing.T) {
	in := Input{Kind: mention.KindTriage, ChannelLabel: "#team-a-dev", Author: "teammate-a", At: at}

	if got := Window(in, nil); got != "tri/team-a-dev-1338" {
		t.Errorf("name = %q", got)
	}
}

func TestTriageOfDirectMessageUsesAuthor(t *testing.T) {
	in := Input{Kind: mention.KindTriage, ChannelLabel: "DM with Alex Twin", IsDM: true, Author: "alex", At: at}

	if got := Window(in, nil); got != "tri/alex-1338" {
		t.Errorf("name = %q", got)
	}
}

func TestHelpMacroGetsOwnPrefix(t *testing.T) {
	in := Input{Kind: mention.KindHelp, ChannelLabel: "#team-a-dev", At: at}

	got := Window(in, nil)

	if !strings.HasPrefix(got, "help/") {
		t.Errorf("name = %q, want help/ prefix", got)
	}
}

func TestNameNeverContainsTmuxTargetSeparators(t *testing.T) {
	names := []string{
		Window(Input{Kind: mention.KindTriage, ChannelLabel: "#dev.traffic:main", At: at}, nil),
		Window(Input{Kind: mention.KindOther, Author: "self.name:agent", At: at}, nil),
	}

	for _, name := range names {
		if strings.ContainsAny(name, ":.") {
			t.Errorf("name %q must not contain ':' or '.'", name)
		}
	}
}

func TestNameIsCappedInLength(t *testing.T) {
	in := Input{Kind: mention.KindTriage, ChannelLabel: "#very-long-channel-name-for-testing", At: at}

	if got := Window(in, nil); len([]rune(got)) > maxLength {
		t.Errorf("name %q is %d runes", got, len([]rune(got)))
	}
}

func TestCollisionGetsNumericSuffix(t *testing.T) {
	in := Input{Kind: mention.KindReview, Refs: mrref.Refs{JiraKeys: []string{"PRJ-8866"}}, At: at}
	existing := map[string]bool{"rev/PRJ-8866": true, "rev/PRJ-8866-2": true}

	got := Window(in, func(name string) bool { return existing[name] })

	if got != "rev/PRJ-8866-3" {
		t.Errorf("name = %q", got)
	}
}

func TestSuffixKeepsNameWithinLimit(t *testing.T) {
	in := Input{Kind: mention.KindReview, Refs: mrref.Refs{JiraKeys: []string{"PRJLONGKEY-1234567"}}, At: at}
	taken := func(name string) bool { return name == "rev/PRJLONGKEY-12345" }

	got := Window(in, taken)

	if len([]rune(got)) > maxLength {
		t.Errorf("name %q is %d runes", got, len([]rune(got)))
	}
	if !strings.HasSuffix(got, "-2") {
		t.Errorf("name = %q, want numeric suffix", got)
	}
}

func TestTagMarksCommitteeMemberWindows(t *testing.T) {
	in := Input{Kind: mention.KindReview, Refs: mrref.Refs{JiraKeys: []string{"PRJ-8866"}}, At: at, Tag: "codex"}

	if got := Window(in, nil); got != "rev/PRJ-8866~codex" {
		t.Errorf("name = %q", got)
	}
	long := Input{Kind: mention.KindReview, Refs: mrref.Refs{JiraKeys: []string{"PROJECTX-123456"}}, At: at, Tag: "codex"}
	if got := Window(long, nil); len([]rune(got)) > maxLength || !strings.HasSuffix(got, "~codex") {
		t.Errorf("tagged name = %q must stay within %d runes and keep the tag", got, maxLength)
	}
}

func TestALongTagDoesNotBlowUpTheWindowName(t *testing.T) {
	in := Input{Kind: mention.KindReview, Refs: mrref.Extract("https://gitlab.example.com/team-a/service-a/-/merge_requests/899"),
		At: time.Date(2026, 9, 15, 18, 30, 0, 0, time.UTC), Tag: strings.Repeat("deepseek", 4)}

	name := Window(in, nil)

	if len([]rune(name)) > maxLength {
		t.Errorf("name = %q, longer than the limit", name)
	}
	if !strings.Contains(name, "~") {
		t.Errorf("name = %q, want the tag kept", name)
	}
}
