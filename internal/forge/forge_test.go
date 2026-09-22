package forge

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Kriso1337/handoffd/internal/mrref"
)

type fakeCmd struct {
	calls [][]string
	out   string
	err   error
}

func (f *fakeCmd) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	return []byte(f.out), f.err
}

var mr = mrref.MR{Host: "gitlab.example.com", Namespace: "team-a", Project: "service-a", IID: 899}

const payload = `{"iid":899,"state":"opened","target_branch":"master","source_branch":"PRJ-8866",
"sha":"08c9fc23a726a07692b7f9cf28d4ef6f57e4fec6",
"diff_refs":{"base_sha":"da85e99d6e589924215fb821669ac10e1172ecdd","head_sha":"08c9fc23a726a07692b7f9cf28d4ef6f57e4fec6","start_sha":"da85e99d6e589924215fb821669ac10e1172ecdd"}}`

func TestMergeRequestReadsRevisionsFromAPI(t *testing.T) {
	cmd := &fakeCmd{out: payload}

	got, err := New(cmd, "/opt/homebrew/bin/glab", "gh").Details(context.Background(), mr)

	if err != nil {
		t.Fatalf("Details: %v", err)
	}
	if got.HeadSHA != "08c9fc23a726a07692b7f9cf28d4ef6f57e4fec6" || got.BaseSHA != "da85e99d6e589924215fb821669ac10e1172ecdd" {
		t.Errorf("revisions = %+v", got)
	}
	if got.TargetBranch != "master" || got.State != "opened" {
		t.Errorf("mr = %+v", got)
	}
	call := strings.Join(cmd.calls[0], " ")
	for _, want := range []string{"/opt/homebrew/bin/glab api", "--hostname gitlab.example.com", "projects/team-a%2Fservice-a/merge_requests/899"} {
		if !strings.Contains(call, want) {
			t.Errorf("call %q missing %q", call, want)
		}
	}
}

func TestMergeRequestWithoutDiffRefsHasNoBase(t *testing.T) {
	cmd := &fakeCmd{out: `{"iid":899,"state":"opened","target_branch":"master","sha":"08c9fc23a726a07692b7f9cf28d4ef6f57e4fec6","diff_refs":null}`}

	got, err := New(cmd, "glab", "gh").Details(context.Background(), mr)

	if err != nil {
		t.Fatalf("Details: %v", err)
	}
	if got.BaseSHA != "" || got.HeadSHA != "08c9fc23a726a07692b7f9cf28d4ef6f57e4fec6" {
		t.Errorf("mr = %+v", got)
	}
}

func TestMergeRequestReportsCLIFailure(t *testing.T) {
	cmd := &fakeCmd{out: "glab: 404 Project Not Found (HTTP 404)", err: errors.New("exit 1")}

	_, err := New(cmd, "glab", "gh").Details(context.Background(), mr)

	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v, want the glab output", err)
	}
}

func TestMergeRequestRejectsGarbage(t *testing.T) {
	cmd := &fakeCmd{out: "not json"}

	if _, err := New(cmd, "glab", "gh").Details(context.Background(), mr); err == nil {
		t.Error("garbage output must be an error")
	}
}

func TestMergeRequestNeedsBinary(t *testing.T) {
	cmd := &fakeCmd{out: payload}

	if _, err := New(cmd, "", "").Details(context.Background(), mr); err == nil {
		t.Error("client without a binary must refuse")
	}
	if len(cmd.calls) != 0 {
		t.Error("nothing must be executed")
	}
}

var pr = mrref.MR{Host: "github.com", Namespace: "acme", Project: "service-a", IID: 42, Forge: mrref.ForgeGitHub}

func TestGitHubPullRequestReadsBaseAndHeadThroughGh(t *testing.T) {
	cmd := &fakeCmd{out: `{"number":42,"state":"open","base":{"ref":"main","sha":"da85e99d6e589924215fb821669ac10e1172ecdd"},"head":{"sha":"08c9fc23a726a07692b7f9cf28d4ef6f57e4fec6"}}`}

	got, err := New(cmd, "glab", "/opt/homebrew/bin/gh").Details(context.Background(), pr)

	if err != nil {
		t.Fatalf("Details: %v", err)
	}
	if got.Number != 42 || got.State != "open" || got.TargetBranch != "main" || got.BaseSHA != "da85e99d6e589924215fb821669ac10e1172ecdd" || got.HeadSHA != "08c9fc23a726a07692b7f9cf28d4ef6f57e4fec6" {
		t.Errorf("details = %+v", got)
	}
	if call := strings.Join(cmd.calls[0], " "); call != "/opt/homebrew/bin/gh api --hostname github.com repos/acme/service-a/pulls/42" {
		t.Errorf("call = %q", call)
	}
}

func TestGitHubNeedsGhAndReportsFailures(t *testing.T) {
	if _, err := New(&fakeCmd{}, "glab", "").Details(context.Background(), pr); err == nil {
		t.Error("client without gh must refuse GitHub")
	}
	cmd := &fakeCmd{out: "gh: Not Found (HTTP 404)", err: errors.New("exit 1")}
	if _, err := New(cmd, "glab", "gh").Details(context.Background(), pr); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v", err)
	}
}

func TestNotePostsThroughGlabWithTheRepositoryURL(t *testing.T) {
	cmd := &fakeCmd{}

	err := New(cmd, "/opt/homebrew/bin/glab", "gh").Note(context.Background(), mr, "(codex, 4a8259ce0aa1) app.go:12 nil deref")

	if err != nil {
		t.Fatalf("Note: %v", err)
	}
	got := strings.Join(cmd.calls[0], " ")
	want := "/opt/homebrew/bin/glab mr note 899 --repo https://gitlab.example.com/team-a/service-a --message (codex, 4a8259ce0aa1) app.go:12 nil deref"
	if got != want {
		t.Errorf("call = %q\nwant %q", got, want)
	}
	cmd.err, cmd.out = errors.New("exit 1"), "401 Unauthorized"
	if err := New(cmd, "glab", "gh").Note(context.Background(), mr, "x"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("CLI failure must surface, got %v", err)
	}
	if err := New(cmd, "", "gh").Note(context.Background(), mr, "x"); err == nil {
		t.Error("missing glab must be an error")
	}
}

func TestNotePostsThroughGhForPullRequests(t *testing.T) {
	cmd := &fakeCmd{}
	pr := mrref.MR{Host: "github.com", Namespace: "acme", Project: "service-a", IID: 42, Forge: mrref.ForgeGitHub}

	err := New(cmd, "glab", "/usr/local/bin/gh").Note(context.Background(), pr, "finding")

	if err != nil {
		t.Fatalf("Note: %v", err)
	}
	if got := strings.Join(cmd.calls[0], " "); got != "/usr/local/bin/gh pr comment 42 --repo github.com/acme/service-a --body finding" {
		t.Errorf("call = %q", got)
	}
	if err := New(cmd, "glab", "").Note(context.Background(), pr, "x"); err == nil {
		t.Error("missing gh must be an error")
	}
}
