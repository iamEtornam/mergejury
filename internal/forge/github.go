package forge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/google/go-github/v68/github"
)

// GitHub implements Forge over the REST API. The token must be a
// narrowly-scoped credential for a dedicated machine user or App
// installation; reviewers never see it (section 10).
type GitHub struct {
	client *github.Client
}

// NewGitHubFromEnv reads GITHUB_TOKEN (or GH_TOKEN). Credentials come from
// the environment, never CLI arguments.
func NewGitHubFromEnv() (*GitHub, error) {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		tok = os.Getenv("GH_TOKEN")
	}
	if tok == "" {
		return nil, errors.New("GITHUB_TOKEN is not set")
	}
	c := github.NewClient(nil).WithAuthToken(tok)
	if base := os.Getenv("GITHUB_API_URL"); base != "" {
		var err error
		c, err = c.WithEnterpriseURLs(base, base)
		if err != nil {
			return nil, err
		}
	}
	return &GitHub{client: c}, nil
}

func splitRepo(repo string) (owner, name string, err error) {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("repo %q is not owner/name", repo)
	}
	return parts[0], parts[1], nil
}

func wrapErr(err error) error {
	var ghErr *github.ErrorResponse
	if errors.As(err, &ghErr) && ghErr.Response != nil {
		return StatusError{StatusCode: ghErr.Response.StatusCode, Msg: err.Error()}
	}
	return err
}

func (g *GitHub) FetchPR(ctx context.Context, repo string, number int) (*PR, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	pr, _, err := g.client.PullRequests.Get(ctx, owner, name, number)
	if err != nil {
		return nil, wrapErr(err)
	}
	out := &PR{
		Repo:    repo,
		Number:  number,
		Title:   pr.GetTitle(),
		Body:    pr.GetBody(),
		BaseSHA: pr.GetBase().GetSHA(),
		HeadSHA: pr.GetHead().GetSHA(),
		Author:  pr.GetUser().GetLogin(),
	}
	baseRepo := pr.GetBase().GetRepo().GetFullName()
	headRepo := pr.GetHead().GetRepo().GetFullName()
	out.IsFork = headRepo != "" && headRepo != baseRepo
	out.CloneURL = pr.GetBase().GetRepo().GetCloneURL()

	opts := &github.ListOptions{PerPage: 100}
	for {
		files, resp, err := g.client.PullRequests.ListFiles(ctx, owner, name, number, opts)
		if err != nil {
			return nil, wrapErr(err)
		}
		for _, f := range files {
			out.Files = append(out.Files, PRFile{
				Path:     f.GetFilename(),
				PrevPath: f.GetPreviousFilename(),
				Status:   f.GetStatus(),
				Patch:    f.GetPatch(),
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

func (g *GitHub) Viewer(ctx context.Context) (string, error) {
	u, _, err := g.client.Users.Get(ctx, "")
	if err != nil {
		return "", wrapErr(err)
	}
	return u.GetLogin(), nil
}

func (g *GitHub) CreateReview(ctx context.Context, repo string, number int, req ReviewRequest) (int64, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return 0, err
	}
	comments := make([]*github.DraftReviewComment, 0, len(req.Comments))
	for _, c := range req.Comments {
		dc := &github.DraftReviewComment{
			Path: github.Ptr(c.Path),
			Line: github.Ptr(c.Line),
			Side: github.Ptr("RIGHT"),
			Body: github.Ptr(c.Body),
		}
		if c.StartLine != nil {
			dc.StartLine = github.Ptr(*c.StartLine)
			dc.StartSide = github.Ptr("RIGHT")
		}
		comments = append(comments, dc)
	}
	review, _, err := g.client.PullRequests.CreateReview(ctx, owner, name, number, &github.PullRequestReviewRequest{
		CommitID: github.Ptr(req.CommitID),
		Body:     github.Ptr(req.Body),
		Event:    github.Ptr(req.Event),
		Comments: comments,
	})
	if err != nil {
		return 0, wrapErr(err)
	}
	return review.GetID(), nil
}

func (g *GitHub) UpdateReviewBody(ctx context.Context, repo string, number int, reviewID int64, body string) error {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return err
	}
	_, _, err = g.client.PullRequests.UpdateReview(ctx, owner, name, number, reviewID, body)
	return wrapErr(err)
}

func (g *GitHub) ListReviews(ctx context.Context, repo string, number int) ([]Review, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	var out []Review
	opts := &github.ListOptions{PerPage: 100}
	for {
		reviews, resp, err := g.client.PullRequests.ListReviews(ctx, owner, name, number, opts)
		if err != nil {
			return nil, wrapErr(err)
		}
		for _, r := range reviews {
			out = append(out, Review{ID: r.GetID(), State: r.GetState(), Body: r.GetBody()})
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

func (g *GitHub) DismissReview(ctx context.Context, repo string, number int, reviewID int64, message string) error {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return err
	}
	_, _, err = g.client.PullRequests.DismissReview(ctx, owner, name, number, reviewID, &github.PullRequestReviewDismissalRequest{
		Message: github.Ptr(message),
	})
	return wrapErr(err)
}

func (g *GitHub) ListReviewComments(ctx context.Context, repo string, number int) ([]ReviewComment, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	var out []ReviewComment
	opts := &github.PullRequestListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	for {
		comments, resp, err := g.client.PullRequests.ListComments(ctx, owner, name, number, opts)
		if err != nil {
			return nil, wrapErr(err)
		}
		for _, c := range comments {
			out = append(out, ReviewComment{
				ID:        c.GetID(),
				ReviewID:  c.GetPullRequestReviewID(),
				Path:      c.GetPath(),
				Line:      c.GetLine(),
				Body:      c.GetBody(),
				InReplyTo: c.GetInReplyTo(),
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}
