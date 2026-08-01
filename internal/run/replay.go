package run

import (
	"context"
	"fmt"
	"os"

	"github.com/iamEtornam/mergejury/internal/cluster"
	"github.com/iamEtornam/mergejury/internal/finding"
	"github.com/iamEtornam/mergejury/internal/forge"
	"github.com/iamEtornam/mergejury/internal/packet"
	"github.com/iamEtornam/mergejury/internal/validate"
)

// Replay re-runs clustering, challenging, and judging against findings
// already in the store. No adapter invocations: judge prompt iteration for
// cents against a fixed corpus of real findings. Never posts.
func (o *Orchestrator) Replay(ctx context.Context, runID int64, opts Options) (*Summary, error) {
	r, err := o.Store.GetRun(runID)
	if err != nil {
		return nil, fmt.Errorf("run %d: %w", runID, err)
	}
	stored, err := o.Store.FindingsForRun(runID, true)
	if err != nil {
		return nil, err
	}

	// Rebuild the packet from git when the SHAs are reachable, so the
	// judge's re-anchor check and code context work. Otherwise trust the
	// stored anchors (they passed validation when the run happened).
	p := &packet.Packet{Repo: r.Repo, PRNumber: r.PRNumber, BaseSHA: r.BaseSHA, HeadSHA: r.HeadSHA}
	if dir, derr := os.Getwd(); derr == nil && r.BaseSHA != "" && r.HeadSHA != "" {
		if _, gerr := packet.Git(ctx, dir, "rev-parse", "--git-dir"); gerr == nil {
			if packet.EnsureCommit(ctx, dir, r.HeadSHA) == nil && packet.EnsureCommit(ctx, dir, r.BaseSHA) == nil {
				if diff, derr2 := packet.Git(ctx, dir, "diff", "--no-color", "--no-ext-diff", r.BaseSHA, r.HeadSHA); derr2 == nil {
					if files, perr := packet.ParseUnifiedDiff(diff); perr == nil {
						p.Files = o.filterSkipped(files)
						p.RepoDir = dir
					}
				}
			}
		}
	}
	p.Build()
	if p.RepoDir == "" {
		// No diff available: accept stored anchors as commentable.
		p.Commentable = map[string]map[int]bool{}
		for _, sf := range stored {
			if p.Commentable[sf.Finding.Path] == nil {
				p.Commentable[sf.Finding.Path] = map[int]bool{}
			}
			p.Commentable[sf.Finding.Path][sf.Finding.Line] = true
			if sf.Finding.StartLine != nil {
				p.Commentable[sf.Finding.Path][*sf.Finding.StartLine] = true
			}
		}
	}

	if err := o.Store.DeleteAdjudication(runID); err != nil {
		return nil, err
	}
	sum := &Summary{RunID: runID, AdapterStatus: map[string]string{}, Status: "replayed"}
	o.emit(runID, "replay_started", map[string]any{"findings": len(stored)})

	var items []cluster.Item
	for _, sf := range stored {
		if sf.DropReason == validate.ReasonUnanchored {
			continue // demoted body notes are not clustered
		}
		items = append(items, cluster.Item{FindingID: sf.ID, Finding: sf.Finding})
	}
	sum.Kept = len(items)

	var wts *packet.Worktrees
	if p.RepoDir != "" {
		if wts, err = packet.NewWorktrees(ctx, p.RepoDir); err == nil {
			defer wts.Cleanup()
		}
	}
	published, needsHuman, nClusters, cost, err := o.adjudicate(ctx, runID, p, items, opts, wts)
	if err != nil {
		return sum, err
	}
	sum.Clusters = nClusters
	sum.Published = len(published)
	sum.NeedsHuman = len(needsHuman)
	sum.TotalCostUSD = cost

	// Recompute what the event would have been, for comparison. Replay
	// cannot know adapter completeness better than the original run did.
	maxSev := finding.Severity("")
	for _, it := range published {
		if it.Severity.Rank() > maxSev.Rank() {
			maxSev = it.Severity
		}
	}
	event, reason := forge.ComputeEvent(forge.VerdictInput{
		Enabled:              o.Cfg.Verdict.Enabled && !opts.NoVerdict,
		RequestChangesAt:     o.Cfg.Verdict.RequestChangesAt,
		ApproveOnClean:       o.Cfg.Verdict.ApproveOnClean,
		ApproveForks:         o.Cfg.Verdict.ApproveForks,
		RunComplete:          r.Status == "completed",
		GatesPassed:          r.Status != "gated",
		PublishedMaxSeverity: maxSev,
		PublishedCount:       len(published),
		NeedsHumanCount:      len(needsHuman),
	})
	sum.Event, sum.EventReason = event, reason
	o.emit(runID, "replay_finished", sum)
	return sum, nil
}
