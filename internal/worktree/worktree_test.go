package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kriso1337/handoffd/internal/forge"
	"github.com/Kriso1337/handoffd/internal/mrref"
)

const (
	oldSHA  = "3bd416e4346c8de2f21af16dc0dd8728c0d0f1d5"
	newSHA  = "08c9fc23a726a07692b7f9cf28d4ef6f57e4fec6"
	baseSHA = "da85e99d6e589924215fb821669ac10e1172ecdd"
)

type fakeGit struct {
	origin  string
	calls   [][]string
	fail    map[string]error
	heads   map[string]string
	remote  string
	dirty   map[string]bool
	missing map[string]bool
	broken  map[string]bool
}

func newFakeGit() *fakeGit {
	return &fakeGit{heads: map[string]string{}, dirty: map[string]bool{}, missing: map[string]bool{}, broken: map[string]bool{}, remote: newSHA,
		origin: "https://gitlab.example.com/team-a/service-a.git"}
}

type fakeMRs struct {
	info  forge.Details
	err   error
	calls int
}

func (f *fakeMRs) Details(context.Context, mrref.MR) (forge.Details, error) {
	f.calls++
	return f.info, f.err
}

func knownBase() *fakeMRs {
	return &fakeMRs{info: forge.Details{HeadSHA: newSHA, BaseSHA: baseSHA, TargetBranch: "master", State: "opened"}}
}

func (f *fakeGit) Run(_ context.Context, dir string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{dir}, args...))
	if err, ok := f.fail[args[0]]; ok {
		return []byte("git said no"), err
	}
	if f.broken[dir] && (args[0] == "rev-parse" || args[0] == "status") {
		return []byte("fatal: not a git repository"), errors.New("exit 128")
	}
	switch args[0] {
	case "remote":
		return []byte(f.origin + "\n"), nil
	case "cat-file":
		if f.missing[strings.TrimSuffix(args[2], "^{commit}")] {
			return []byte("fatal: Not a valid object name"), errors.New("exit 128")
		}
		return nil, nil
	case "rev-parse":
		if args[1] == "FETCH_HEAD" || strings.HasPrefix(args[1], "refs/handoffd/") {
			return []byte(f.remote + "\n"), nil
		}
		return []byte(f.heads[dir] + "\n"), nil
	case "status":
		if f.dirty[dir] {
			return []byte(" M app/service.go\n?? notes.txt\n"), nil
		}
		return nil, nil
	case "checkout":
		f.heads[dir] = args[len(args)-1]
	case "worktree":
		switch args[1] {
		case "add":
			path, sha := args[3], args[4]
			if err := os.MkdirAll(path, 0o700); err != nil {
				return nil, err
			}
			f.heads[path] = sha
		case "remove":
			path := args[len(args)-1]
			if f.dirty[path] {
				return []byte("fatal: contains modified or untracked files, use --force to delete it"), errors.New("exit 128")
			}
			return nil, os.RemoveAll(path)
		}
	}
	return nil, nil
}

func (f *fakeGit) commands() []string {
	var out []string
	for _, c := range f.calls {
		out = append(out, strings.Join(c[1:], " "))
	}
	return out
}

func repoWithGitDir(t *testing.T, project string) (string, func(mrref.MR) string) {
	t.Helper()
	projects := t.TempDir()
	repo := filepath.Join(projects, project)
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	return projects, func(mrref.MR) string { return repo }
}

func existingWorktree(t *testing.T, git *fakeGit, repo, name, sha string) string {
	t.Helper()
	path := filepath.Join(repo, ".claude", "worktrees", name)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	git.heads[path] = sha
	return path
}

var mr = mrref.MR{Host: "gitlab.example.com", Namespace: "team-a", Project: "service-a", IID: 899}

func TestPrepareCreatesDetachedWorktreeOnExactHead(t *testing.T) {
	_, repoDir := repoWithGitDir(t, "team-a")
	git := newFakeGit()

	got, err := New(git, repoDir, nil).Prepare(context.Background(), mr, nil)

	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !strings.HasSuffix(got.Path, filepath.Join(".claude", "worktrees", "review-899")) {
		t.Errorf("path = %q", got.Path)
	}
	if got.SHA != newSHA || got.HeadSHA != newSHA {
		t.Errorf("sha = %q head = %q, want both %q", got.SHA, got.HeadSHA, newSHA)
	}
	cmds := git.commands()
	if cmds[0] != "remote get-url origin" {
		t.Errorf("the origin must be confirmed before anything is fetched, first call = %q", cmds[0])
	}
	if cmds[1] != "fetch --force origin refs/merge-requests/899/head:refs/handoffd/gitlab.example.com/team-a/service-a/899" {
		t.Errorf("second call = %q", cmds[1])
	}
	add := "worktree add --detach " + got.Path + " " + newSHA
	if !contains(cmds, add) {
		t.Errorf("worktree must be added on the exact SHA, calls:\n%s", strings.Join(cmds, "\n"))
	}
}

func TestPrepareMovesCleanReusedWorktreeToCurrentHead(t *testing.T) {
	_, repoDir := repoWithGitDir(t, "team-a")
	git := newFakeGit()
	existing := existingWorktree(t, git, repoDir(mr), "review-899", oldSHA)

	got, err := New(git, repoDir, nil).Prepare(context.Background(), mr, nil)

	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got.Path != existing {
		t.Errorf("path = %q, want %q", got.Path, existing)
	}
	if got.SHA != newSHA {
		t.Errorf("sha = %q, want the worktree moved to %q", got.SHA, newSHA)
	}
	if !contains(git.commands(), "checkout --detach "+newSHA) {
		t.Errorf("stale worktree must be checked out to the new head, calls: %v", git.commands())
	}
	if !strings.Contains(got.Note, oldSHA[:12]) || !strings.Contains(got.Note, newSHA[:12]) {
		t.Errorf("note must name both SHAs: %q", got.Note)
	}
}

func TestPrepareRefusesDirtyStaleWorktree(t *testing.T) {
	_, repoDir := repoWithGitDir(t, "team-a")
	git := newFakeGit()
	existing := existingWorktree(t, git, repoDir(mr), "review-899", oldSHA)
	git.dirty[existing] = true

	got, err := New(git, repoDir, nil).Prepare(context.Background(), mr, nil)

	if err == nil {
		t.Fatal("a dirty worktree that cannot move to the MR head must not be reported as prepared")
	}
	if !strings.Contains(err.Error(), oldSHA[:12]) || !strings.Contains(err.Error(), newSHA[:12]) {
		t.Errorf("error must name both SHAs: %v", err)
	}
	if got.SHA != oldSHA || got.HeadSHA != newSHA {
		t.Errorf("sha = %q head = %q", got.SHA, got.HeadSHA)
	}
	if !got.Dirty {
		t.Error("dirty flag must be set")
	}
	for _, c := range git.commands() {
		if strings.HasPrefix(c, "checkout") {
			t.Fatalf("dirty worktree must not be touched: %v", git.commands())
		}
	}
	if !strings.Contains(got.Note, "2 file") {
		t.Errorf("note must count the local changes: %q", got.Note)
	}
}

func TestPrepareKeepsDirtyWorktreeAtTheMRHead(t *testing.T) {
	_, repoDir := repoWithGitDir(t, "team-a")
	git := newFakeGit()
	existing := existingWorktree(t, git, repoDir(mr), "review-899", newSHA)
	git.dirty[existing] = true

	got, err := New(git, repoDir, nil).Prepare(context.Background(), mr, nil)

	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got.SHA != newSHA || !got.Dirty {
		t.Errorf("result = %+v", got)
	}
}

func TestPrepareLeavesUpToDateWorktreeAlone(t *testing.T) {
	_, repoDir := repoWithGitDir(t, "team-a")
	git := newFakeGit()
	existingWorktree(t, git, repoDir(mr), "review-899", newSHA)

	got, err := New(git, repoDir, nil).Prepare(context.Background(), mr, nil)

	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got.SHA != newSHA || got.Dirty {
		t.Errorf("result = %+v", got)
	}
	for _, c := range git.commands() {
		if strings.HasPrefix(c, "checkout") || strings.HasPrefix(c, "worktree add") {
			t.Fatalf("up-to-date worktree must only be inspected: %v", git.commands())
		}
	}
}

func TestPrepareUsesSuffixedPathWhenBaseIsHeldByLiveSession(t *testing.T) {
	_, repoDir := repoWithGitDir(t, "team-a")
	git := newFakeGit()
	base := existingWorktree(t, git, repoDir(mr), "review-899", oldSHA)

	got, err := New(git, repoDir, nil).Prepare(context.Background(), mr, map[string]bool{base: true})

	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if filepath.Base(got.Path) != "review-899-2" {
		t.Errorf("path = %q, want a second tree next to the busy one", got.Path)
	}
	if git.heads[base] != oldSHA {
		t.Error("busy worktree must not be moved under a live session")
	}
	if got.SHA != newSHA {
		t.Errorf("sha = %q", got.SHA)
	}
}

func TestPrepareFallsBackToExistingWorktreeWhenFetchFails(t *testing.T) {
	_, repoDir := repoWithGitDir(t, "team-a")
	git := newFakeGit()
	git.fail = map[string]error{"fetch": errors.New("exit 128")}
	existing := existingWorktree(t, git, repoDir(mr), "review-899", oldSHA)

	got, err := New(git, repoDir, nil).Prepare(context.Background(), mr, nil)

	if err == nil {
		t.Fatal("fetch failure must be reported")
	}
	if got.Path != existing || got.SHA != oldSHA {
		t.Errorf("result = %+v, want the old tree kept usable", got)
	}
	if !strings.Contains(got.Note, "fetch") {
		t.Errorf("note = %q", got.Note)
	}
}

func TestPrepareReportsMissingRepository(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nowhere")
	git := newFakeGit()

	got, err := New(git, func(mrref.MR) string { return missing }, nil).Prepare(context.Background(), mr, nil)

	if err == nil {
		t.Fatal("want error for missing repository")
	}
	if got.Path != "" {
		t.Errorf("path = %q, want empty", got.Path)
	}
	if !strings.Contains(got.Note, "service-a") {
		t.Errorf("note = %q, must name the project", got.Note)
	}
	if len(git.calls) != 0 {
		t.Errorf("git must not run for missing repository: %v", git.calls)
	}
}

func TestPrepareReportsFetchFailureWithoutWorktree(t *testing.T) {
	_, repoDir := repoWithGitDir(t, "team-a")
	git := newFakeGit()
	git.fail = map[string]error{"fetch": errors.New("exit 128")}

	got, err := New(git, repoDir, nil).Prepare(context.Background(), mr, nil)

	if err == nil {
		t.Fatal("want error when fetch fails")
	}
	if got.Path != "" {
		t.Errorf("path = %q, want empty", got.Path)
	}
}

func TestPrepareFailsWhenReusedWorktreeIsUnreadable(t *testing.T) {
	git := newFakeGit()
	projects, repoDir := repoWithGitDir(t, "service-a")
	path := existingWorktree(t, git, filepath.Join(projects, "service-a"), "review-899", oldSHA)
	git.broken[path] = true

	res, err := New(git, repoDir, nil).Prepare(context.Background(), mr, nil)

	if err == nil || !strings.Contains(err.Error(), "rev-parse HEAD") {
		t.Fatalf("Prepare = %v, want a fail-closed error", err)
	}
	if res.SHA != "" || res.Dirty || !strings.Contains(res.Note, "worktree unreadable") {
		t.Errorf("result = %+v, a broken tree must not look clean", res)
	}
	for _, c := range git.commands() {
		if strings.HasPrefix(c, "checkout") {
			t.Errorf("no checkout on a tree whose state is unknown: %v", git.commands())
		}
	}
}

func TestPruneKeepsWorktreeWhoseStatusIsUnknown(t *testing.T) {
	git := newFakeGit()
	projects, repoDir := repoWithGitDir(t, "service-a")
	path := existingWorktree(t, git, filepath.Join(projects, "service-a"), "review-899", oldSHA)
	git.broken[path] = true
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	report, err := New(git, repoDir, nil).Prune(context.Background(), PruneOptions{ProjectsDir: projects, Now: time.Now(), TTL: time.Hour})

	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(report.Removed) != 0 || len(report.Kept) != 1 || !strings.Contains(report.Kept[0].Reason, "status in") {
		t.Errorf("report = %+v, want the tree kept with the git error", report)
	}
}

func TestInspectReportsDriftWithoutTouchingTree(t *testing.T) {
	_, repoDir := repoWithGitDir(t, "team-a")
	git := newFakeGit()
	existing := existingWorktree(t, git, repoDir(mr), "review-899", oldSHA)

	got, err := New(git, repoDir, nil).Inspect(context.Background(), existing, mr)

	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if got.SHA != oldSHA || got.HeadSHA != newSHA {
		t.Errorf("result = %+v", got)
	}
	if git.heads[existing] != oldSHA {
		t.Error("Inspect must not move the tree")
	}
}

func TestRefreshMovesCleanTree(t *testing.T) {
	_, repoDir := repoWithGitDir(t, "team-a")
	git := newFakeGit()
	existing := existingWorktree(t, git, repoDir(mr), "review-899", oldSHA)

	got, err := New(git, repoDir, nil).Refresh(context.Background(), existing, mr)

	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got.SHA != newSHA || git.heads[existing] != newSHA {
		t.Errorf("tree not moved: %+v", got)
	}
}

func TestStaleSkipsFreshAndKeptWorktrees(t *testing.T) {
	now := time.Now()
	candidates := []Candidate{
		{Path: "/repo/.claude/worktrees/review-1", Modified: now.Add(-30 * 24 * time.Hour)},
		{Path: "/repo/.claude/worktrees/review-2", Modified: now.Add(-time.Hour)},
		{Path: "/repo/.claude/worktrees/review-3", Modified: now.Add(-30 * 24 * time.Hour)},
	}
	keep := map[string]bool{"/repo/.claude/worktrees/review-3": true}

	got := Stale(candidates, now, 14*24*time.Hour, keep)

	if len(got) != 1 {
		t.Fatalf("want 1 stale worktree, got %d: %+v", len(got), got)
	}
	if got[0].Path != "/repo/.claude/worktrees/review-1" {
		t.Errorf("stale = %q", got[0].Path)
	}
}

func pruneFixture(t *testing.T) (string, func(mrref.MR) string, *fakeGit, string) {
	t.Helper()
	projects, repoDir := repoWithGitDir(t, "team-a")
	root := filepath.Join(repoDir(mr), ".claude", "worktrees")
	git := newFakeGit()
	for _, name := range []string{"review-1", "review-2", "review-3", "scratch"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	for _, name := range []string{"review-1", "review-2", "scratch"} {
		if err := os.Chtimes(filepath.Join(root, name), old, old); err != nil {
			t.Fatal(err)
		}
	}
	git.dirty[filepath.Join(root, "review-2")] = true
	return projects, repoDir, git, root
}

func TestPruneRemovesOnlyCleanStaleReviewWorktreesWithoutForce(t *testing.T) {
	projects, repoDir, git, root := pruneFixture(t)

	report, err := New(git, repoDir, nil).Prune(context.Background(), PruneOptions{
		ProjectsDir: projects, Now: time.Now(), TTL: 14 * 24 * time.Hour,
	})

	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(report.Removed) != 1 || filepath.Base(report.Removed[0]) != "review-1" {
		t.Errorf("removed = %v", report.Removed)
	}
	if len(report.Kept) != 1 || filepath.Base(report.Kept[0].Path) != "review-2" || !strings.Contains(report.Kept[0].Reason, "2 file") {
		t.Errorf("kept = %+v, want the dirty tree kept with a reason", report.Kept)
	}
	if _, err := os.Stat(filepath.Join(root, "review-2")); err != nil {
		t.Error("dirty worktree must survive prune")
	}
	for _, c := range git.commands() {
		if strings.Contains(c, "--force") {
			t.Fatalf("prune must never force-remove: %v", git.commands())
		}
	}
	if !contains(git.commands(), "worktree prune") {
		t.Errorf("git worktree prune must follow removals: %v", git.commands())
	}
}

func TestPruneDryRunTouchesNothing(t *testing.T) {
	projects, repoDir, git, root := pruneFixture(t)

	report, err := New(git, repoDir, nil).Prune(context.Background(), PruneOptions{
		ProjectsDir: projects, Now: time.Now(), TTL: 14 * 24 * time.Hour, DryRun: true,
	})

	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(report.Removed) != 1 {
		t.Errorf("dry run must still list what would go: %v", report.Removed)
	}
	if _, err := os.Stat(filepath.Join(root, "review-1")); err != nil {
		t.Error("dry run must not delete anything")
	}
	for _, c := range git.commands() {
		if strings.HasPrefix(c, "worktree remove") || c == "worktree prune" {
			t.Fatalf("dry run must not run destructive git: %v", git.commands())
		}
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func TestPrepareRecordsBaseFromGitLab(t *testing.T) {
	_, repoDir := repoWithGitDir(t, "team-a")
	git := newFakeGit()
	mrs := knownBase()

	got, err := New(git, repoDir, mrs).Prepare(context.Background(), mr, nil)

	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got.BaseSHA != baseSHA || got.TargetBranch != "master" {
		t.Errorf("result = %+v", got)
	}
	if mrs.calls != 1 {
		t.Errorf("gitlab calls = %d", mrs.calls)
	}
	if contains(git.commands(), "fetch origin master") {
		t.Error("base already present locally must not trigger a target fetch")
	}
}

func TestPrepareFetchesTargetBranchWhenBaseIsMissingLocally(t *testing.T) {
	_, repoDir := repoWithGitDir(t, "team-a")
	git := newFakeGit()
	git.missing[baseSHA] = true

	got, err := New(git, repoDir, knownBase()).Prepare(context.Background(), mr, nil)

	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !contains(git.commands(), "fetch origin master") {
		t.Errorf("missing base must be fetched through the target branch: %v", git.commands())
	}
	if got.BaseSHA != baseSHA {
		t.Errorf("base = %q", got.BaseSHA)
	}
}

func TestPrepareSurvivesGitLabOutage(t *testing.T) {
	_, repoDir := repoWithGitDir(t, "team-a")
	git := newFakeGit()
	mrs := &fakeMRs{err: errors.New("glab: 401")}

	got, err := New(git, repoDir, mrs).Prepare(context.Background(), mr, nil)

	if err != nil {
		t.Fatalf("Prepare must not fail on a missing base: %v", err)
	}
	if got.SHA != newSHA || got.BaseSHA != "" {
		t.Errorf("result = %+v", got)
	}
	if !strings.Contains(got.Note, "base unknown") || !strings.Contains(got.Note, "401") {
		t.Errorf("note = %q", got.Note)
	}
}

func TestRefreshKeepsBaseNextToMoveNote(t *testing.T) {
	_, repoDir := repoWithGitDir(t, "team-a")
	git := newFakeGit()
	existing := existingWorktree(t, git, repoDir(mr), "review-899", oldSHA)

	got, err := New(git, repoDir, knownBase()).Refresh(context.Background(), existing, mr)

	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got.SHA != newSHA || got.BaseSHA != baseSHA || !strings.Contains(got.Note, "moved") {
		t.Errorf("result = %+v", got)
	}
}

func TestWorktreeDirIsConfigurable(t *testing.T) {
	_, repoDir := repoWithGitDir(t, "team-a")
	git := newFakeGit()

	got, err := New(git, repoDir, nil).WithDir(".agents/trees").Prepare(context.Background(), mr, nil)

	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !strings.HasSuffix(got.Path, filepath.Join(".agents", "trees", "review-899")) {
		t.Errorf("path = %q", got.Path)
	}
}

func TestPrepareFetchesGitHubPullRequestHead(t *testing.T) {
	_, repoDir := repoWithGitDir(t, "team-a")
	git := newFakeGit()
	git.origin = "git@github.com:team-a/service-a.git"
	pr := mrref.MR{Host: "github.com", Namespace: "team-a", Project: "service-a", IID: 42, Forge: mrref.ForgeGitHub}

	got, err := New(git, repoDir, nil).Prepare(context.Background(), pr, nil)

	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !strings.HasSuffix(got.Path, filepath.Join(".claude", "worktrees", "review-42")) {
		t.Errorf("path = %q", got.Path)
	}
	if cmds := git.commands(); cmds[1] != "fetch --force origin refs/pull/42/head:refs/handoffd/github.com/team-a/service-a/42" {
		t.Errorf("second call = %q", cmds[1])
	}
}

func TestRevisionReadsTheProviderHeadAndBaseWithoutATree(t *testing.T) {
	_, repoDir := repoWithGitDir(t, "team-a")
	git := newFakeGit()

	got, err := New(git, repoDir, knownBase()).Revision(context.Background(), mr)

	if err != nil {
		t.Fatalf("Revision: %v", err)
	}
	if got.HeadSHA != newSHA || got.BaseSHA != baseSHA || got.TargetBranch != "master" {
		t.Errorf("result = %+v", got)
	}
	if got.Path != "" || got.SHA != "" {
		t.Errorf("result = %+v, Revision inspects no tree", got)
	}
}

func TestRevisionReportsAHeadItCannotFetch(t *testing.T) {
	_, repoDir := repoWithGitDir(t, "team-a")
	git := newFakeGit()
	git.fail = map[string]error{"fetch": errors.New("exit 128")}

	if _, err := New(git, repoDir, knownBase()).Revision(context.Background(), mr); err == nil {
		t.Fatal("Revision must report a head it cannot fetch")
	}
}

func TestRevisionLeavesTheBaseEmptyWhenTheProviderIsUnreachable(t *testing.T) {
	_, repoDir := repoWithGitDir(t, "team-a")
	git := newFakeGit()

	got, err := New(git, repoDir, &fakeMRs{err: errors.New("glab api: 503")}).Revision(context.Background(), mr)

	if err != nil {
		t.Fatalf("Revision: %v", err)
	}
	if got.HeadSHA != newSHA || got.BaseSHA != "" {
		t.Errorf("result = %+v, want the head alone and no guessed base", got)
	}
	if !strings.Contains(got.Note, "base unknown") {
		t.Errorf("note = %q", got.Note)
	}
}

func TestRevisionFetchesIntoARefOfItsOwn(t *testing.T) {
	_, repoDir := repoWithGitDir(t, "team-a")
	git := newFakeGit()

	if _, err := New(git, repoDir, knownBase()).Revision(context.Background(), mr); err != nil {
		t.Fatalf("Revision: %v", err)
	}

	var fetched, parsed string
	for _, call := range git.calls {
		switch call[1] {
		case "fetch":
			fetched = call[len(call)-1]
		case "rev-parse":
			parsed = call[2]
		}
	}
	if !strings.Contains(fetched, ":refs/handoffd/") {
		t.Errorf("fetch = %q, a shared FETCH_HEAD cannot tell two merge requests apart", fetched)
	}
	if !strings.HasPrefix(parsed, "refs/handoffd/") {
		t.Errorf("rev-parse = %q, want the ref this fetch wrote", parsed)
	}
}

func TestRevisionRefusesAHeadTheProviderDoesNotConfirm(t *testing.T) {
	_, repoDir := repoWithGitDir(t, "team-a")
	git := newFakeGit()
	mrs := &fakeMRs{info: forge.Details{HeadSHA: oldSHA, BaseSHA: baseSHA, TargetBranch: "master", State: "opened"}}

	_, err := New(git, repoDir, mrs).Revision(context.Background(), mr)

	if err == nil {
		t.Fatal("a fetched head that is not the head the provider names must not be reviewed")
	}
	if !strings.Contains(err.Error(), oldSHA[:12]) && !strings.Contains(err.Error(), newSHA[:12]) {
		t.Errorf("err = %v, want both revisions named", err)
	}
}

func TestARepoThatPointsAtAnotherProjectIsNotUsed(t *testing.T) {
	_, repoDir := repoWithGitDir(t, "team-a")
	git := newFakeGit()
	git.origin = "https://gitlab.example.com/team-b/service-a.git"

	_, err := New(git, repoDir, knownBase()).Revision(context.Background(), mr)

	if err == nil {
		t.Fatal("a directory whose origin is a different project must not be reviewed as this one")
	}
	if !strings.Contains(err.Error(), "team-b/service-a") || !strings.Contains(err.Error(), "team-a/service-a") {
		t.Errorf("err = %v, want both projects named", err)
	}
}

func TestTheCanonicalRemoteIsAcceptedInEitherForm(t *testing.T) {
	_, repoDir := repoWithGitDir(t, "team-a")
	for _, origin := range []string{
		"https://gitlab.example.com/team-a/service-a.git",
		"https://user@gitlab.example.com/team-a/service-a",
		"git@gitlab.example.com:team-a/service-a.git",
		"ssh://git@gitlab.example.com:2222/team-a/service-a.git",
	} {
		git := newFakeGit()
		git.origin = origin
		if _, err := New(git, repoDir, knownBase()).Revision(context.Background(), mr); err != nil {
			t.Errorf("origin %q: %v", origin, err)
		}
	}
}
