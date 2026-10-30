package run

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/iamEtornam/mergejury/internal/forge"
)

func regressionReview(t *testing.T, stub *modelStub) (*Orchestrator, *fakeForge) {
	t.Helper()
	dir, base, head, patch := makePRFixture(t)
	t.Chdir(dir)
	ff := &fakeForge{pr: &forge.PR{Repo: "o/r", Number: 1, BaseSHA: base, HeadSHA: head,
		Files: []forge.PRFile{{Path: "auth.go", Status: "modified", Patch: patch}}}}
	return e2eOrchestrator(t, stub, ff), ff
}

func TestJudgeFailureNeverApproves(t *testing.T) {
	for _, response := range []string{"not JSON", `{}`, `{"verdict":"unknown"}`, `{"verdict":"publish"}`, `{"verdict":"needs_human","final":{}}`} {
		t.Run(response, func(t *testing.T) {
			o, ff := regressionReview(t, &modelStub{reviewerText: reviewerFinding(7), judgeText: response})
			sum, err := o.ReviewPR(context.Background(), "o/r", 1, Options{NoChallenger: true})
			if err == nil {
				t.Fatalf("invalid judge output accepted: %+v", sum)
			}
			if len(ff.created) != 0 {
				t.Fatalf("posted review after judge failure: %+v", ff.created)
			}
			rn, loadErr := o.Store.GetRun(sum.RunID)
			if loadErr != nil || rn.Status != "failed" || rn.FinishedAt == nil {
				t.Fatalf("run not marked failed: %+v, %v", rn, loadErr)
			}
		})
	}
}

func TestUnanchoredBlockerNeedsHuman(t *testing.T) {
	o, ff := regressionReview(t, &modelStub{reviewerText: reviewerFinding(1)})
	sum, err := o.ReviewPR(context.Background(), "o/r", 1, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Event != forge.EventComment || sum.NeedsHuman != 1 {
		t.Fatalf("unresolved blocker ignored: %+v", sum)
	}
	if !strings.Contains(ff.created[0].Body, "string(expected) == tok leaks timing.") {
		t.Fatal("unanchored finding body missing")
	}
	replay, err := o.Replay(context.Background(), sum.RunID, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if replay.Event == forge.EventApprove || replay.NeedsHuman != 1 {
		t.Fatalf("replay ignored unresolved blocker: %+v", replay)
	}
}

func TestConfiguredDryRunNeverPostsGateReview(t *testing.T) {
	for _, configured := range []bool{false, true} {
		o, ff := regressionReview(t, &modelStub{})
		o.Cfg.Gates.MaxChangedLines = 1
		o.Cfg.Posting.DryRun = configured
		sum, err := o.ReviewPR(context.Background(), "o/r", 1, Options{DryRun: !configured})
		if err != nil {
			t.Fatal(err)
		}
		if sum.Status != "gated" || len(ff.created) != 0 {
			t.Fatalf("dry run posted: %+v, reviews=%d", sum, len(ff.created))
		}
	}
}

func TestReplayPreservesOriginalHistory(t *testing.T) {
	stub := &modelStub{reviewerText: reviewerFinding(7), judgeText: judgePublishBlocker}
	o, ff := regressionReview(t, stub)
	original, err := o.ReviewPR(context.Background(), "o/r", 1, Options{NoChallenger: true})
	if err != nil {
		t.Fatal(err)
	}
	verdicts, err := o.Store.VerdictsForRun(original.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Store.InsertOutcome(verdicts[0].ID, "observed", false, true, 2); err != nil {
		t.Fatal(err)
	}
	before, err := o.Store.Stats()
	if err != nil {
		t.Fatal(err)
	}
	stub.judgeText = `{"verdict":"drop","reason":"No longer reproduced"}`
	replay, err := o.Replay(context.Background(), original.RunID, Options{NoChallenger: true})
	if err != nil {
		t.Fatal(err)
	}
	if replay.RunID == original.RunID {
		t.Fatal("replay overwrote original run")
	}
	afterVerdicts, err := o.Store.VerdictsForRun(original.RunID)
	if err != nil || !reflect.DeepEqual(verdicts, afterVerdicts) {
		t.Fatalf("original verdicts changed: %+v, %v", afterVerdicts, err)
	}
	after, err := o.Store.Stats()
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("replay changed scoreboard: before=%+v after=%+v err=%v", before, after, err)
	}
	replayVerdicts, err := o.Store.VerdictsForRun(replay.RunID)
	if err != nil || len(replayVerdicts) != 1 || replayVerdicts[0].Verdict != "drop" || replayVerdicts[0].PostedCommentID != nil {
		t.Fatalf("replay verdict missing or marked posted: %+v %v", replayVerdicts, err)
	}
	replayFindings, err := o.Store.FindingsForRun(replay.RunID, true)
	if err != nil || len(replayFindings) != 1 {
		t.Fatalf("replay finding trace missing: %+v %v", replayFindings, err)
	}
	rn, err := o.Store.GetRun(replay.RunID)
	if err != nil || rn.FinishedAt == nil || rn.Status != "completed" || rn.CommentsPosted != 0 {
		t.Fatalf("replay not completed: %+v %v", rn, err)
	}
	if len(ff.created) != 1 {
		t.Fatalf("replay posted a review: %d", len(ff.created))
	}
	// Failed replays must preserve the published source too.
	stub.judgeText = "invalid JSON"
	failed, err := o.Replay(context.Background(), original.RunID, Options{NoChallenger: true})
	if err == nil {
		t.Fatal("invalid replay succeeded")
	}
	if failed.RunID == original.RunID {
		t.Fatal("failed replay reused original")
	}
	failedRun, loadErr := o.Store.GetRun(failed.RunID)
	if loadErr != nil || failedRun.Status != "failed" || failedRun.FinishedAt == nil {
		t.Fatalf("failed replay not persisted: %+v %v", failedRun, loadErr)
	}
	afterVerdicts, err = o.Store.VerdictsForRun(original.RunID)
	if err != nil || !reflect.DeepEqual(verdicts, afterVerdicts) {
		t.Fatal("failed replay changed original verdicts")
	}
}

func TestJudgeAPIErrorNeverApproves(t *testing.T) {
	o, ff := regressionReview(t, &modelStub{reviewerText: reviewerFinding(7), judgeStatus: 503})
	sum, err := o.ReviewPR(context.Background(), "o/r", 1, Options{NoChallenger: true})
	if err == nil || len(ff.created) != 0 {
		t.Fatalf("judge failure posted a review: sum=%+v err=%v reviews=%+v", sum, err, ff.created)
	}
	rn, err := o.Store.GetRun(sum.RunID)
	if err != nil || rn.Status != "failed" {
		t.Fatalf("failed adjudication not persisted: %+v %v", rn, err)
	}
}

func TestValidJudgeDropCanApprove(t *testing.T) {
	o, _ := regressionReview(t, &modelStub{reviewerText: reviewerFinding(7), judgeText: `{"verdict":"drop","reason":"Evidence refuted"}`})
	sum, err := o.ReviewPR(context.Background(), "o/r", 1, Options{NoChallenger: true})
	if err != nil || sum.Event != forge.EventApprove {
		t.Fatalf("legitimate drop rejected: %+v %v", sum, err)
	}
}
