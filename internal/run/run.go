// Package run orchestrates a review: packet, adapter fan-out, validation,
// clustering, challenge, verification, judging, verdict, posting. The CLI
// and the server both drive this same orchestrator; if they ever diverge in
// behaviour, one of them is wrong.
package run

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"revu/internal/adapter"
	"revu/internal/adjudicate"
	"revu/internal/cluster"
	"revu/internal/config"
	"revu/internal/finding"
	"revu/internal/forge"
	"revu/internal/packet"
	"revu/internal/store"
	"revu/internal/validate"
	"revu/internal/verify"
	"revu/prompts"
)

type Options struct {
	Adapters     []string // subset of configured adapter IDs; empty = all
	DryRun       bool
	NoChallenger bool
	NoVerdict    bool
	MaxComments  int // 0 = config value
	Trigger      string
}

type Orchestrator struct {
	Cfg     config.Config
	Store   *store.Store
	Prompts *prompts.Set
	Forge   forge.Forge       // nil for local-only use
	Client  *adapter.AnthropicClient
	Runner  adapter.Runner
	Events  *Broker // may be nil
}

// Summary is the machine-readable result for --json and the API.
type Summary struct {
	RunID          int64             `json:"run_id"`
	Status         string            `json:"status"` // completed | degraded | failed | gated
	Event          string            `json:"review_event,omitempty"`
	EventReason    string            `json:"review_event_reason,omitempty"`
	AdapterStatus  map[string]string `json:"adapter_status"`
	Produced       int               `json:"findings_produced"`
	Kept           int               `json:"findings_kept"`
	Clusters       int               `json:"clusters"`
	Published      int               `json:"published"`
	NeedsHuman     int               `json:"needs_human"`
	Posted         bool              `json:"posted"`
	ReviewID       int64             `json:"review_id,omitempty"`
	TotalCostUSD   float64           `json:"total_cost_usd"`
	RenderedReview string            `json:"rendered_review,omitempty"` // dry-run output
}

func (o *Orchestrator) emit(runID int64, typ string, payload any) {
	if o.Events != nil {
		o.Events.Publish(Event{RunID: runID, Type: typ, Payload: payload, At: time.Now().UTC()})
	}
}

func (o *Orchestrator) selectAdapters(opts Options) ([]config.Adapter, error) {
	if len(opts.Adapters) == 0 {
		return o.Cfg.Adapters, nil
	}
	want := map[string]bool{}
	for _, id := range opts.Adapters {
		want[id] = true
	}
	var out []config.Adapter
	for _, a := range o.Cfg.Adapters {
		if want[a.ID] {
			out = append(out, a)
			delete(want, a.ID)
		}
	}
	if len(want) > 0 {
		var missing []string
		for id := range want {
			missing = append(missing, id)
		}
		return nil, fmt.Errorf("unknown adapter id(s): %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// matchGlob supports * and ** the way gates.skip_paths needs.
func matchGlob(pattern, p string) bool {
	if !strings.Contains(pattern, "**") {
		ok, _ := path.Match(pattern, p)
		return ok
	}
	// Split on ** and match the pieces in order.
	parts := strings.Split(pattern, "**")
	rest := p
	for i, part := range parts {
		part = strings.Trim(part, "/")
		if part == "" {
			continue
		}
		if i == 0 {
			if !strings.HasPrefix(rest, part) {
				// leading fixed prefix must anchor
				ok, _ := path.Match(part, firstSeg(rest))
				if !ok {
					return false
				}
			}
			rest = rest[min(len(part), len(rest)):]
			continue
		}
		if i == len(parts)-1 {
			// trailing part matches the basename or a suffix segment
			base := path.Base(p)
			ok, _ := path.Match(part, base)
			if ok {
				return true
			}
			return strings.HasSuffix(p, "/"+part) || rest == part
		}
		// Middle parts must match at a path-segment boundary so that
		// "**/vendor/**" does not skip "myvendor/x.go".
		idx := strings.Index(rest, part)
		for idx > 0 && rest[idx-1] != '/' {
			next := strings.Index(rest[idx+1:], part)
			if next < 0 {
				idx = -1
				break
			}
			idx += 1 + next
		}
		if idx < 0 {
			return false
		}
		rest = rest[idx+len(part):]
	}
	return true
}

func firstSeg(p string) string {
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return p
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (o *Orchestrator) filterSkipped(files []packet.FileDiff) []packet.FileDiff {
	var out []packet.FileDiff
	for _, f := range files {
		skip := false
		for _, pat := range o.Cfg.Gates.SkipPaths {
			if matchGlob(pat, f.Path) {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, f)
		}
	}
	return out
}

// ReviewPR runs the full pipeline against a GitHub PR.
func (o *Orchestrator) ReviewPR(ctx context.Context, repo string, number int, opts Options) (*Summary, error) {
	if o.Forge == nil {
		return nil, fmt.Errorf("no forge configured (is GITHUB_TOKEN set?)")
	}
	pr, err := o.Forge.FetchPR(ctx, repo, number)
	if err != nil {
		return nil, fmt.Errorf("fetch PR: %w", err)
	}
	p := &packet.Packet{
		Repo: repo, PRNumber: number,
		Title: pr.Title, Body: pr.Body,
		BaseSHA: pr.BaseSHA, HeadSHA: pr.HeadSHA, IsFork: pr.IsFork,
	}
	for _, f := range pr.Files {
		fd := packet.FileDiff{Path: f.Path, OldPath: f.PrevPath}
		switch f.Status {
		case "added":
			fd.Status = packet.StatusAdded
		case "removed":
			fd.Status = packet.StatusDeleted
		case "renamed":
			fd.Status = packet.StatusRenamed
		default:
			fd.Status = packet.StatusModified
		}
		if f.Patch == "" {
			fd.Binary = fd.Status != packet.StatusRenamed // no patch + not a pure rename = binary or huge
		} else {
			hunks, err := packet.ParsePatchHunks(f.Patch)
			if err != nil {
				return nil, fmt.Errorf("parse patch for %s: %w", f.Path, err)
			}
			fd.Hunks = hunks
		}
		p.Files = append(p.Files, fd)
	}
	p.Files = o.filterSkipped(p.Files)
	p.Build()

	// A local checkout enables worktrees, evidence checks, and mechanical
	// verification. Optional: modelapi works without one.
	if dir, err := os.Getwd(); err == nil {
		if _, gerr := packet.Git(ctx, dir, "rev-parse", "--git-dir"); gerr == nil {
			if packet.EnsureCommit(ctx, dir, p.HeadSHA) == nil {
				p.RepoDir = dir
			}
		}
	}
	return o.execute(ctx, p, opts, false)
}

// ReviewLocal reviews the working tree against a base ref. No PR, no
// posting: the fast dev loop.
func (o *Orchestrator) ReviewLocal(ctx context.Context, repoDir, baseRef string, opts Options) (*Summary, error) {
	if baseRef == "" {
		baseRef = "main"
	}
	diff, err := packet.LocalDiff(ctx, repoDir, baseRef)
	if err != nil {
		return nil, err
	}
	files, err := packet.ParseUnifiedDiff(diff)
	if err != nil {
		return nil, err
	}
	p := &packet.Packet{
		Title:   "local working tree vs " + baseRef,
		RepoDir: repoDir,
	}
	p.Files = o.filterSkipped(files)
	p.Build()
	opts.DryRun = true // local runs never post
	return o.execute(ctx, p, opts, true)
}

type adapterOutcome struct {
	cfg    config.Adapter
	result adapter.Result
	arID   int64
}

func (o *Orchestrator) execute(ctx context.Context, p *packet.Packet, opts Options, local bool) (*Summary, error) {
	adapters, err := o.selectAdapters(opts)
	if err != nil {
		return nil, err
	}
	if len(adapters) == 0 {
		return nil, fmt.Errorf("no adapters configured")
	}
	if opts.Trigger == "" {
		opts.Trigger = "cli"
	}

	runID, err := o.Store.CreateRun(p.Repo, p.PRNumber, p.HeadSHA, p.BaseSHA, opts.Trigger, o.Cfg.Snapshot())
	if err != nil {
		return nil, err
	}
	sum := &Summary{RunID: runID, AdapterStatus: map[string]string{}}
	o.emit(runID, "run_started", map[string]any{"repo": p.Repo, "pr": p.PRNumber, "head_sha": p.HeadSHA})

	// Gates: five agentic reviewers on a two-thousand-line PR is real money.
	if g := o.Cfg.Gates; (g.MaxChangedLines > 0 && p.ChangedLines > g.MaxChangedLines) ||
		(g.MaxChangedFiles > 0 && p.ChangedFiles > g.MaxChangedFiles) {
		reason := fmt.Sprintf("skipped: %d changed lines across %d files exceeds gates (max %d lines, %d files)",
			p.ChangedLines, p.ChangedFiles, g.MaxChangedLines, g.MaxChangedFiles)
		sum.Status = "gated"
		sum.EventReason = reason
		if !opts.DryRun && o.Forge != nil {
			body := "**revu: skipped.** " + reason
			if _, err := o.Forge.CreateReview(ctx, p.Repo, p.PRNumber, forge.ReviewRequest{
				CommitID: p.HeadSHA, Body: body, Event: forge.EventComment,
			}); err != nil {
				o.Store.FinishRun(runID, "gated", "", reason, 0)
				return sum, err
			}
			sum.Event = forge.EventComment
		}
		o.Store.FinishRun(runID, "gated", sum.Event, reason, 0)
		o.emit(runID, "run_finished", sum)
		return sum, nil
	}

	// Worktrees: one per adapter at head SHA. Local mode reviews the working
	// tree itself (uncommitted changes are the point), so no worktrees.
	var wts *packet.Worktrees
	if !local && p.RepoDir != "" {
		wts, err = packet.NewWorktrees(ctx, p.RepoDir)
		if err != nil {
			return nil, err
		}
		defer wts.Cleanup()
	}

	// Fan out.
	outcomes := make([]adapterOutcome, len(adapters))
	g, gctx := errgroup.WithContext(ctx)
	for i, acfg := range adapters {
		g.Go(func() error {
			outcomes[i] = o.runAdapter(gctx, runID, acfg, p, wts, local)
			return nil
		})
	}
	g.Wait()

	var totalCost float64
	allOK := true
	var allFindings []finding.Finding
	findingIdx := map[int]int64{} // index in allFindings -> adapter_run id
	for _, oc := range outcomes {
		sum.AdapterStatus[oc.cfg.ID] = string(oc.result.Status)
		totalCost += oc.result.CostUSD
		if oc.result.Status != adapter.StatusOK {
			allOK = false
		}
		for _, f := range oc.result.Findings {
			findingIdx[len(allFindings)] = oc.arID
			allFindings = append(allFindings, f)
		}
	}
	sum.Produced = len(allFindings)

	// Validation: deterministic, before anything reaches the judge.
	var counter validate.FileLineCounter
	if p.RepoDir != "" {
		sha := p.HeadSHA
		if local {
			sha = "" // working tree
		}
		counter = &validate.GitLineCounter{RepoDir: p.RepoDir, SHA: sha}
	}
	vOutcomes := validate.Run(ctx, p, counter, allFindings)
	var items []cluster.Item
	var bodyNotes []string
	for i, vo := range vOutcomes {
		fid, err := o.Store.InsertFinding(findingIdx[i], vo.Finding, vo.Kept, vo.DropReason)
		if err != nil {
			return sum, err
		}
		if !vo.Kept {
			continue
		}
		sum.Kept++
		if vo.Demoted {
			bodyNotes = append(bodyNotes, fmt.Sprintf("(unanchorable, from %s) **[%s]** `%s:%d` %s",
				vo.Finding.ReviewerID, vo.Finding.Severity, vo.Finding.Path, vo.Finding.Line, vo.Finding.Title))
			continue
		}
		items = append(items, cluster.Item{FindingID: fid, Finding: vo.Finding})
	}
	o.emit(runID, "validated", map[string]any{"produced": sum.Produced, "kept": sum.Kept})

	published, needsHuman, nClusters, adjCost, err := o.adjudicate(ctx, runID, p, items, opts, wts)
	if err != nil {
		return sum, err
	}
	totalCost += adjCost
	sum.Clusters = nClusters
	sum.Published = len(published)
	sum.NeedsHuman = len(needsHuman)

	status := "completed"
	if !allOK {
		status = "degraded"
	}

	// The verdict is computed, never stated.
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
		RunComplete:          allOK,
		GatesPassed:          true,
		IsFork:               p.IsFork,
		PublishedMaxSeverity: maxSev,
		PublishedCount:       len(published),
		NeedsHumanCount:      len(needsHuman),
	})
	sum.Event, sum.EventReason = event, reason

	maxInline := o.Cfg.Posting.MaxInlineComments
	if opts.MaxComments > 0 {
		maxInline = opts.MaxComments
	}
	var statusLines []forge.AdapterStatusLine
	for _, oc := range outcomes {
		statusLines = append(statusLines, forge.AdapterStatusLine{ID: oc.cfg.ID, Lens: oc.cfg.Lens, Status: string(oc.result.Status)})
	}
	reviewIn := forge.ReviewInput{
		Repo: p.Repo, PRNumber: p.PRNumber, HeadSHA: p.HeadSHA,
		Event: event, EventReason: reason,
		Published: published, NeedsHuman: needsHuman, BodyNotes: bodyNotes,
		Adapters:          statusLines,
		MaxInline:         maxInline,
		MinSeverity:       o.Cfg.Posting.MinSeverity,
		FooterAttribution: o.Cfg.Posting.FooterAttribution,
	}

	if opts.DryRun || o.Cfg.Posting.DryRun || o.Forge == nil {
		inline, overflow := forge.SelectComments(published, reviewIn.MinSeverity, maxInline)
		sum.RenderedReview = renderDryRun(reviewIn, inline, overflow)
	} else {
		prevSHA, prevEvent, prevID, _ := o.Store.LastPostedReview(p.Repo, p.PRNumber)
		reviewIn.PriorHeadSHA, reviewIn.PriorEvent, reviewIn.PriorReviewID = prevSHA, prevEvent, prevID
		out, err := forge.Post(ctx, o.Forge, reviewIn)
		if err != nil {
			o.Store.FinishRun(runID, "failed", event, "post failed: "+err.Error(), totalCost)
			return sum, fmt.Errorf("post review: %w", err)
		}
		sum.Posted = true
		sum.ReviewID = out.ReviewID
		if !out.UpdatedInPlace {
			o.Store.RecordPostedReview(runID, p.Repo, p.PRNumber, p.HeadSHA, event, out.ReviewID)
		}
		for _, it := range out.Inline {
			if it.VerdictID != 0 {
				o.Store.MarkVerdictPosted(it.VerdictID, out.ReviewID)
			}
		}
		o.emit(runID, "posted", map[string]any{"review_id": out.ReviewID, "event": event})
	}

	sum.Status = status
	sum.TotalCostUSD = totalCost
	o.Store.FinishRun(runID, status, sum.Event, sum.EventReason, totalCost)
	o.emit(runID, "run_finished", sum)
	return sum, nil
}

func (o *Orchestrator) runAdapter(ctx context.Context, runID int64, acfg config.Adapter, p *packet.Packet, wts *packet.Worktrees, local bool) adapterOutcome {
	oc := adapterOutcome{cfg: acfg}
	o.emit(runID, "adapter_started", map[string]any{"adapter": acfg.ID, "lens": acfg.Lens})

	rev, err := adapter.New(acfg, o.Prompts, o.Runner, o.Client)
	if err != nil {
		oc.result = adapter.Result{Status: adapter.StatusCrashed, Err: err.Error()}
	} else {
		worktree := ""
		if local {
			// Local mode reviews the working tree in place; adapters are
			// read-only by configuration.
			worktree = p.RepoDir
		} else if wts != nil {
			if wt, werr := wts.Add(ctx, acfg.ID, p.HeadSHA); werr == nil {
				worktree = wt
			} else {
				oc.result.Err = "worktree: " + werr.Error()
			}
		}
		if acfg.Kind != "modelapi" && worktree == "" && !local {
			oc.result = adapter.Result{Status: adapter.StatusCrashed,
				Err: "no local checkout available for an agentic adapter; run inside a clone or use modelapi"}
		} else {
			oc.result = rev.Review(ctx, p, worktree)
			if oc.result.Status.Retryable() {
				o.emit(runID, "adapter_retry", map[string]any{"adapter": acfg.ID, "status": string(oc.result.Status)})
				oc.result = rev.Review(ctx, p, worktree)
			}
			// A dirty worktree is an adapter permission bug: fail loudly.
			if wts != nil && !local {
				if cerr := wts.AssertClean(ctx, acfg.ID); cerr != nil {
					oc.result.Status = adapter.StatusCrashed
					oc.result.Err = cerr.Error()
					oc.result.Findings = nil
				}
			}
		}
	}

	arID, serr := o.Store.InsertAdapterRun(store.AdapterRun{
		RunID: runID, AdapterID: acfg.ID, Lens: acfg.Lens, Model: acfg.Model,
		Status:     string(oc.result.Status),
		DurationMS: oc.result.Duration.Milliseconds(),
		CostUSD:    oc.result.CostUSD,
		InputTokens: oc.result.Tokens.Input, OutputTokens: oc.result.Tokens.Output,
		RawOutput: oc.result.Raw, Error: oc.result.Err,
	})
	if serr == nil {
		oc.arID = arID
	}
	o.emit(runID, "adapter_finished", map[string]any{
		"adapter": acfg.ID, "status": string(oc.result.Status),
		"findings": len(oc.result.Findings), "cost_usd": oc.result.CostUSD,
		"duration_ms": oc.result.Duration.Milliseconds(),
	})
	return oc
}

// adjudicate clusters items, then runs verification, challenge, and judge
// per cluster, persisting everything.
func (o *Orchestrator) adjudicate(ctx context.Context, runID int64, p *packet.Packet, items []cluster.Item, opts Options, wts *packet.Worktrees) (published, needsHuman []forge.PublishedItem, nClusters int, cost float64, err error) {
	clusters := cluster.Group(items, o.Cfg.Adjudication.ClusterWindowLines)
	nClusters = len(clusters)
	o.emit(runID, "clustered", map[string]any{"clusters": nClusters})

	// One extra worktree for verification and code context.
	verifyDir := ""
	if p.RepoDir != "" {
		if wts != nil {
			if d, werr := wts.Add(ctx, "adjudicate", p.HeadSHA); werr == nil {
				verifyDir = d
			}
		} else {
			verifyDir = p.RepoDir // local mode: working tree is the truth
		}
	}
	var codeCtx adjudicate.CodeContext
	if verifyDir != "" {
		codeCtx = adjudicate.ContextFromWorktree(verifyDir)
	} else {
		codeCtx = adjudicate.ContextFromPacket(p)
	}

	for i := range clusters {
		c := &clusters[i]
		var fids []int64
		for _, it := range c.Items {
			fids = append(fids, it.FindingID)
		}
		cid, serr := o.Store.InsertCluster(runID, c.Path, c.Line, string(c.Category), len(c.Supporters), fids)
		if serr != nil {
			return nil, nil, nClusters, cost, serr
		}
		o.emit(runID, "cluster", map[string]any{"id": cid, "path": c.Path, "line": c.Line, "category": c.Category, "supporters": c.Supporters})

		code := codeCtx(c.Path, c.Line)

		var vres []verify.Result
		if verifyDir != "" {
			vres = verify.Run(ctx, verifyDir, o.Cfg.Verification.Commands, c)
			for _, v := range vres {
				o.Store.InsertVerification(store.StoredVerification{
					ClusterID: cid, Kind: v.Kind, Command: v.Command,
					ExitCode: v.ExitCode, Output: v.Output, Conclusion: v.Conclusion,
				})
			}
		}

		var ch *adjudicate.Challenge
		chCfg := o.Cfg.Adjudication.Challenger
		if chCfg.Enabled && !opts.NoChallenger &&
			c.MaxSeverity().Rank() >= chCfg.MinSeverity.Rank() {
			got, chCost, cherr := adjudicate.RunChallenge(ctx, o.Client, o.Prompts, chCfg.Model, c, code)
			cost += chCost
			if cherr == nil {
				ch = &got
				o.Store.InsertChallenge(store.StoredChallenge{
					ClusterID: cid, Model: got.Model, Argument: got.Argument, CouldArgue: got.CouldArgue,
				})
				o.emit(runID, "challenge", map[string]any{"cluster_id": cid, "could_argue": got.CouldArgue})
			}
		}

		jout, jCost, jerr := adjudicate.RunJudge(ctx, o.Client, o.Prompts, o.Cfg.Adjudication.Judge.Model, adjudicate.JudgeInput{
			Cluster: c, Code: code, Challenge: ch, Verifications: vres,
		})
		cost += jCost
		if jerr != nil {
			// A judge that cannot run is a dropped cluster with the reason
			// recorded, not a crashed run.
			jout = adjudicate.JudgeOutput{Verdict: adjudicate.VerdictDrop, Reason: "judge failed: " + jerr.Error()}
		}

		// The judge's anchor is re-validated: it has no authority to anchor
		// outside the commentable set.
		if jout.Verdict == adjudicate.VerdictPublish && jout.Final != nil &&
			!p.Commentable[jout.Final.Path][jout.Final.Line] {
			jout.Verdict = adjudicate.VerdictNeedsHuman
			jout.Reason += " (judge anchor outside commentable set; demoted to needs_human)"
		}

		sv := store.StoredVerdict{ClusterID: cid, Verdict: jout.Verdict, Reason: jout.Reason}
		if jout.Final != nil {
			sv.FinalSeverity = string(jout.Final.Severity)
			sv.FinalBody = jout.Final.Body
			sv.FinalPatch = jout.Final.SuggestedPatch
		}
		vid, serr2 := o.Store.InsertVerdict(sv)
		if serr2 != nil {
			return nil, nil, nClusters, cost, serr2
		}
		o.emit(runID, "verdict", map[string]any{"cluster_id": cid, "verdict": jout.Verdict, "reason": jout.Reason})

		if jout.Final == nil {
			continue
		}
		item := forge.PublishedItem{
			VerdictID:  vid,
			Path:       jout.Final.Path,
			Line:       jout.Final.Line,
			StartLine:  jout.Final.StartLine,
			Severity:   jout.Final.Severity,
			Title:      c.Items[0].Finding.Title,
			Body:       jout.Final.Body,
			Patch:      jout.Final.SuggestedPatch,
			Supporters: c.Supporters,
		}
		if ch != nil && ch.CouldArgue {
			item.Dissenters = []string{"challenger (" + ch.Model + ")"}
		}
		switch jout.Verdict {
		case adjudicate.VerdictPublish:
			published = append(published, item)
		case adjudicate.VerdictNeedsHuman:
			needsHuman = append(needsHuman, item)
		}
	}
	return published, needsHuman, nClusters, cost, nil
}

func renderDryRun(in forge.ReviewInput, inline, overflow []forge.PublishedItem) string {
	var b strings.Builder
	b.WriteString("──────────────────────────────────────────────\n")
	fmt.Fprintf(&b, "DRY RUN — review that would be posted (event: %s)\n", in.Event)
	b.WriteString("──────────────────────────────────────────────\n\n")
	b.WriteString(forge.RenderBody(in, inline, overflow))
	for _, it := range inline {
		fmt.Fprintf(&b, "\n--- inline comment @ %s:%d ---\n%s", it.Path, it.Line, forge.RenderComment(it, in.FooterAttribution))
	}
	return b.String()
}
