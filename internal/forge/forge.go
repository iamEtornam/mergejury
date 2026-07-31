// Package forge talks to GitHub: PR fetch, review posting, supersession.
// The API surface is an interface so the poster is testable against a fake,
// and so another forge is addable without touching the pipeline.
package forge

import "context"

type PRFile struct {
	Path     string
	PrevPath string // set on rename
	Status   string // added | removed | modified | renamed
	Patch    string // GitHub's per-file patch body (may be empty for binary)
}

type PR struct {
	Repo     string // owner/name
	Number   int
	Title    string
	Body     string
	BaseSHA  string
	HeadSHA  string
	IsFork   bool
	Author   string
	CloneURL string
	Files    []PRFile
}

type DraftComment struct {
	Path      string
	Line      int
	StartLine *int // nil for single-line; side is always RIGHT
	Body      string
}

type ReviewRequest struct {
	CommitID string
	Body     string
	Event    string // APPROVE | REQUEST_CHANGES | COMMENT
	Comments []DraftComment
}

type Review struct {
	ID    int64
	State string // APPROVED | CHANGES_REQUESTED | COMMENTED | DISMISSED
	Body  string
}

type ReviewComment struct {
	ID        int64
	ReviewID  int64
	Path      string
	Line      int
	Body      string
	InReplyTo int64
}

// StatusError carries the HTTP status of a failed forge call so the poster
// can distinguish a 422 (invalid comment anchor) from everything else.
type StatusError struct {
	StatusCode int
	Msg        string
}

func (e StatusError) Error() string { return e.Msg }

type Forge interface {
	FetchPR(ctx context.Context, repo string, number int) (*PR, error)
	// Viewer returns the authenticated login, to find our own prior reviews.
	Viewer(ctx context.Context) (string, error)
	CreateReview(ctx context.Context, repo string, number int, req ReviewRequest) (reviewID int64, err error)
	UpdateReviewBody(ctx context.Context, repo string, number int, reviewID int64, body string) error
	ListReviews(ctx context.Context, repo string, number int) ([]Review, error)
	DismissReview(ctx context.Context, repo string, number int, reviewID int64, message string) error
	ListReviewComments(ctx context.Context, repo string, number int) ([]ReviewComment, error)
}
