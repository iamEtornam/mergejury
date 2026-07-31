package forge

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"mergejury/internal/finding"
)

// PublishedItem is one judge-published cluster, ready to render.
type PublishedItem struct {
	VerdictID  int64
	Path       string
	Line       int
	StartLine  *int
	Severity   finding.Severity
	Title      string
	Body       string
	Patch      *string
	Supporters []string
	Dissenters []string
}

type AdapterStatusLine struct {
	ID     string
	Lens   string
	Status string // adapter.Status string
}

type ReviewInput struct {
	Repo        string
	PRNumber    int
	HeadSHA     string
	Event       string
	EventReason string

	Published  []PublishedItem
	NeedsHuman []PublishedItem // unanchored: body only
	BodyNotes  []string        // demoted findings and other unanchorable notes

	Adapters          []AdapterStatusLine
	MaxInline         int
	MinSeverity       finding.Severity
	FooterAttribution bool

	// Prior posted review on this PR by this identity, for idempotency and
	// supersession. Zero values when none.
	PriorHeadSHA  string
	PriorEvent    string
	PriorReviewID int64
}

type PostOutcome struct {
	ReviewID       int64
	UpdatedInPlace bool // same SHA + same event: body updated, no new review
	DismissedPrior bool
	Inline         []PublishedItem // what went inline
	Overflow       []PublishedItem // past the cap, into the body
	Body           string
}

// SelectComments applies the severity threshold and the inline cap: sort by
// severity then path then line, first MaxInline go inline, the rest into the
// body.
func SelectComments(items []PublishedItem, minSeverity finding.Severity, maxInline int) (inline, overflow []PublishedItem) {
	var kept []PublishedItem
	for _, it := range items {
		if it.Severity.Rank() >= minSeverity.Rank() {
			kept = append(kept, it)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].Severity.Rank() != kept[j].Severity.Rank() {
			return kept[i].Severity.Rank() > kept[j].Severity.Rank()
		}
		if kept[i].Path != kept[j].Path {
			return kept[i].Path < kept[j].Path
		}
		return kept[i].Line < kept[j].Line
	})
	if maxInline <= 0 {
		maxInline = 10
	}
	if len(kept) <= maxInline {
		return kept, nil
	}
	return kept[:maxInline], kept[maxInline:]
}

// RenderComment renders one inline comment: severity, body, optional
// suggestion, attribution footer.
func RenderComment(it PublishedItem, attribution bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**[%s]** %s\n\n%s\n", it.Severity, it.Title, it.Body)
	if it.Patch != nil && *it.Patch != "" {
		fmt.Fprintf(&b, "\n```suggestion\n%s\n```\n", strings.TrimSuffix(*it.Patch, "\n"))
	}
	if attribution {
		foot := "flagged by " + strings.Join(it.Supporters, ", ")
		if len(it.Dissenters) > 0 {
			foot += " · disputed by " + strings.Join(it.Dissenters, ", ")
		}
		fmt.Fprintf(&b, "\n<sub>%s</sub>\n", foot)
	}
	return b.String()
}

// RenderBody builds the review body: verdict and one-line summary first,
// then reviewer completion status, then unanchored needs-human items, then
// anything past the cap.
func RenderBody(in ReviewInput, inline, overflow []PublishedItem) string {
	var b strings.Builder
	switch in.Event {
	case EventApprove:
		fmt.Fprintf(&b, "**mergejury: approve.** %s\n", in.EventReason)
	case EventRequestChanges:
		fmt.Fprintf(&b, "**mergejury: changes requested.** %s\n", in.EventReason)
	default:
		fmt.Fprintf(&b, "**mergejury: comments.** %s\n", in.EventReason)
	}
	fmt.Fprintf(&b, "\n%d finding(s) published, %d inline.\n", len(inline)+len(overflow), len(inline))

	// Completion status: which reviewers finished, which failed and how.
	var okIDs, failed []string
	var lenses []string
	for _, a := range in.Adapters {
		if a.Status == "ok" {
			okIDs = append(okIDs, a.ID)
			lenses = append(lenses, fmt.Sprintf("%s (%s)", a.ID, a.Lens))
		} else {
			failed = append(failed, fmt.Sprintf("%s: %s", a.ID, a.Status))
		}
	}
	fmt.Fprintf(&b, "\n%d of %d reviewers completed.", len(okIDs), len(in.Adapters))
	if len(failed) > 0 {
		fmt.Fprintf(&b, " %s.", strings.Join(failed, ", "))
	}
	b.WriteString("\n")
	if in.Event == EventApprove {
		// An unexplained approval teaches people to ignore the bot.
		fmt.Fprintf(&b, "\nChecked by: %s. No findings survived validation and adjudication.\n", strings.Join(lenses, ", "))
	}

	if len(in.NeedsHuman) > 0 {
		b.WriteString("\n### Needs human review (could not be anchored)\n\n")
		for _, it := range in.NeedsHuman {
			fmt.Fprintf(&b, "- **[%s]** %s — near `%s:%d`: %s\n", it.Severity, it.Title, it.Path, it.Line, it.Body)
		}
	}
	if len(in.BodyNotes) > 0 {
		b.WriteString("\n### Notes\n\n")
		for _, n := range in.BodyNotes {
			fmt.Fprintf(&b, "- %s\n", n)
		}
	}
	if len(overflow) > 0 {
		fmt.Fprintf(&b, "\n<details><summary>%d more finding(s) past the inline cap</summary>\n\n", len(overflow))
		for _, it := range overflow {
			fmt.Fprintf(&b, "- **[%s]** `%s:%d` %s: %s\n", it.Severity, it.Path, it.Line, it.Title, it.Body)
		}
		b.WriteString("\n</details>\n")
	}
	return b.String()
}

// Post submits (or updates) the single review for a run, handling
// idempotency, supersession of a stale REQUEST_CHANGES, and 422 fallback.
func Post(ctx context.Context, f Forge, in ReviewInput) (PostOutcome, error) {
	inline, overflow := SelectComments(in.Published, in.MinSeverity, in.MaxInline)
	out := PostOutcome{Inline: inline, Overflow: overflow}
	body := RenderBody(in, inline, overflow)
	out.Body = body

	// Idempotency on head SHA: same SHA, same event -> update the existing
	// review instead of adding a second one. A changed verdict on the same
	// SHA submits a new review (the API cannot edit a submitted review's
	// event).
	if in.PriorReviewID != 0 && in.PriorHeadSHA == in.HeadSHA && in.PriorEvent == in.Event {
		if err := f.UpdateReviewBody(ctx, in.Repo, in.PRNumber, in.PriorReviewID, body); err != nil {
			return out, err
		}
		// ponytail: inline comments are not reconciled on same-SHA re-runs;
		// add per-comment PATCH reconciliation if prompt iteration against a
		// live PR becomes a real workflow.
		out.ReviewID = in.PriorReviewID
		out.UpdatedInPlace = true
		return out, nil
	}

	comments := make([]DraftComment, 0, len(inline))
	for _, it := range inline {
		comments = append(comments, DraftComment{
			Path: it.Path, Line: it.Line, StartLine: it.StartLine,
			Body: RenderComment(it, in.FooterAttribution),
		})
	}
	req := ReviewRequest{CommitID: in.HeadSHA, Body: body, Event: in.Event, Comments: comments}
	reviewID, err := f.CreateReview(ctx, in.Repo, in.PRNumber, req)
	if err != nil {
		se, is422 := err.(StatusError)
		if !is422 || se.StatusCode != 422 || len(comments) == 0 {
			return out, err
		}
		// The API is the final authority on what is commentable. A 422 does
		// not identify the offending comment reliably, so retry once with
		// every inline comment folded into the body.
		out.Overflow = append(out.Inline, out.Overflow...)
		out.Inline = nil
		body = RenderBody(in, nil, out.Overflow)
		out.Body = body
		reviewID, err = f.CreateReview(ctx, in.Repo, in.PRNumber, ReviewRequest{
			CommitID: in.HeadSHA, Body: body, Event: in.Event,
		})
		if err != nil {
			return out, fmt.Errorf("retry after 422 also failed: %w", err)
		}
	}
	out.ReviewID = reviewID

	// Supersession: a REQUEST_CHANGES state is sticky. If the prior review
	// requested changes and this one does not, an APPROVE supersedes by
	// itself; a COMMENT does not, so the stale objection must be dismissed
	// explicitly or the PR stays blocked.
	if in.PriorReviewID != 0 && in.PriorEvent == EventRequestChanges &&
		in.Event != EventRequestChanges && in.Event != EventApprove {
		msg := fmt.Sprintf("Superseded by mergejury run against head %s.", in.HeadSHA)
		if err := f.DismissReview(ctx, in.Repo, in.PRNumber, in.PriorReviewID, msg); err != nil {
			return out, fmt.Errorf("review posted but stale REQUEST_CHANGES not dismissed: %w", err)
		}
		out.DismissedPrior = true
	}
	return out, nil
}
