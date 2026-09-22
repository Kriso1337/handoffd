package mrref

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	ForgeGitLab = "gitlab"
	ForgeGitHub = "github"
)

var (
	mrPattern    = regexp.MustCompile(`https?://([^/\s]+)/([A-Za-z0-9._/-]+?)/-/merge_requests/(\d+)`)
	prPattern    = regexp.MustCompile(`https?://([^/\s]+)/([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)/pull/(\d+)`)
	jiraPattern  = regexp.MustCompile(`\b([A-Z][A-Z0-9]+-\d+)\b`)
	headChanged  = regexp.MustCompile(`(?i)\[HEAD CHANGED\][^\n]*`)
	reviewStart  = regexp.MustCompile(`(?i)\[REVIEW START\][^\n]*`)
	headLine     = regexp.MustCompile(`(?im)^[^\n]*\bHEAD\b[^\n]*$`)
	fullSHA      = regexp.MustCompile(`\b[0-9a-f]{40}\b`)
	shortSHA     = regexp.MustCompile(`\b[0-9a-f]{7,12}\b`)
	hasHexLetter = regexp.MustCompile(`[a-f]`)
)

const minSHALength = 7

func SameCommit(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	if len(a) < minSHALength || len(b) < minSHALength {
		return false
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

type MR struct {
	Host      string
	Namespace string
	Project   string
	IID       int
	Forge     string
}

func (m MR) IsGitHub() bool {
	return m.Forge == ForgeGitHub
}

func (m MR) ForgeName() string {
	if m.IsGitHub() {
		return "GitHub"
	}
	return "GitLab"
}

func (m MR) Slug() string {
	if m.IsGitHub() {
		return m.Project + "#" + strconv.Itoa(m.IID)
	}
	return m.Project + "!" + strconv.Itoa(m.IID)
}

func (m MR) URL() string {
	if m.IsGitHub() {
		return fmt.Sprintf("https://%s/%s/%s/pull/%d", m.Host, m.Namespace, m.Project, m.IID)
	}
	return fmt.Sprintf("https://%s/%s/-/merge_requests/%d", m.Host, m.Path(), m.IID)
}

func (m MR) HeadRef() string {
	if m.IsGitHub() {
		return fmt.Sprintf("refs/pull/%d/head", m.IID)
	}
	return fmt.Sprintf("refs/merge-requests/%d/head", m.IID)
}

func (m MR) LocalRef() string {
	return fmt.Sprintf("refs/handoffd/%s/%s/%d", m.Host, m.Path(), m.IID)
}

func (m MR) Path() string {
	return m.Namespace + "/" + m.Project
}

func (m MR) Key() string {
	return m.Host + "/" + m.Path() + "!" + strconv.Itoa(m.IID)
}

type Refs struct {
	MRs      []MR
	JiraKeys []string
	SHA      string
}

func Extract(text string) Refs {
	return Refs{
		MRs:      extractMRs(text),
		JiraKeys: extractJira(text),
		SHA:      extractSHA(text),
	}
}

func (r Refs) Primary() (MR, bool) {
	if len(r.MRs) == 0 {
		return MR{}, false
	}
	return r.MRs[0], true
}

type match struct {
	pos int
	mr  MR
}

func extractMRs(text string) []MR {
	var found []match
	for _, m := range mrPattern.FindAllStringSubmatchIndex(text, -1) {
		host, path, number := text[m[2]:m[3]], strings.Trim(text[m[4]:m[5]], "/"), text[m[6]:m[7]]
		iid, err := strconv.Atoi(number)
		if err != nil {
			continue
		}
		idx := strings.LastIndex(path, "/")
		if idx <= 0 || idx == len(path)-1 {
			continue
		}
		found = append(found, match{pos: m[0], mr: MR{Host: strings.ToLower(host), Namespace: path[:idx], Project: path[idx+1:], IID: iid, Forge: ForgeGitLab}})
	}
	for _, m := range prPattern.FindAllStringSubmatchIndex(text, -1) {
		host, owner, repo, number := text[m[2]:m[3]], text[m[4]:m[5]], text[m[6]:m[7]], text[m[8]:m[9]]
		iid, err := strconv.Atoi(number)
		if err != nil {
			continue
		}
		found = append(found, match{pos: m[0], mr: MR{Host: strings.ToLower(host), Namespace: owner, Project: repo, IID: iid, Forge: ForgeGitHub}})
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].pos < found[j].pos })
	var out []MR
	seen := map[string]bool{}
	for _, f := range found {
		if seen[f.mr.Key()] {
			continue
		}
		seen[f.mr.Key()] = true
		out = append(out, f.mr)
	}
	return out
}

func extractJira(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range jiraPattern.FindAllStringSubmatchIndex(text, -1) {
		key := text[m[2]:m[3]]
		if seen[key] || !jiraLinked(text, m[2], m[3]) {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}

func jiraLinked(text string, start, end int) bool {
	before, after := byte(' '), byte(' ')
	if start > 0 {
		before = text[start-1]
	}
	if end < len(text) {
		after = text[end]
	}
	return before == '/' || before == '[' || after == ']'
}

func LineSHA(text, marker string) string {
	if marker == "" {
		return ""
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(strings.ToLower(line), strings.ToLower(marker)) {
			return shaInLine(line, false)
		}
	}
	return ""
}

func extractSHA(text string) string {
	for _, pattern := range []*regexp.Regexp{headChanged, reviewStart, headLine} {
		for _, line := range pattern.FindAllString(text, -1) {
			if sha := shaInLine(line, pattern == headChanged); sha != "" {
				return sha
			}
		}
	}
	return ""
}

func shaInLine(line string, last bool) string {
	candidates := fullSHA.FindAllString(line, -1)
	if len(candidates) == 0 {
		for _, c := range shortSHA.FindAllString(line, -1) {
			if hasHexLetter.MatchString(c) {
				candidates = append(candidates, c)
			}
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	if last {
		return candidates[len(candidates)-1]
	}
	return candidates[0]
}
