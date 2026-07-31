package run

import (
	"context"
	"strings"
)

// SyncOutcomes polls posted reviews and records what happened to each
// comment: replies and dismissal. This is the feedback loop behind the
// scoreboard's precision numbers.
// ponytail: thread resolution state is GraphQL-only on GitHub; replies and
// dismissals are the REST-visible proxy. Add the GraphQL isResolved query
// when a few hundred comments exist and the precision numbers start to
// matter.
func (o *Orchestrator) SyncOutcomes(ctx context.Context, runID int64) error {
	if o.Forge == nil {
		return nil
	}
	r, err := o.Store.GetRun(runID)
	if err != nil {
		return err
	}
	if r.Repo == "" || r.PRNumber == 0 {
		return nil
	}
	comments, err := o.Forge.ListReviewComments(ctx, r.Repo, r.PRNumber)
	if err != nil {
		return err
	}
	replyCount := map[int64]int{}
	for _, c := range comments {
		if c.InReplyTo != 0 {
			replyCount[c.InReplyTo]++
		}
	}
	reviews, err := o.Forge.ListReviews(ctx, r.Repo, r.PRNumber)
	if err != nil {
		return err
	}
	dismissedReview := map[int64]bool{}
	for _, rev := range reviews {
		if strings.EqualFold(rev.State, "DISMISSED") {
			dismissedReview[rev.ID] = true
		}
	}
	verdicts, err := o.Store.VerdictsForRun(runID)
	if err != nil {
		return err
	}
	clusters, err := o.Store.ClustersForRun(runID)
	if err != nil {
		return err
	}
	clusterByID := map[int64]struct {
		path string
		line int
	}{}
	for _, c := range clusters {
		clusterByID[c.ID] = struct {
			path string
			line int
		}{c.Path, c.Line}
	}
	for _, v := range verdicts {
		if v.PostedCommentID == nil {
			continue
		}
		reviewID := *v.PostedCommentID // MarkVerdictPosted records the review id
		anchor := clusterByID[v.ClusterID]
		replies := 0
		for _, c := range comments {
			if c.ReviewID == reviewID && c.Path == anchor.path && c.Line == anchor.line {
				replies = replyCount[c.ID]
				break
			}
		}
		if err := o.Store.InsertOutcome(v.ID, "observed", false, dismissedReview[reviewID], replies); err != nil {
			return err
		}
	}
	return nil
}
