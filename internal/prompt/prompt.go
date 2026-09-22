package prompt

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

//go:embed templates/*/*.tmpl
var defaults embed.FS

var Languages = []string{"en"}

const (
	continuationTextLimit = 1200
	shortSHALength        = 12
)

var names = []string{"initial", "continuation", "triage", "ask", "results", "review_done"}

type Persona struct {
	Owner             string `json:"owner"`
	OwnerGenitive     string `json:"owner_genitive"`
	OwnerDative       string `json:"owner_dative"`
	OwnerFull         string `json:"owner_full"`
	OwnerFullGenitive string `json:"owner_full_genitive"`
	Agent             string `json:"agent"`
	Approvers         string `json:"approvers"`
	MainModel         string `json:"main_model"`
	TriageModel       string `json:"triage_model"`
	ThreadTool        string `json:"thread_tool"`
	Capabilities      string `json:"capabilities"`
}

type Context struct {
	Workspace      string
	SelfUserID     string
	Channel        string
	ChannelName    string
	AuthorID       string
	AuthorName     string
	MsgTS          string
	ThreadTS       string
	Text           string
	IsDM           bool
	Worktree       string
	WorktreeSHA    string
	WorktreeNote   string
	WorktreeDirty  bool
	HeadSHA        string
	BaseSHA        string
	TargetBranch   string
	MergeRef       string
	SHA            string
	Forge          string
	ReviewEnabled  bool
	ReviewMode     bool
	Resumed        bool
	Macro          bool
	VerdictMissing bool
	Committee      Committee
	PostBin        string
	Round          int
	NewRound       bool
}

type ResultRow struct {
	Agent     string
	Submitted bool
	Blockers  int
	Others    int
	Decision  string
	Notes     []string
}

type ResultsContext struct {
	Root    string
	Round   int
	HeadSHA string
	PostBin string
	Rows    []ResultRow
}

type ReviewDone struct {
	SHA         string
	Blockers    int
	Others      int
	Decision    string
	FindingsURL string
	Agent       string
	Stale       bool
	StaleRound  int
	StaleHead   string
}

type Committee struct {
	Agent      string
	Peers      string
	Driver     bool
	DriverName string
}

type TailReply struct {
	AuthorName string
	AuthorID   string
	Text       string
}

type Example struct {
	Text  string
	React bool
}

type TriageContext struct {
	Context
	ChannelLabel string
	IsDM         bool
	SelfInThread bool
	ThreadSize   int
	ThreadTail   []TailReply
	VerdictPath  string
	Namesake     string
	PeerAgents   []string
	Examples     []Example
	RootAuthor   string
	RootText     string
	LastFromSelf bool
}

type Renderer struct {
	persona Persona
	tmpl    *template.Template
}

func New(persona Persona, overrideDir string) (Renderer, error) {
	return NewLocalized(persona, overrideDir, "en")
}

func NewLocalized(persona Persona, overrideDir, language string) (Renderer, error) {
	if !supported(language) {
		return Renderer{}, fmt.Errorf("prompt language %q is not supported (%s)", language, strings.Join(Languages, ", "))
	}
	root := template.New("prompts").Option("missingkey=error")
	for _, name := range names {
		text, err := source(name, overrideDir, language)
		if err != nil {
			return Renderer{}, err
		}
		if _, err := root.New(name).Parse(text); err != nil {
			return Renderer{}, fmt.Errorf("parse prompt template %s: %w", name, err)
		}
	}
	return Renderer{persona: persona, tmpl: root}, nil
}

func supported(language string) bool {
	for _, l := range Languages {
		if l == language {
			return true
		}
	}
	return false
}

func source(name, overrideDir, language string) (string, error) {
	if overrideDir != "" {
		raw, err := os.ReadFile(filepath.Join(overrideDir, name+".tmpl"))
		if err == nil {
			return string(raw), nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("read prompt override %s: %w", name, err)
		}
	}
	raw, err := defaults.ReadFile("templates/" + language + "/" + name + ".tmpl")
	if err != nil {
		return "", fmt.Errorf("embedded prompt template %s/%s: %w", language, name, err)
	}
	return string(raw), nil
}

func Permalink(workspace, channel, ts string) string {
	return fmt.Sprintf("https://%s.slack.com/archives/%s/p%s",
		workspace, channel, strings.ReplaceAll(ts, ".", ""))
}

type common struct {
	Context
	Persona          Persona
	Author           string
	Permalink        string
	Behind           bool
	SHAMismatch      bool
	WorktreeSHAShort string
	HeadSHAShort     string
	BaseSHAShort     string
	RefreshCommand   string
}

func (r Renderer) common(c Context) common {
	return common{
		Context:          c,
		Persona:          r.persona,
		Author:           author(c),
		Permalink:        Permalink(c.Workspace, c.Channel, c.MsgTS),
		Behind:           c.HeadSHA != "" && c.HeadSHA != c.WorktreeSHA,
		SHAMismatch:      c.SHA != "" && c.SHA != c.WorktreeSHA,
		WorktreeSHAShort: short(c.WorktreeSHA),
		HeadSHAShort:     short(c.HeadSHA),
		BaseSHAShort:     short(c.BaseSHA),
		RefreshCommand:   refreshCommand(c),
	}
}

func refreshCommand(c Context) string {
	if c.MergeRef == "" {
		return ""
	}
	target := "FETCH_HEAD"
	if c.HeadSHA != "" {
		target = c.HeadSHA
	}
	return fmt.Sprintf("git fetch origin %s && git checkout --detach %s", c.MergeRef, target)
}

func (r Renderer) Initial(c Context) (string, error) {
	return r.render("initial", r.common(c))
}

type continuationData struct {
	common
	Flattened  string
	ReviewMode bool
}

func (r Renderer) Continuation(c Context, reviewMode bool) (string, error) {
	out, err := r.render("continuation", continuationData{
		common:     r.common(c),
		Flattened:  flatten(c.Text),
		ReviewMode: reviewMode,
	})
	if err != nil {
		return "", err
	}
	return strings.Join(strings.Fields(out), " "), nil
}

type triageData struct {
	common
	ChannelLabel string
	IsDM         bool
	SelfInThread bool
	ThreadSize   int
	ThreadTail   []TailReply
	VerdictPath  string
	Namesake     string
	PeerAgents   string
	Examples     []Example
	RootAuthor   string
	RootText     string
	LastFromSelf bool
}

func (r Renderer) Triage(c TriageContext) (string, error) {
	tail := make([]TailReply, 0, len(c.ThreadTail))
	for _, reply := range c.ThreadTail {
		reply.Text = flatten(reply.Text)
		tail = append(tail, reply)
	}
	examples := make([]Example, 0, len(c.Examples))
	for _, ex := range c.Examples {
		ex.Text = flatten(ex.Text)
		examples = append(examples, ex)
	}
	return r.render("triage", triageData{
		common:       r.common(c.Context),
		ChannelLabel: c.ChannelLabel,
		IsDM:         c.IsDM,
		SelfInThread: c.SelfInThread,
		ThreadSize:   c.ThreadSize,
		ThreadTail:   tail,
		VerdictPath:  c.VerdictPath,
		Namesake:     c.Namesake,
		PeerAgents:   strings.Join(c.PeerAgents, ", "),
		Examples:     examples,
		RootAuthor:   c.RootAuthor,
		RootText:     flatten(c.RootText),
		LastFromSelf: c.LastFromSelf,
	})
}

type askData struct {
	common
	Task   string
	Reason string
}

func (r Renderer) Ask(c Context, task, reason string) (string, error) {
	return r.render("ask", askData{common: r.common(c), Task: task, Reason: reason})
}

type resultsData struct {
	ResultsContext
	Persona      Persona
	HeadSHAShort string
}

func (r Renderer) Results(c ResultsContext) (string, error) {
	return r.render("results", resultsData{ResultsContext: c, Persona: r.persona, HeadSHAShort: short(c.HeadSHA)})
}

type reviewDoneData struct {
	ReviewDone
	StaleHeadShort string
}

func (r Renderer) ReviewDone(d ReviewDone) (string, error) {
	out, err := r.render("review_done", reviewDoneData{ReviewDone: d, StaleHeadShort: short(d.StaleHead)})
	if err != nil {
		return "", err
	}
	return strings.Join(strings.Fields(out), " "), nil
}

func (r Renderer) render(name string, data any) (string, error) {
	var buf bytes.Buffer
	if err := r.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		return "", fmt.Errorf("render prompt %s: %w", name, err)
	}
	return buf.String(), nil
}

func author(c Context) string {
	if c.AuthorName != "" {
		return c.AuthorName
	}
	return c.AuthorID
}

func short(sha string) string {
	if len(sha) > shortSHALength {
		return sha[:shortSHALength]
	}
	return sha
}

func flatten(text string) string {
	collapsed := strings.Join(strings.Fields(strings.ReplaceAll(text, "\n", " ⏎ ")), " ")
	runes := []rune(collapsed)
	if len(runes) <= continuationTextLimit {
		return collapsed
	}
	return string(runes[:continuationTextLimit]) + "…"
}
