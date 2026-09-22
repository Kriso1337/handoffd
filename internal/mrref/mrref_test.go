package mrref

import "testing"

func TestExtractsSingleMRAndJiraKey(t *testing.T) {
	text := "[HANDOFF] @U100SELF, take this review, please " +
		"[Service A !899](https://gitlab.example.com/team-a/service-a/-/merge_requests/899) · " +
		"[PRJ-8866](https://jira.example.com/browse/PRJ-8866).\n" +
		"HEAD: `08c9fc23a726a07692b7f9cf28d4ef6f57e4fec6`\n" +
		"Base: `master` / `da85e99d6e589924215fb821669ac10e1172ecdd`"

	refs := Extract(text)

	mr, ok := refs.Primary()
	if !ok {
		t.Fatal("no MR extracted")
	}
	if mr.Namespace != "team-a" || mr.Project != "service-a" || mr.IID != 899 {
		t.Errorf("mr = %+v", mr)
	}
	if len(refs.JiraKeys) != 1 || refs.JiraKeys[0] != "PRJ-8866" {
		t.Errorf("jira = %v", refs.JiraKeys)
	}
	if refs.SHA != "08c9fc23a726a07692b7f9cf28d4ef6f57e4fec6" {
		t.Errorf("sha = %q, base SHA must not win", refs.SHA)
	}
}

func TestExtractsFromSlackLinkMarkup(t *testing.T) {
	text := "[HANDOFF] <@U100SELF>, take this review " +
		"<https://gitlab.example.com/team-a/service-a/-/merge_requests/899|Service A !899> · " +
		"<https://jira.example.com/browse/PRJ-8866|PRJ-8866>."

	refs := Extract(text)

	mr, ok := refs.Primary()
	if !ok {
		t.Fatal("no MR extracted from Slack link markup")
	}
	if mr.Project != "service-a" || mr.IID != 899 {
		t.Errorf("mr = %+v", mr)
	}
	if len(refs.JiraKeys) != 1 || refs.JiraKeys[0] != "PRJ-8866" {
		t.Errorf("jira = %v", refs.JiraKeys)
	}
}

func TestExtractsNestedNamespace(t *testing.T) {
	text := "https://gitlab.example.com/team-b/service-b/-/merge_requests/2251"

	mr, ok := Extract(text).Primary()

	if !ok {
		t.Fatal("no MR extracted")
	}
	if mr.Namespace != "team-b" || mr.Project != "service-b" || mr.IID != 2251 {
		t.Errorf("mr = %+v", mr)
	}
}

func TestExtractsMultipleMRsInOrderWithoutDuplicates(t *testing.T) {
	text := "• Service B MR !2246 https://gitlab.example.com/team-b/service-b/-/merge_requests/2246\n" +
		"• Service A MR !897 https://gitlab.example.com/team-a/service-a/-/merge_requests/897\n" +
		"repeated: https://gitlab.example.com/team-b/service-b/-/merge_requests/2246"

	refs := Extract(text)

	if len(refs.MRs) != 2 {
		t.Fatalf("want 2 MRs, got %d: %+v", len(refs.MRs), refs.MRs)
	}
	if refs.MRs[0].Slug() != "service-b!2246" {
		t.Errorf("first = %s", refs.MRs[0].Slug())
	}
	if refs.MRs[1].Slug() != "service-a!897" {
		t.Errorf("second = %s", refs.MRs[1].Slug())
	}
}

func TestHeadChangedTakesNewSHA(t *testing.T) {
	text := "[HEAD CHANGED] 908ddd9f83f1fb18c2044c02007e8aa18e16b7d8 → 4a8259ce0aa1b2c3d4e5f60718293a4b5c6d7e8f"

	if got := Extract(text).SHA; got != "4a8259ce0aa1b2c3d4e5f60718293a4b5c6d7e8f" {
		t.Errorf("sha = %q", got)
	}
}

func TestReviewStartSHAUsedWhenNoHeadLine(t *testing.T) {
	text := "[REVIEW START] 84a98eeefb6edc9b7b3a8d4eb17008b6e4fae897"

	if got := Extract(text).SHA; got != "84a98eeefb6edc9b7b3a8d4eb17008b6e4fae897" {
		t.Errorf("sha = %q", got)
	}
}

func TestTimestampIsNotMistakenForSHA(t *testing.T) {
	text := "HEAD updated, ts=1789047527"

	if got := Extract(text).SHA; got != "" {
		t.Errorf("sha = %q, want empty", got)
	}
}

func TestEmptyTextYieldsNothing(t *testing.T) {
	refs := Extract("")

	if _, ok := refs.Primary(); ok {
		t.Error("unexpected MR")
	}
	if len(refs.JiraKeys) != 0 || refs.SHA != "" {
		t.Errorf("refs = %+v", refs)
	}
}

func TestIgnoresURLWithoutNamespace(t *testing.T) {
	text := "https://gitlab.example.com/service-a/-/merge_requests/899"

	if _, ok := Extract(text).Primary(); ok {
		t.Error("MR without namespace must be ignored")
	}
}

func TestExtractCapturesHost(t *testing.T) {
	text := "https://gitlab.example.com/team-a/service-a/-/merge_requests/899"

	mr, ok := Extract(text).Primary()

	if !ok {
		t.Fatal("no MR extracted")
	}
	if mr.Host != "gitlab.example.com" {
		t.Errorf("host = %q", mr.Host)
	}
	if mr.Path() != "team-a/service-a" {
		t.Errorf("path = %q", mr.Path())
	}
	if mr.Key() != "gitlab.example.com/team-a/service-a!899" {
		t.Errorf("key = %q", mr.Key())
	}
}

func TestSameProjectNameInDifferentNamespacesStaysDistinct(t *testing.T) {
	text := "https://gitlab.example.com/team-a/service/-/merge_requests/7 " +
		"https://gitlab.example.com/team-b/service/-/merge_requests/7"

	refs := Extract(text)

	if len(refs.MRs) != 2 {
		t.Fatalf("want 2 MRs, got %d: %+v", len(refs.MRs), refs.MRs)
	}
	if refs.MRs[0].Namespace != "team-a" || refs.MRs[1].Namespace != "team-b" {
		t.Errorf("mrs = %+v", refs.MRs)
	}
}

func TestSameMROnDifferentHostsStaysDistinct(t *testing.T) {
	text := "https://gitlab.example.com/team-a/service/-/merge_requests/7 " +
		"https://gitlab.other.com/team-a/service/-/merge_requests/7"

	if got := len(Extract(text).MRs); got != 2 {
		t.Errorf("want 2 MRs, got %d", got)
	}
}

func TestExtractsGitHubPullRequestsNextToGitLabMergeRequests(t *testing.T) {
	text := "check https://github.com/acme/service-a/pull/42 and then " +
		"<https://gitlab.example.com/team-a/service-a/-/merge_requests/899|!899>, " +
		"and also https://ghe.corp.example/platform/infra/pull/7?diff=split"

	refs := Extract(text)

	if len(refs.MRs) != 3 {
		t.Fatalf("mrs = %+v", refs.MRs)
	}
	pr := refs.MRs[0]
	if pr.Forge != ForgeGitHub || pr.Host != "github.com" || pr.Namespace != "acme" || pr.Project != "service-a" || pr.IID != 42 {
		t.Errorf("pr = %+v", pr)
	}
	if pr.HeadRef() != "refs/pull/42/head" || pr.Slug() != "service-a#42" || pr.ForgeName() != "GitHub" {
		t.Errorf("pr refs = %s %s %s", pr.HeadRef(), pr.Slug(), pr.ForgeName())
	}
	mr := refs.MRs[1]
	if mr.Forge != ForgeGitLab || mr.HeadRef() != "refs/merge-requests/899/head" || mr.Slug() != "service-a!899" || mr.ForgeName() != "GitLab" {
		t.Errorf("mr = %+v refs = %s %s", mr, mr.HeadRef(), mr.Slug())
	}
	if ghe := refs.MRs[2]; ghe.Host != "ghe.corp.example" || ghe.Namespace != "platform" || ghe.IID != 7 || !ghe.IsGitHub() {
		t.Errorf("ghe = %+v", ghe)
	}
}

func TestGitHubLinksOutsidePullsAreNotReferences(t *testing.T) {
	refs := Extract("see https://github.com/acme/service-a/pulls and https://github.com/acme/service-a/issues/42")

	if len(refs.MRs) != 0 {
		t.Errorf("mrs = %+v", refs.MRs)
	}
}

func TestSameCommitMatchesShortAndFullForms(t *testing.T) {
	full := "4a8259ce0aa1b2c3d4e5f60718293a4b5c6d7e8f"
	cases := []struct {
		a, b string
		want bool
	}{
		{full, full, true},
		{"4a8259ce0aa1", full, true},
		{full, "4A8259CE", true},
		{"3bd416e4346c", full, false},
		{"4a8259", full, false},
		{"", full, false},
	}
	for _, c := range cases {
		if got := SameCommit(c.a, c.b); got != c.want {
			t.Errorf("SameCommit(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestJiraKeysIgnoreTechnicalTokens(t *testing.T) {
	text := "converted to UTF-8, checked SHA-256, enabled HTTP-2 and ISO-8859 — none of these is a ticket"

	if got := Extract(text).JiraKeys; len(got) != 0 {
		t.Errorf("keys = %v, encodings and protocols are not Jira keys", got)
	}
}

func TestJiraKeysComeFromLinkedContext(t *testing.T) {
	cases := map[string][]string{
		"[PRJ-8866](https://jira.example.com/browse/PRJ-8866)": {"PRJ-8866"},
		"https://jira.example.com/browse/PRJ-6135":             {"PRJ-6135"},
		"check PRJ-6135 in Jira":                               nil,
	}
	for text, want := range cases {
		got := Extract(text).JiraKeys
		if len(got) != len(want) {
			t.Errorf("%q -> %v, want %v", text, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%q -> %v, want %v", text, got, want)
				break
			}
		}
	}
}

func TestJiraKeyAndTechnicalTokenInOneMessage(t *testing.T) {
	text := "hash SHA-256 does not match, ticket [PRJ-6135](https://jira.example.com/browse/PRJ-6135)"

	got := Extract(text).JiraKeys

	if len(got) != 1 || got[0] != "PRJ-6135" {
		t.Errorf("keys = %v, want only the linked ticket", got)
	}
}
