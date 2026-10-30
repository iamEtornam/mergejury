package run

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/iamEtornam/mergejury/internal/cluster"
	"github.com/iamEtornam/mergejury/internal/finding"
	"github.com/iamEtornam/mergejury/internal/forge"
	"github.com/iamEtornam/mergejury/internal/packet"
	"github.com/iamEtornam/mergejury/internal/store"
	"github.com/iamEtornam/mergejury/internal/validate"
)

// Replay re-runs clustering, challenging, and judging against findings
// already in the store. No adapter invocations: judge prompt iteration for
// cents against a fixed corpus of real findings. Results are saved as a new
// run; the source run and its posting/outcome history are never changed.
func (o *Orchestrator) Replay(ctx context.Context, runID int64, opts Options) (sum *Summary, err error) {
	r, err := o.Store.GetRun(runID)
	if err != nil {
		return nil, fmt.Errorf("run %d: %w", runID, err)
	}
	if r.Status == "running" {
		return nil, fmt.Errorf("run %d is still running", runID)
	}
	stored, err := o.Store.FindingsForRun(runID, false)
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

	replayID, err := o.Store.CreateRun(r.Repo, r.PRNumber, r.HeadSHA, r.BaseSHA, "replay", o.Cfg.Snapshot())
	if err != nil {
		return nil, err
	}
	sum = &Summary{RunID: replayID, AdapterStatus: map[string]string{}}
	defer func() {
		if err != nil {
			sum.Status = "failed"
			err = errors.Join(err, o.Store.FinishRun(replayID, "failed", "", fmt.Sprintf("replay of run %d: %v", runID, err), sum.TotalCostUSD))
			o.emit(replayID, "run_error", err.Error())
		}
	}()
	o.emit(replayID, "replay_started", map[string]any{"source_run_id": runID, "findings": len(stored)})

	// Copy the recorded inputs, without claiming another adapter execution.
	adapters, err := o.Store.AdapterRunsForRun(runID)
	if err != nil {
		return sum, err
	}
	adapterIDs := map[int64]int64{}
	for _, ar := range adapters {
		id, insertErr := o.Store.InsertAdapterRun(store.AdapterRun{
			RunID: replayID, AdapterID: ar.AdapterID, Lens: ar.Lens, Model: ar.Model,
			Status: ar.Status, RawOutput: ar.RawOutput, Error: ar.Error,
		})
		if insertErr != nil {
			return sum, insertErr
		}
		adapterIDs[ar.ID] = id
		sum.AdapterStatus[ar.AdapterID] = ar.Status
	}
	var items []cluster.Item
	var unanchored []forge.PublishedItem
	for _, sf := range stored {
		fid, insertErr := o.Store.InsertFinding(adapterIDs[sf.AdapterRunID], sf.Finding, sf.Kept, sf.DropReason)
		if insertErr != nil {
			return sum, insertErr
		}
		sum.Produced++
		if !sf.Kept {
			continue
		}
		sum.Kept++
		if sf.DropReason == validate.ReasonUnanchored {
			unanchored = append(unanchored, unresolvedFinding(sf.Finding))
			continue
		}
		items = append(items, cluster.Item{FindingID: fid, Finding: sf.Finding})
	}

	var wts *packet.Worktrees
	if p.RepoDir != "" {
		wts, err = packet.NewWorktrees(ctx, p.RepoDir)
		if err != nil {
			return sum, err
		}
		defer wts.Cleanup()
	}
	published, needsHuman, nClusters, cost, err := o.adjudicate(ctx, replayID, p, items, opts, wts)
	sum.TotalCostUSD = cost
	if err != nil {
		return sum, err
	}
	needsHuman = append(needsHuman, unanchored...)
	sum.Clusters = nClusters
	sum.Published = len(published)
	sum.NeedsHuman = len(needsHuman)
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
	sum.Event = event
	sum.EventReason = fmt.Sprintf("replay of run %d: %s", runID, reason)
	sum.Status = "completed"
	if r.Status != "completed" {
		sum.Status = "degraded"
	}
	if err = o.Store.FinishRun(replayID, sum.Status, event, sum.EventReason, sum.TotalCostUSD); err != nil {
		return sum, err
	}
	o.emit(replayID, "replay_finished", sum)
	return sum, nil
}
