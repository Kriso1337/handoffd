package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Kriso1337/handoffd/internal/forge"
	"github.com/Kriso1337/handoffd/internal/mrref"
)

const (
	reviewPrefix   = "review-"
	maxSuffix      = 20
	shortSHALength = 12
)

type Runner interface {
	Run(ctx context.Context, dir string, args ...string) ([]byte, error)
}

type MergeRequests interface {
	Details(ctx context.Context, mr mrref.MR) (forge.Details, error)
}

type Preparer struct {
	git     Runner
	repoDir func(mr mrref.MR) string
	mrs     MergeRequests
	subdir  string
}

func New(git Runner, repoDir func(mr mrref.MR) string, mrs MergeRequests) Preparer {
	return Preparer{git: git, repoDir: repoDir, mrs: mrs, subdir: DefaultDir}
}

func (p Preparer) WithDir(subdir string) Preparer {
	if subdir != "" {
		p.subdir = subdir
	}
	return p
}

const DefaultDir = ".claude/worktrees"

type Result struct {
	Path         string
	SHA          string
	HeadSHA      string
	BaseSHA      string
	TargetBranch string
	Dirty        bool
	Note         string
}

type revision struct {
	head         string
	base         string
	target       string
	providerHead string
	note         string
}

func (r Result) Stale() bool {
	return r.HeadSHA != "" && r.SHA != r.HeadSHA
}

func (p Preparer) Prepare(ctx context.Context, mr mrref.MR, busy map[string]bool) (Result, error) {
	repo := p.repoDir(mr)
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		note := fmt.Sprintf("repository %s not found locally (%s)", mr.Project, repo)
		return Result{Note: note}, fmt.Errorf("%s: %w", note, err)
	}

	path := p.choosePath(repo, mr.IID, busy)
	exists := dirExists(path)

	rev, fetchErr := p.fetchRevision(ctx, repo, mr)
	if fetchErr != nil {
		if !exists {
			return Result{Note: fetchErr.Error()}, fetchErr
		}
		res, err := p.inspect(ctx, path, revision{})
		if err != nil {
			return res, errors.Join(fetchErr, err)
		}
		res.Note = fetchErr.Error() + ", worktree left at " + short(res.SHA)
		return res, fetchErr
	}

	if exists {
		return p.refresh(ctx, path, rev, true)
	}
	if out, err := p.git.Run(ctx, repo, "worktree", "add", "--detach", path, rev.head); err != nil {
		note := fmt.Sprintf("could not create worktree %s", path)
		return Result{Note: note}, fmt.Errorf("%s: %w: %s", note, err, out)
	}
	res, err := p.inspect(ctx, path, rev)
	if err != nil {
		return res, err
	}
	if filepath.Base(path) != reviewPrefix+strconv.Itoa(mr.IID) {
		res.Note = joinNotes("primary worktree is held by another live session, created a separate one", res.Note)
	}
	return res, nil
}

func (p Preparer) Revision(ctx context.Context, mr mrref.MR) (Result, error) {
	rev, err := p.fetchRevision(ctx, p.repoDir(mr), mr)
	if err != nil {
		return Result{}, err
	}
	return Result{HeadSHA: rev.head, BaseSHA: rev.base, TargetBranch: rev.target, Note: rev.note}, nil
}

func (p Preparer) Inspect(ctx context.Context, path string, mr mrref.MR) (Result, error) {
	rev, fetchErr := p.fetchRevision(ctx, p.repoDir(mr), mr)
	res, err := p.inspect(ctx, path, rev)
	return res, errors.Join(fetchErr, err)
}

func (p Preparer) Refresh(ctx context.Context, path string, mr mrref.MR) (Result, error) {
	rev, fetchErr := p.fetchRevision(ctx, p.repoDir(mr), mr)
	if fetchErr != nil {
		res, err := p.inspect(ctx, path, revision{})
		return res, errors.Join(fetchErr, err)
	}
	return p.refresh(ctx, path, rev, true)
}

func (p Preparer) choosePath(repo string, iid int, busy map[string]bool) string {
	root := filepath.Join(repo, filepath.FromSlash(p.subdir))
	base := filepath.Join(root, reviewPrefix+strconv.Itoa(iid))
	if !busy[base] {
		return base
	}
	for n := 2; n <= maxSuffix; n++ {
		candidate := base + "-" + strconv.Itoa(n)
		if !busy[candidate] {
			return candidate
		}
	}
	return base + "-" + strconv.Itoa(maxSuffix+1)
}

func (p Preparer) fetchRevision(ctx context.Context, repo string, mr mrref.MR) (revision, error) {
	if err := p.confirmRemote(ctx, repo, mr); err != nil {
		return revision{}, err
	}
	ref, local := mr.HeadRef(), mr.LocalRef()
	if out, err := p.git.Run(ctx, repo, "fetch", "--force", "origin", ref+":"+local); err != nil {
		return revision{}, fmt.Errorf("fetch %s failed: %w: %s", ref, err, strings.TrimSpace(string(out)))
	}
	out, err := p.git.Run(ctx, repo, "rev-parse", local)
	if err != nil {
		return revision{}, fmt.Errorf("rev-parse %s: %w: %s", local, err, strings.TrimSpace(string(out)))
	}
	rev := revision{head: strings.TrimSpace(string(out))}
	if rev.head == "" {
		return revision{}, fmt.Errorf("fetch %s: %s is empty", ref, local)
	}
	p.lookupBase(ctx, repo, mr, &rev)
	if rev.providerHead != "" && !mrref.SameCommit(rev.providerHead, rev.head) {
		return revision{}, fmt.Errorf("%s fetched %s but %s names %s as its head",
			mr.Slug(), short(rev.head), mr.ForgeName(), short(rev.providerHead))
	}
	return rev, nil
}

func (p Preparer) confirmRemote(ctx context.Context, repo string, mr mrref.MR) error {
	out, err := p.git.Run(ctx, repo, "remote", "get-url", "origin")
	if err != nil {
		return fmt.Errorf("origin of %s not read: %w: %s", repo, err, strings.TrimSpace(string(out)))
	}
	host, path := splitRemote(strings.TrimSpace(string(out)))
	if !strings.EqualFold(host, mr.Host) || !strings.EqualFold(path, mr.Path()) {
		return fmt.Errorf("%s is %s/%s, not %s/%s", repo, host, path, mr.Host, mr.Path())
	}
	return nil
}

func splitRemote(url string) (host, path string) {
	url = strings.TrimSuffix(url, ".git")
	switch {
	case strings.Contains(url, "://"):
		url = url[strings.Index(url, "://")+3:]
	case strings.Contains(url, "@") && strings.Contains(url, ":"):
		url = strings.Replace(url[strings.Index(url, "@")+1:], ":", "/", 1)
	}
	if at := strings.Index(url, "@"); at >= 0 {
		url = url[at+1:]
	}
	host, path, _ = strings.Cut(url, "/")
	if port := strings.Index(host, ":"); port >= 0 {
		host = host[:port]
	}
	return host, path
}

func (p Preparer) lookupBase(ctx context.Context, repo string, mr mrref.MR, rev *revision) {
	if p.mrs == nil {
		return
	}
	info, err := p.mrs.Details(ctx, mr)
	if err != nil {
		rev.note = "base unknown: " + err.Error()
		return
	}
	rev.base, rev.target, rev.providerHead = info.BaseSHA, info.TargetBranch, info.HeadSHA
	if rev.base == "" || p.hasCommit(ctx, repo, rev.base) || rev.target == "" {
		return
	}
	if out, err := p.git.Run(ctx, repo, "fetch", "origin", rev.target); err != nil {
		rev.note = fmt.Sprintf("base %s not fetched: fetch %s: %s", short(rev.base), rev.target, strings.TrimSpace(string(out)))
	}
}

func (p Preparer) hasCommit(ctx context.Context, repo, sha string) bool {
	_, err := p.git.Run(ctx, repo, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

func (p Preparer) refresh(ctx context.Context, path string, rev revision, checkout bool) (Result, error) {
	res, err := p.inspect(ctx, path, rev)
	switch {
	case err != nil:
		return res, err
	case !res.Stale():
		return res, nil
	case res.Dirty:
		res.Note = joinNotes(fmt.Sprintf("worktree is dirty (%s), left at %s, MR head is %s",
			res.Note, short(res.SHA), short(rev.head)), rev.note)
		return res, fmt.Errorf("worktree %s is dirty at %s and cannot move to %s", path, short(res.SHA), short(rev.head))
	case !checkout:
		return res, nil
	}
	if out, err := p.git.Run(ctx, path, "checkout", "--detach", rev.head); err != nil {
		res.Note = fmt.Sprintf("could not move worktree to %s: %s", short(rev.head), strings.TrimSpace(string(out)))
		return res, fmt.Errorf("checkout %s in %s: %w: %s", rev.head, path, err, out)
	}
	old := res.SHA
	res.SHA = rev.head
	res.Note = joinNotes(fmt.Sprintf("worktree moved %s → %s", short(old), short(rev.head)), rev.note)
	return res, nil
}

func (p Preparer) inspect(ctx context.Context, path string, rev revision) (Result, error) {
	res := Result{Path: path, HeadSHA: rev.head, BaseSHA: rev.base, TargetBranch: rev.target, Note: rev.note}
	sha, err := p.head(ctx, path)
	if err != nil {
		res.Note = joinNotes("worktree unreadable: "+err.Error(), res.Note)
		return res, err
	}
	res.SHA = sha
	changed, err := p.changes(ctx, path)
	if err != nil {
		res.Note = joinNotes("worktree status unknown: "+err.Error(), res.Note)
		return res, err
	}
	if changed > 0 {
		res.Dirty = true
		res.Note = fmt.Sprintf("%d file(s) with local changes", changed)
	}
	return res, nil
}

func joinNotes(notes ...string) string {
	var kept []string
	for _, n := range notes {
		if n != "" {
			kept = append(kept, n)
		}
	}
	return strings.Join(kept, "; ")
}

func (p Preparer) head(ctx context.Context, path string) (string, error) {
	out, err := p.git.Run(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("rev-parse HEAD in %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	sha := strings.TrimSpace(string(out))
	if sha == "" {
		return "", fmt.Errorf("rev-parse HEAD in %s: empty output", path)
	}
	return sha, nil
}

func (p Preparer) changes(ctx context.Context, path string) (int, error) {
	out, err := p.git.Run(ctx, path, "status", "--porcelain")
	if err != nil {
		return 0, fmt.Errorf("status in %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	listing := strings.TrimSpace(string(out))
	if listing == "" {
		return 0, nil
	}
	return len(strings.Split(listing, "\n")), nil
}

type Candidate struct {
	Repo     string
	Path     string
	Modified time.Time
}

type PruneOptions struct {
	ProjectsDir string
	Now         time.Time
	TTL         time.Duration
	Keep        map[string]bool
	DryRun      bool
}

type Kept struct {
	Path   string
	Reason string
}

type Report struct {
	Removed []string
	Kept    []Kept
}

func (p Preparer) Prune(ctx context.Context, opts PruneOptions) (Report, error) {
	candidates, err := scan(opts.ProjectsDir, p.subdir)
	if err != nil {
		return Report{}, err
	}

	var report Report
	var failures error
	touched := map[string]bool{}
	for _, c := range Stale(candidates, opts.Now, opts.TTL, opts.Keep) {
		changed, err := p.changes(ctx, c.Path)
		if err != nil {
			report.Kept = append(report.Kept, Kept{Path: c.Path, Reason: err.Error()})
			continue
		}
		if changed > 0 {
			report.Kept = append(report.Kept, Kept{
				Path:   c.Path,
				Reason: fmt.Sprintf("%d file(s) with local changes", changed),
			})
			continue
		}
		if opts.DryRun {
			report.Removed = append(report.Removed, c.Path)
			continue
		}
		if out, err := p.git.Run(ctx, c.Repo, "worktree", "remove", c.Path); err != nil {
			failures = fmt.Errorf("remove %s: %w: %s", c.Path, err, out)
			report.Kept = append(report.Kept, Kept{Path: c.Path, Reason: strings.TrimSpace(string(out))})
			continue
		}
		touched[c.Repo] = true
		report.Removed = append(report.Removed, c.Path)
	}
	for repo := range touched {
		if out, err := p.git.Run(ctx, repo, "worktree", "prune"); err != nil {
			failures = fmt.Errorf("worktree prune in %s: %w: %s", repo, err, out)
		}
	}
	return report, failures
}

func Stale(candidates []Candidate, now time.Time, ttl time.Duration, keep map[string]bool) []Candidate {
	cutoff := now.Add(-ttl)
	var out []Candidate
	for _, c := range candidates {
		if keep[c.Path] || !c.Modified.Before(cutoff) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func scan(projectsDir, subdir string) ([]Candidate, error) {
	projects, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", projectsDir, err)
	}

	var out []Candidate
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		repo := filepath.Join(projectsDir, project.Name())
		root := filepath.Join(repo, filepath.FromSlash(subdir))
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() || !strings.HasPrefix(entry.Name(), reviewPrefix) {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			out = append(out, Candidate{
				Repo:     repo,
				Path:     filepath.Join(root, entry.Name()),
				Modified: info.ModTime(),
			})
		}
	}
	return out, nil
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func short(sha string) string {
	if len(sha) > shortSHALength {
		return sha[:shortSHALength]
	}
	return sha
}
