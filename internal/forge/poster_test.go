package forge

import (
	"context"
	"strings"
	"testing"

	"revu/internal/finding"
)

// fakeForge records calls and scripts failures.
type fakeForge struct {
	createCalls   []ReviewRequest
	createErrs    []error // popped per CreateReview call
	nextReviewID  int64
	updateCalls   []int64
	dismissCalls  []int64
	dismissMsgs   []string
	reviews       []Review
	viewerLogin   string
}

func (f *fakeForge) FetchPR(context.Context, string, int) (*PR, error) { return nil, nil }
func (f *fakeForge) Viewer(context.Context) (string, error)           { return f.viewerLogin, nil }
func (f *fakeForge) CreateReview(_ context.Context, _ string, _ int, req ReviewRequest) (int64, error) {
	f.createCalls = append(f.createCalls, req)
	if len(f.createErrs) > 0 {
		err := f.createErrs[0]
		f.createErrs = f.createErrs[1:]
		if err != nil {
			return 0, err
		}
	}
	f.nextReviewID++
	return f.nextReviewID, nil
}
func (f *fakeForge) UpdateReviewBody(_ context.Context, _ string, _ int, id int64, _ string) error {
	f.updateCalls = append(f.updateCalls, id)
	return nil
}
func (f *fakeForge) ListReviews(context.Context, string, int) ([]Review, error) {
	return f.reviews, nil
}
func (f *fakeForge) DismissReview(_ context.Context, _ string, _ int, id int64, msg string) error {
	f.dismissCalls = append(f.dismissCalls, id)
	f.dismissMsgs = append(f.dismissMsgs, msg)
	return nil
}
func (f *fakeForge) ListReviewComments(context.Context, string, int) ([]ReviewComment, error) {
	return nil, nil
}

func item(path string, line int, sev finding.Severity) PublishedItem {
	return PublishedItem{Path: path, Line: line, Severity: sev, Title: "t", Body: "b", Supporters: []string{"a1"}}
}

// ---- the verdict table from 9.1, exhaustively ----

func TestVerdictTable(t *testing.T) {
	base := VerdictInput{
		Enabled:          true,
		RequestChangesAt: finding.SevBlocker,
		ApproveOnClean:   true,
		ApproveForks:     false,
		RunComplete:      true,
		GatesPassed:      true,
	}
	tests := []struct {
		name string
		mod  func(*VerdictInput)
		want string
	}{
		{"clean complete run approves", func(v *VerdictInput) {}, EventApprove},
		{"published blocker requests changes", func(v *VerdictInput) {
			v.PublishedCount = 1
			v.PublishedMaxSeverity = finding.SevBlocker
		}, EventRequestChanges},
		{"degraded run with zero findings comments, never approves", func(v *VerdictInput) {
			v.RunComplete = false
		}, EventComment},
		{"degraded run with a blocker still requests changes", func(v *VerdictInput) {
			v.RunComplete = false
			v.PublishedCount = 1
			v.PublishedMaxSeverity = finding.SevBlocker
		}, EventRequestChanges},
		{"fork PR with a clean run comments", func(v *VerdictInput) {
			v.IsFork = true
		}, EventComment},
		{"fork PR approves only when approve_forks is on", func(v *VerdictInput) {
			v.IsFork = true
			v.ApproveForks = true
		}, EventApprove},
		{"needs_human blocks approval", func(v *VerdictInput) {
			v.NeedsHumanCount = 1
		}, EventComment},
		{"no-verdict forces comment even on a clean run", func(v *VerdictInput) {
			v.Enabled = false
		}, EventComment},
		{"no-verdict forces comment even with a blocker", func(v *VerdictInput) {
			v.Enabled = false
			v.PublishedCount = 1
			v.PublishedMaxSeverity = finding.SevBlocker
		}, EventComment},
		{"findings below threshold comment, not request changes", func(v *VerdictInput) {
			v.PublishedCount = 2
			v.PublishedMaxSeverity = finding.SevMajor // threshold is blocker
		}, EventComment},
		{"lower threshold: major requests changes", func(v *VerdictInput) {
			v.RequestChangesAt = finding.SevMajor
			v.PublishedCount = 1
			v.PublishedMaxSeverity = finding.SevMajor
		}, EventRequestChanges},
		{"approve_on_clean off: clean run comments", func(v *VerdictInput) {
			v.ApproveOnClean = false
		}, EventComment},
		{"gates failed: no approval", func(v *VerdictInput) {
			v.GatesPassed = false
		}, EventComment},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.mod(&in)
			got, reason := ComputeEvent(in)
			if got != tc.want {
				t.Errorf("event = %s (%s), want %s", got, reason, tc.want)
			}
			if reason == "" {
				t.Error("reason must never be empty")
			}
		})
	}
}

// ---- poster mechanics ----

func TestCapAndSeverityThreshold(t *testing.T) {
	var items []PublishedItem
	items = append(items, item("z.go", 1, finding.SevNit))      // dropped by threshold
	items = append(items, item("a.go", 5, finding.SevBlocker))  // first
	for i := 0; i < 12; i++ {
		items = append(items, item("m.go", 10+i, finding.SevMinor))
	}
	inline, overflow := SelectComments(items, finding.SevMinor, 10)
	if len(inline) != 10 {
		t.Fatalf("inline = %d, want 10", len(inline))
	}
	if len(overflow) != 3 { // 13 kept (nit dropped) - 10 inline
		t.Fatalf("overflow = %d, want 3", len(overflow))
	}
	if inline[0].Severity != finding.SevBlocker || inline[0].Path != "a.go" {
		t.Errorf("sort order wrong: first inline is %+v", inline[0])
	}
	for _, it := range append(inline, overflow...) {
		if it.Severity == finding.SevNit {
			t.Error("nit survived the threshold")
		}
	}
}

func TestPostHappyPath(t *testing.T) {
	ff := &fakeForge{}
	in := ReviewInput{
		Repo: "o/r", PRNumber: 1, HeadSHA: "abc", Event: EventRequestChanges, EventReason: "blocker found",
		Published:         []PublishedItem{item("a.go", 5, finding.SevBlocker)},
		Adapters:          []AdapterStatusLine{{ID: "a1", Lens: "security", Status: "ok"}, {ID: "a2", Lens: "perf", Status: "auth_error"}},
		MaxInline:         10,
		MinSeverity:       finding.SevMinor,
		FooterAttribution: true,
	}
	out, err := Post(context.Background(), ff, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(ff.createCalls) != 1 {
		t.Fatalf("createCalls = %d", len(ff.createCalls))
	}
	req := ff.createCalls[0]
	if req.Event != EventRequestChanges || req.CommitID != "abc" || len(req.Comments) != 1 {
		t.Errorf("bad request: %+v", req)
	}
	if !strings.Contains(req.Body, "1 of 2 reviewers completed") || !strings.Contains(req.Body, "a2: auth_error") {
		t.Errorf("degradation not named in body:\n%s", req.Body)
	}
	if !strings.Contains(req.Comments[0].Body, "flagged by a1") {
		t.Errorf("attribution footer missing:\n%s", req.Comments[0].Body)
	}
	if out.ReviewID == 0 {
		t.Error("review id not returned")
	}
}

func Test422MovesCommentsToBodyAndRetriesOnce(t *testing.T) {
	ff := &fakeForge{createErrs: []error{StatusError{StatusCode: 422, Msg: "Unprocessable"}}}
	in := ReviewInput{
		Repo: "o/r", PRNumber: 1, HeadSHA: "abc", Event: EventComment, EventReason: "r",
		Published:   []PublishedItem{item("a.go", 5, finding.SevMajor)},
		MaxInline:   10,
		MinSeverity: finding.SevMinor,
	}
	out, err := Post(context.Background(), ff, in)
	if err != nil {
		t.Fatalf("422 must not fail the review: %v", err)
	}
	if len(ff.createCalls) != 2 {
		t.Fatalf("want exactly one retry, got %d calls", len(ff.createCalls))
	}
	if len(ff.createCalls[1].Comments) != 0 {
		t.Error("retry must carry no inline comments")
	}
	if !strings.Contains(ff.createCalls[1].Body, "a.go:5") {
		t.Error("rejected comment not moved into the body")
	}
	if len(out.Inline) != 0 || len(out.Overflow) != 1 {
		t.Errorf("outcome not updated: inline=%d overflow=%d", len(out.Inline), len(out.Overflow))
	}
}

func TestNon422NotRetried(t *testing.T) {
	ff := &fakeForge{createErrs: []error{StatusError{StatusCode: 500, Msg: "boom"}}}
	in := ReviewInput{
		Repo: "o/r", PRNumber: 1, HeadSHA: "abc", Event: EventComment,
		Published: []PublishedItem{item("a.go", 5, finding.SevMajor)},
	}
	if _, err := Post(context.Background(), ff, in); err == nil {
		t.Fatal("500 must propagate")
	}
	if len(ff.createCalls) != 1 {
		t.Fatalf("500 must not be retried, got %d calls", len(ff.createCalls))
	}
}

func TestIdempotencyOnRepeatedHeadSHA(t *testing.T) {
	ff := &fakeForge{}
	in := ReviewInput{
		Repo: "o/r", PRNumber: 1, HeadSHA: "abc", Event: EventComment, EventReason: "r",
		Published:     []PublishedItem{item("a.go", 5, finding.SevMajor)},
		PriorHeadSHA:  "abc",
		PriorEvent:    EventComment,
		PriorReviewID: 77,
	}
	out, err := Post(context.Background(), ff, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(ff.createCalls) != 0 {
		t.Error("same SHA + same event must not create a second review")
	}
	if len(ff.updateCalls) != 1 || ff.updateCalls[0] != 77 {
		t.Errorf("existing review not updated: %v", ff.updateCalls)
	}
	if !out.UpdatedInPlace || out.ReviewID != 77 {
		t.Errorf("outcome: %+v", out)
	}
}

func TestChangedVerdictOnSameSHACreatesNewReview(t *testing.T) {
	ff := &fakeForge{}
	in := ReviewInput{
		Repo: "o/r", PRNumber: 1, HeadSHA: "abc", Event: EventRequestChanges, EventReason: "r",
		Published:     []PublishedItem{item("a.go", 5, finding.SevBlocker)},
		PriorHeadSHA:  "abc",
		PriorEvent:    EventComment,
		PriorReviewID: 77,
	}
	if _, err := Post(context.Background(), ff, in); err != nil {
		t.Fatal(err)
	}
	if len(ff.createCalls) != 1 || len(ff.updateCalls) != 0 {
		t.Error("changed verdict on same SHA must submit a new review")
	}
}

func TestSupersessionDismissesStaleRequestChanges(t *testing.T) {
	// Prior run requested changes; new run on a new SHA comes back clean but
	// degraded (COMMENT). The stale objection must be dismissed explicitly.
	ff := &fakeForge{}
	in := ReviewInput{
		Repo: "o/r", PRNumber: 1, HeadSHA: "def", Event: EventComment, EventReason: "degraded",
		PriorHeadSHA:  "abc",
		PriorEvent:    EventRequestChanges,
		PriorReviewID: 42,
	}
	out, err := Post(context.Background(), ff, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(ff.dismissCalls) != 1 || ff.dismissCalls[0] != 42 {
		t.Fatalf("stale REQUEST_CHANGES not dismissed: %v", ff.dismissCalls)
	}
	if !strings.Contains(ff.dismissMsgs[0], "def") {
		t.Errorf("dismissal message must name the new head SHA: %q", ff.dismissMsgs[0])
	}
	if !out.DismissedPrior {
		t.Error("outcome must record the dismissal")
	}
}

func TestApproveSupersedesWithoutDismissal(t *testing.T) {
	ff := &fakeForge{}
	in := ReviewInput{
		Repo: "o/r", PRNumber: 1, HeadSHA: "def", Event: EventApprove, EventReason: "clean",
		Adapters:      []AdapterStatusLine{{ID: "a1", Lens: "security", Status: "ok"}},
		PriorHeadSHA:  "abc",
		PriorEvent:    EventRequestChanges,
		PriorReviewID: 42,
	}
	if _, err := Post(context.Background(), ff, in); err != nil {
		t.Fatal(err)
	}
	if len(ff.dismissCalls) != 0 {
		t.Error("an APPROVE supersedes a prior REQUEST_CHANGES by itself; no dismissal call")
	}
}

func TestApproveBodyStatesWhatWasChecked(t *testing.T) {
	in := ReviewInput{
		Event: EventApprove, EventReason: "clean",
		Adapters: []AdapterStatusLine{{ID: "a1", Lens: "security", Status: "ok"}, {ID: "a2", Lens: "correctness", Status: "ok"}},
	}
	body := RenderBody(in, nil, nil)
	if !strings.Contains(body, "a1 (security)") || !strings.Contains(body, "a2 (correctness)") {
		t.Errorf("approve body must state what was checked and by which lenses:\n%s", body)
	}
}

func TestNeedsHumanInBody(t *testing.T) {
	in := ReviewInput{
		Event: EventComment, EventReason: "r",
		NeedsHuman: []PublishedItem{{Path: "x.go", Line: 3, Severity: finding.SevMajor, Title: "possible race", Body: "detail"}},
	}
	body := RenderBody(in, nil, nil)
	if !strings.Contains(body, "Needs human review") || !strings.Contains(body, "possible race") {
		t.Errorf("needs_human items missing from body:\n%s", body)
	}
}
