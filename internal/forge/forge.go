package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/Kriso1337/handoffd/internal/mrref"
)

type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type Details struct {
	Number       int
	State        string
	TargetBranch string
	HeadSHA      string
	BaseSHA      string
}

type Client struct {
	run  Runner
	glab string
	gh   string
}

func New(run Runner, glab, gh string) Client {
	return Client{run: run, glab: glab, gh: gh}
}

type gitlabMergeRequest struct {
	IID          int    `json:"iid"`
	State        string `json:"state"`
	TargetBranch string `json:"target_branch"`
	SHA          string `json:"sha"`
	DiffRefs     *struct {
		BaseSHA string `json:"base_sha"`
		HeadSHA string `json:"head_sha"`
	} `json:"diff_refs"`
}

type githubPullRequest struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Base   struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
	Head struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

func (c Client) Details(ctx context.Context, mr mrref.MR) (Details, error) {
	if mr.IsGitHub() {
		return c.github(ctx, mr)
	}
	return c.gitlab(ctx, mr)
}

func (c Client) gitlab(ctx context.Context, mr mrref.MR) (Details, error) {
	if c.glab == "" {
		return Details{}, errors.New("glab is not configured")
	}
	endpoint := "projects/" + url.PathEscape(mr.Path()) + "/merge_requests/" + strconv.Itoa(mr.IID)
	out, err := c.run.Run(ctx, c.glab, "api", "--hostname", mr.Host, endpoint)
	if err != nil {
		return Details{}, fmt.Errorf("glab api %s: %w: %s", endpoint, err, strings.TrimSpace(string(out)))
	}
	var raw gitlabMergeRequest
	if err := json.Unmarshal(out, &raw); err != nil {
		return Details{}, fmt.Errorf("glab api %s: parse: %w: %s", endpoint, err, strings.TrimSpace(string(out)))
	}
	info := Details{Number: raw.IID, State: raw.State, TargetBranch: raw.TargetBranch, HeadSHA: raw.SHA}
	if raw.DiffRefs != nil {
		info.BaseSHA = raw.DiffRefs.BaseSHA
		if info.HeadSHA == "" {
			info.HeadSHA = raw.DiffRefs.HeadSHA
		}
	}
	return info, nil
}

func (c Client) github(ctx context.Context, mr mrref.MR) (Details, error) {
	if c.gh == "" {
		return Details{}, errors.New("gh is not configured")
	}
	endpoint := "repos/" + mr.Namespace + "/" + mr.Project + "/pulls/" + strconv.Itoa(mr.IID)
	out, err := c.run.Run(ctx, c.gh, "api", "--hostname", mr.Host, endpoint)
	if err != nil {
		return Details{}, fmt.Errorf("gh api %s: %w: %s", endpoint, err, strings.TrimSpace(string(out)))
	}
	var raw githubPullRequest
	if err := json.Unmarshal(out, &raw); err != nil {
		return Details{}, fmt.Errorf("gh api %s: parse: %w: %s", endpoint, err, strings.TrimSpace(string(out)))
	}
	return Details{Number: raw.Number, State: raw.State, TargetBranch: raw.Base.Ref, HeadSHA: raw.Head.SHA, BaseSHA: raw.Base.SHA}, nil
}

func (c Client) Note(ctx context.Context, mr mrref.MR, text string) error {
	if mr.IsGitHub() {
		if c.gh == "" {
			return errors.New("gh is not configured")
		}
		repo := mr.Host + "/" + mr.Namespace + "/" + mr.Project
		out, err := c.run.Run(ctx, c.gh, "pr", "comment", strconv.Itoa(mr.IID), "--repo", repo, "--body", text)
		if err != nil {
			return fmt.Errorf("gh pr comment %s#%d: %w: %s", repo, mr.IID, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if c.glab == "" {
		return errors.New("glab is not configured")
	}
	repo := "https://" + mr.Host + "/" + mr.Path()
	out, err := c.run.Run(ctx, c.glab, "mr", "note", strconv.Itoa(mr.IID), "--repo", repo, "--message", text)
	if err != nil {
		return fmt.Errorf("glab mr note %s!%d: %w: %s", mr.Path(), mr.IID, err, strings.TrimSpace(string(out)))
	}
	return nil
}
