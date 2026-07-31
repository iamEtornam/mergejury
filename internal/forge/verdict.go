package forge

import (
	"fmt"

	"mergejury/internal/finding"
)

// VerdictInput is everything the 9.1 rule needs. The event is a
// deterministic function of this struct: no model ever emits a verdict, and
// no model text is parsed for one.
type VerdictInput struct {
	Enabled          bool // verdict.enabled AND not --no-verdict
	RequestChangesAt finding.Severity
	ApproveOnClean   bool
	ApproveForks     bool

	RunComplete bool // every configured adapter finished with status ok
	GatesPassed bool
	IsFork      bool

	PublishedMaxSeverity finding.Severity // zero value when nothing published
	PublishedCount       int
	NeedsHumanCount      int
}

const (
	EventApprove        = "APPROVE"
	EventRequestChanges = "REQUEST_CHANGES"
	EventComment        = "COMMENT"
)

// ComputeEvent evaluates the rule in order, first match wins. The asymmetry
// is the safety property: positive findings are evidence regardless of what
// else failed; the absence of findings is only evidence when the review was
// complete. A degraded run can request changes but can never approve, and
// that is not configurable.
func ComputeEvent(in VerdictInput) (event, reason string) {
	if !in.Enabled {
		return EventComment, "verdict disabled"
	}
	if in.PublishedCount > 0 && in.PublishedMaxSeverity.Rank() >= in.RequestChangesAt.Rank() {
		return EventRequestChanges, fmt.Sprintf("published a %s finding (threshold %s); positive findings count even on a degraded run", in.PublishedMaxSeverity, in.RequestChangesAt)
	}
	if in.RunComplete && in.GatesPassed && (!in.IsFork || in.ApproveForks) &&
		in.PublishedCount == 0 && in.NeedsHumanCount == 0 && in.ApproveOnClean {
		return EventApprove, "complete run, all gates passed, zero published findings, zero needs-human items"
	}
	switch {
	case !in.RunComplete:
		return EventComment, "run degraded: absence of findings is only evidence when the review was complete"
	case in.IsFork && !in.ApproveForks:
		return EventComment, "fork PR: approval disabled for forks"
	case in.NeedsHumanCount > 0:
		return EventComment, "needs-human items present: approving would approve over a suspected defect"
	case in.PublishedCount > 0:
		return EventComment, "published findings below the request-changes threshold"
	case !in.ApproveOnClean:
		return EventComment, "approve_on_clean disabled"
	default:
		return EventComment, "gates not passed"
	}
}
