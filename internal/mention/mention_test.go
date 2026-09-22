package mention

import "testing"

const (
	selfID = "U100SELF"
	home   = "C100HOME"
	peerID = "U200PEER"
	twinID = "U300TWIN"
	other  = "C200TEAM"
)

func rules() Rules {
	return Rules{
		MentionWords:      []string{"atlas"},
		TriageWords:       []string{"atlas", "alex", "sam"},
		MacroPhrases:      []string{"atlas, help", "atlas help", "atlas, help", "atlas help"},
		StopPhrases:       []string{"atlas, stop", "atlas stop", "atlas, stop", "atlas stop"},
		ReviewMarkers:     ReviewMarkersEN,
		ReviewHeadMarkers: ReviewHeadMarkersEN,
		ReviewWords:       ReviewWordsEN,
		AckWords:          AckWordsEN,
		ProgressMarkers:   DefaultProgressMarkers,
	}
}

func classifier() Classifier {
	return New(selfID, home, rules())
}

func TestHomeChannelHandoffIsReview(t *testing.T) {
	m := Message{
		Channel:  home,
		AuthorID: peerID,
		Text: "[HANDOFF] <@U100SELF>, take this review " +
			"https://gitlab.example.com/team-a/service-a/-/merge_requests/899",
	}

	if got := classifier().Classify(m, false); got.Kind != KindReview {
		t.Errorf("kind = %s, want review", got.Kind)
	}
}

func TestHomeChannelIgnoresIvanByName(t *testing.T) {
	m := Message{Channel: home, AuthorID: peerID, Text: "Alex asked for this to be handed off after CI"}

	if got := classifier().Classify(m, false); got.Kind != KindNone {
		t.Errorf("kind = %s: the owner name in the home channel must not open a window", got.Kind)
	}
}

func TestOwnMessageIsIgnored(t *testing.T) {
	m := Message{Channel: home, AuthorID: selfID, Text: ":robot_face: Atlas: [REVIEW DONE] abc"}

	if got := classifier().Classify(m, true); got.Kind != KindNone {
		t.Errorf("kind = %s, want none", got.Kind)
	}
}

func TestNameInForeignChannelGoesToTriage(t *testing.T) {
	m := Message{
		Channel:      other,
		ChannelLabel: "#team-a-dev",
		AuthorID:     "U400PEER",
		Text:         "Alex, check both, when you have time",
	}

	got := classifier().Classify(m, false)

	if got.Kind != KindTriage {
		t.Errorf("kind = %s, want triage", got.Kind)
	}
	if !got.Mentioned {
		t.Error("addressing by name must count as a mention")
	}
}

func TestThreadParticipationGoesToTriageWithoutName(t *testing.T) {
	m := Message{
		Channel:      other,
		AuthorID:     "U500PEER",
		Text:         "check too please https://gitlab.example.com/team-c/service-c/-/merge_requests/3",
		SelfInThread: true,
	}

	got := classifier().Classify(m, false)

	if got.Kind != KindTriage {
		t.Errorf("kind = %s, want triage", got.Kind)
	}
	if got.Mentioned {
		t.Error("no name mention here, only thread participation")
	}
}

func TestForeignChannelChatterIsDropped(t *testing.T) {
	m := Message{Channel: other, AuthorID: "U500PEER", Text: "great"}

	if got := classifier().Classify(m, false); got.Kind != KindNone {
		t.Errorf("kind = %s, want none", got.Kind)
	}
}

func TestDirectMessageGoesToTriageWithoutAnyName(t *testing.T) {
	m := Message{Channel: "D100DM", AuthorID: twinID, Text: "check please MR 2241", IsDM: true}

	if got := classifier().Classify(m, false); got.Kind != KindTriage {
		t.Errorf("kind = %s, want triage", got.Kind)
	}
}

func TestGreetingOnlyDirectMessageWaitsForTheActualAsk(t *testing.T) {
	m := Message{Channel: "D100DM", AuthorID: twinID, Text: "hello", IsDM: true}

	if got := classifier().Classify(m, false); got.Kind != KindNone || got.Filtered == "" {
		t.Errorf("kind = %s filtered = %q, a bare greeting is not work yet", got.Kind, got.Filtered)
	}
}

func TestMacroSkipsTriage(t *testing.T) {
	m := Message{Channel: other, AuthorID: twinID, Text: "Atlas, help — investigate 429 in service-c"}

	got := classifier().Classify(m, false)

	if got.Kind != KindHelp {
		t.Errorf("kind = %s, want help", got.Kind)
	}
	if !got.MacroHit {
		t.Error("macro must be detected")
	}
}

func TestMacroFromSelfWorks(t *testing.T) {
	m := Message{Channel: "D0SELF", AuthorID: selfID, IsDM: true, Text: "atlas help fetch yesterday spend"}

	got := classifier().Classify(m, false)

	if got.Kind != KindHelp {
		t.Errorf("kind = %s: own macro in a direct message must work", got.Kind)
	}
}

func TestMacroIsCaseInsensitiveAndCommaOptional(t *testing.T) {
	for _, text := range []string{"ATLAS, HELP", "atlas help", "Atlas, help"} {
		m := Message{Channel: other, AuthorID: twinID, Text: text}
		if got := classifier().Classify(m, false); got.Kind != KindHelp {
			t.Errorf("text %q → kind %s, want help", text, got.Kind)
		}
	}
}

func TestConfiguredUnicodeMessageRulesRemainSupported(t *testing.T) {
	r := Rules{
		MentionWords: []string{"\u0430\u0442\u043b\u0430\u0441"},
		MacroPhrases: []string{"\u0430\u0442\u043b\u0430\u0441, \u043f\u043e\u043c\u043e\u0433\u0438"},
		AckWords:     []string{"\u0441\u043f\u0430\u0441\u0438\u0431\u043e"},
	}
	c := New(selfID, home, r)

	help := c.Classify(Message{
		Channel:  other,
		AuthorID: twinID,
		Text:     "\u0410\u0442\u043b\u0430\u0441, \u043f\u043e\u043c\u043e\u0433\u0438 \u043f\u0440\u043e\u0432\u0435\u0440\u0438\u0442\u044c \u043b\u043e\u0433\u0438",
	}, false)
	if help.Kind != KindHelp {
		t.Errorf("configured Unicode macro classified as %s, want help", help.Kind)
	}

	ack := c.Classify(Message{
		Channel:      other,
		AuthorID:     twinID,
		Text:         "\u0421\u043f\u0430\u0441\u0438\u0431\u043e!",
		SelfInThread: true,
	}, false)
	if ack.Kind != KindNone || ack.Filtered == "" {
		t.Errorf("configured Unicode acknowledgement classified as %s", ack.Kind)
	}
}

func TestReviewThreadContinuationStillWorks(t *testing.T) {
	m := Message{Channel: home, AuthorID: peerID, Text: "[HEAD CHANGED] 08c9fc23 → 4a8259ce0aa1b2c3"}

	got := classifier().Classify(m, true)

	if got.Kind != KindReview {
		t.Errorf("kind = %s, want review", got.Kind)
	}
	if !got.ReviewSignal {
		t.Error("HEAD CHANGED must be a review signal")
	}
}

func TestTaggedReviewFollowUpSkipsTriage(t *testing.T) {
	m := Message{
		Channel:      other,
		ChannelLabel: "#team-a-dev",
		AuthorID:     "U400PEER",
		Text:         "<@U100SELF> replied to your review",
		SelfInThread: true,
	}

	got := classifier().Classify(m, false)

	if got.Kind != KindReview {
		t.Errorf("kind = %s: a tagged reply to a review is review work, not a reason to ask Haiku", got.Kind)
	}
}

func TestTaggedReviewRequestWithoutLinkSkipsTriage(t *testing.T) {
	m := Message{
		Channel:  other,
		AuthorID: "U400PEER",
		Text:     "<@U100SELF> check review please",
	}

	if got := classifier().Classify(m, false); got.Kind != KindReview {
		t.Errorf("kind = %s, want review", got.Kind)
	}
}

func TestReviewWordWithoutTagStillGoesToTriage(t *testing.T) {
	m := Message{
		Channel:      other,
		AuthorID:     "U400PEER",
		Text:         "review is taking longer",
		SelfInThread: true,
	}

	if got := classifier().Classify(m, false); got.Kind != KindTriage {
		t.Errorf("kind = %s: without a tag the decision stays with Haiku", got.Kind)
	}
}

func TestTagWithoutReviewWordGoesToTriage(t *testing.T) {
	m := Message{Channel: other, AuthorID: "U400PEER", Text: "<@U100SELF> check when convenient"}

	if got := classifier().Classify(m, false); got.Kind != KindTriage {
		t.Errorf("kind = %s: a plain tag goes to Haiku", got.Kind)
	}
}

func TestBotAndSystemMessagesNeverReachTriage(t *testing.T) {
	c := classifier()
	for _, m := range []Message{
		{Channel: "C200TEAM", AuthorID: "B01BOT", IsBot: true, Text: "Pipeline #1587364 passed for PRJ-8862 <@U100SELF>", IsDM: true},
		{Channel: "C200TEAM", AuthorID: "U400PEER", Subtype: "channel_join", Text: "<@U400PEER> has joined the channel", SelfInThread: true},
		{Channel: home, AuthorID: "B01BOT", IsBot: true, Text: "[HANDOFF] <@U100SELF> https://gitlab.example.com/team-a/service-a/-/merge_requests/899"},
	} {
		res := c.Classify(m, false)
		if res.Kind != KindNone || res.Filtered == "" {
			t.Errorf("message %+v classified as %s (filtered=%q), want none", m, res.Kind, res.Filtered)
		}
	}
}

func TestAcknowledgementsSkipTriage(t *testing.T) {
	c := classifier()
	for _, text := range []string{"ok", "got it", ":kekw:", "merging", "thanks <@U400PEER>", "yes, got it)", "Ok, thanks!", ")))", "2", "+1", "👍👍"} {
		res := c.Classify(Message{Channel: "C200TEAM", AuthorID: "U400PEER", Text: text, SelfInThread: true}, false)
		if res.Kind != KindNone || res.Filtered == "" {
			t.Errorf("%q must be filtered before triage, got %s", text, res.Kind)
		}
	}
}

func TestShortRequestsLinksQuestionsAndTagsStillGoToTriage(t *testing.T) {
	c := classifier()
	for _, text := range []string{
		"well https://jira.example.com/browse/PRJ-8874",
		"who?",
		"<@U100SELF> check",
		"Alex, check both when possible",
		"check",
		"Psst",
		"Alex, ok",
	} {
		res := c.Classify(Message{Channel: "C200TEAM", AuthorID: "U400PEER", Text: text, SelfInThread: true}, false)
		if res.Kind != KindTriage {
			t.Errorf("%q must reach triage, got %s (filtered=%q)", text, res.Kind, res.Filtered)
		}
	}
}

func TestShortReplyFilterDoesNotApplyToHomeChannelReviewThreads(t *testing.T) {
	c := classifier()

	res := c.Classify(Message{Channel: home, AuthorID: "U400PEER", Text: "done"}, true)

	if res.Kind != KindReview {
		t.Errorf("review thread continuation must survive the short-reply filter, got %s", res.Kind)
	}
}

func TestStopPhraseWinsOverEverythingElse(t *testing.T) {
	c := classifier()
	for _, m := range []Message{
		{Channel: home, AuthorID: selfID, Text: "Atlas, stop"},
		{Channel: "C200TEAM", AuthorID: twinID, Text: "atlas stop, not for you", SelfInThread: true},
		{Channel: "D100DM", AuthorID: twinID, Text: "Atlas, stop — I will handle it https://gitlab.example.com/a/b/-/merge_requests/1", IsDM: true},
	} {
		if got := c.Classify(m, true); got.Kind != KindStop {
			t.Errorf("%q → %s, want stop", m.Text, got.Kind)
		}
	}
}

func TestStopPhraseFromBotIsIgnored(t *testing.T) {
	m := Message{Channel: home, AuthorID: "B01BOT", IsBot: true, Text: "Atlas, stop"}

	if got := classifier().Classify(m, false); got.Kind != KindNone {
		t.Errorf("bot stop → %s, want none", got.Kind)
	}
}

func TestEnglishRulesRecogniseReviewAndAcknowledgements(t *testing.T) {
	r := Rules{
		MentionWords: []string{"ada"}, TriageWords: []string{"ada", "ivan"},
		ReviewMarkers: []string{"[handoff]"}, ReviewHeadMarkers: []string{"[head changed]"},
		ReviewWords: ReviewWordsEN, AckWords: AckWordsEN,
	}
	c := New(selfID, home, r)
	mr := " https://gitlab.example.com/team-a/service-a/-/merge_requests/899"

	if got := c.Classify(Message{Channel: home, AuthorID: twinID, Text: "ada, please review" + mr}, false); got.Kind != KindReview {
		t.Errorf("english review request → %s", got.Kind)
	}
	if got := c.Classify(Message{Channel: home, AuthorID: twinID, Text: "[HEAD CHANGED] abc → def"}, false); !got.ReviewSignal {
		t.Error("english head marker must be a review signal")
	}
	if got := c.Classify(Message{Channel: "C200TEAM", AuthorID: twinID, Text: "ok, thanks!", SelfInThread: true}, false); got.Kind != KindNone || got.Filtered == "" {
		t.Errorf("english acknowledgement → %s", got.Kind)
	}
	if got := c.Classify(Message{Channel: "C200TEAM", AuthorID: twinID, Text: "\u043e\u043a, \u0441\u043f\u0430\u0441\u0438\u0431\u043e", SelfInThread: true}, false); got.Kind != KindTriage {
		t.Errorf("non-default acknowledgement words must reach triage, got %s", got.Kind)
	}
}

func TestWithoutReviewMarkersOnlyReviewWordsStartAReview(t *testing.T) {
	r := rules()
	r.ReviewMarkers, r.ReviewHeadMarkers = nil, nil
	c := New(selfID, home, r)
	mr := " https://gitlab.example.com/team-a/service-a/-/merge_requests/899"

	if got := c.Classify(Message{Channel: home, AuthorID: twinID, Text: "[HANDOFF] Atlas, take it" + mr}, false); got.ReviewSignal {
		t.Error("a team without markers must not react to [HANDOFF]")
	}
	if got := c.Classify(Message{Channel: home, AuthorID: twinID, Text: "Atlas, review" + mr}, false); got.Kind != KindReview {
		t.Errorf("review word with a link still starts a review, got %s", got.Kind)
	}
}

func TestOwnPostsCarryProgressMarkersWithSHA(t *testing.T) {
	cases := []struct {
		text  string
		phase string
		sha   string
	}{
		{"[TAKEN] taking MR !899", ProgressTaken, ""},
		{"[REVIEW START] 08c9fc23a726", ProgressReviewStart, "08c9fc23a726"},
		{":robot_face: Atlas: [REVIEW DONE] 4a8259ce0aa1 — 2 findings, both pre-existing", ProgressReviewDone, "4a8259ce0aa1"},
		{":robot_face: Atlas: `[TAKEN]` legacy-service-a !896 / cf344d444f795e39d7710e47960abb2db57d718b", ProgressTaken, "cf344d444f795e39d7710e47960abb2db57d718b"},
		{"🤖 Atlas: *[REVIEW DONE]* `8558b135cda84e285d413f214b374aad9bf8421f` — HEAD checked", ProgressReviewDone, "8558b135cda84e285d413f214b374aad9bf8421f"},
		{"[DONE] ticket created", ProgressDone, ""},
		{"checked, no questions", "", ""},
		{"I will not start. `[TAKEN]` will post after `[HEAD CHANGED]` with the new SHA", "", ""},
		{":robot_face: Atlas: [HANDOFF] @ivan — review !893", "", ""},
	}
	for _, tc := range cases {
		got := classifier().Classify(Message{Channel: home, AuthorID: selfID, Text: tc.text}, true)
		if got.Kind != KindNone {
			t.Errorf("%q: kind = %s, own post must not open anything", tc.text, got.Kind)
		}
		if got.Progress != tc.phase || got.ProgressSHA != tc.sha {
			t.Errorf("%q: progress = %q sha = %q, want %q %q", tc.text, got.Progress, got.ProgressSHA, tc.phase, tc.sha)
		}
	}
}

func TestPeerPostsNeverCountAsOwnProgress(t *testing.T) {
	got := classifier().Classify(Message{Channel: home, AuthorID: peerID, Text: "[REVIEW DONE] 4a8259ce0aa1"}, true)

	if got.Progress != "" || got.Kind != KindReview {
		t.Errorf("result = %+v, want a review continuation without progress", got)
	}
}
