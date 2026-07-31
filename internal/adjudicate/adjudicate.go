// Package adjudicate runs the challenger pass and the judge over clusters.
// Both are model calls with strictly parsed output; neither has verdict
// authority over the review event (that is computed in forge from the
// judge's structured per-cluster output).
package adjudicate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mergejury/internal/anthropic"
	"mergejury/internal/cluster"
	"mergejury/internal/finding"
	"mergejury/internal/packet"
	"mergejury/internal/verify"
	"mergejury/prompts"
)

// CodeContext renders the code around an anchor with real line numbers.
type CodeContext func(path string, line int) string

const contextRadius = 25

// ContextFromWorktree reads the file at head SHA from a checkout.
func ContextFromWorktree(dir string) CodeContext {
	return func(path string, line int) string {
		b, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil {
			return fmt.Sprintf("(could not read %s: %v)", path, err)
		}
		lines := strings.Split(string(b), "\n")
		start := line - contextRadius
		if start < 1 {
			start = 1
		}
		end := line + contextRadius
		if end > len(lines) {
			end = len(lines)
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "=== %s (head version, lines %d-%d) ===\n", path, start, end)
		for i := start; i <= end; i++ {
			marker := "  "
			if i == line {
				marker = "->"
			}
			fmt.Fprintf(&sb, "%s %6d | %s\n", marker, i, lines[i-1])
		}
		return sb.String()
	}
}

// ContextFromPacket falls back to the diff's own hunks when no checkout
// exists (modelapi-only runs).
func ContextFromPacket(p *packet.Packet) CodeContext {
	return func(path string, line int) string {
		for _, f := range p.Files {
			if f.Path != path {
				continue
			}
			var sb strings.Builder
			fmt.Fprintf(&sb, "=== %s (from diff hunks; only changed regions available) ===\n", path)
			for _, h := range f.Hunks {
				for _, l := range h.Lines {
					if l.Kind == packet.LineDeleted {
						continue
					}
					marker := "  "
					if l.NewNum == line {
						marker = "->"
					}
					fmt.Fprintf(&sb, "%s %6d | %s\n", marker, l.NewNum, l.Content)
				}
			}
			return sb.String()
		}
		return "(file not in diff)"
	}
}

func clusterJSON(c *cluster.Cluster) string {
	type item struct {
		Reviewer string          `json:"reviewer_id"`
		Lens     string          `json:"lens"`
		F        finding.Finding `json:"finding"`
	}
	items := make([]item, 0, len(c.Items))
	for _, it := range c.Items {
		items = append(items, item{Reviewer: it.Finding.ReviewerID, Lens: it.Finding.Lens, F: it.Finding})
	}
	b, _ := json.MarshalIndent(map[string]any{
		"path": c.Path, "line": c.Line, "category": c.Category,
		"agreement_count": len(c.Supporters), "supporters": c.Supporters,
		"findings": items,
	}, "", "  ")
	return string(b)
}

// ---- challenger ----

type Challenge struct {
	Model      string
	Argument   string
	CouldArgue bool
}

// RunChallenge asks a model that did not produce the finding to argue it is
// a false positive. The output is an argument, not a verdict.
func RunChallenge(ctx context.Context, client *anthropic.Client, ps *prompts.Set, model string, c *cluster.Cluster, code string) (Challenge, float64, error) {
	system, err := ps.Get("challenger")
	if err != nil {
		return Challenge{}, 0, err
	}
	user := "The finding cluster:\n\n" + prompts.UntrustedBlock("findings", clusterJSON(c)) +
		"\n\nThe code:\n\n" + prompts.UntrustedBlock("code", code)
	resp, err := client.Complete(ctx, model, system, user, 4096)
	if err != nil {
		return Challenge{}, 0, err
	}
	var parsed struct {
		CouldArgue bool   `json:"could_argue"`
		Argument   string `json:"argument"`
	}
	if err := json.Unmarshal([]byte(finding.ExtractJSON(resp.Text)), &parsed); err != nil {
		return Challenge{Model: model}, resp.Cost, fmt.Errorf("challenger output unparseable: %w", err)
	}
	return Challenge{Model: model, Argument: parsed.Argument, CouldArgue: parsed.CouldArgue}, resp.Cost, nil
}

// ---- judge ----

const (
	VerdictPublish    = "publish"
	VerdictDrop       = "drop"
	VerdictNeedsHuman = "needs_human"
)

type JudgeFinal struct {
	Path           string           `json:"path"`
	Line           int              `json:"line"`
	StartLine      *int             `json:"start_line"`
	Severity       finding.Severity `json:"severity"`
	Body           string           `json:"body"`
	SuggestedPatch *string          `json:"suggested_patch"`
}

type JudgeOutput struct {
	Verdict string      `json:"verdict"`
	Reason  string      `json:"reason"`
	Final   *JudgeFinal `json:"final"`
}

type JudgeInput struct {
	Cluster       *cluster.Cluster
	Code          string
	Challenge     *Challenge
	Verifications []verify.Result
}

// RunJudge adjudicates one cluster. The default on any ambiguity is drop:
// unparseable or invalid judge output becomes a drop with the parse problem
// as the reason, never a publish.
func RunJudge(ctx context.Context, client *anthropic.Client, ps *prompts.Set, model string, in JudgeInput) (JudgeOutput, float64, error) {
	system, err := ps.Get("judge")
	if err != nil {
		return JudgeOutput{}, 0, err
	}
	var sb strings.Builder
	sb.WriteString("The finding cluster:\n\n" + prompts.UntrustedBlock("findings", clusterJSON(in.Cluster)))
	sb.WriteString("\n\nThe code around the anchor:\n\n" + prompts.UntrustedBlock("code", in.Code))
	if in.Challenge != nil {
		ch, _ := json.MarshalIndent(map[string]any{
			"could_argue": in.Challenge.CouldArgue, "argument": in.Challenge.Argument,
		}, "", "  ")
		sb.WriteString("\n\nThe challenger's argument (adversarial pass, model " + in.Challenge.Model + "):\n\n" + prompts.UntrustedBlock("challenge", string(ch)))
	} else {
		sb.WriteString("\n\nNo challenger pass was run for this cluster (below the severity gate).")
	}
	if len(in.Verifications) > 0 {
		v, _ := json.MarshalIndent(in.Verifications, "", "  ")
		sb.WriteString("\n\nMechanical verification results (ground truth; outranks opinion):\n\n" + string(v))
	}
	fmt.Fprintf(&sb, "\n\nAgreement count: %d independent reviewer(s) flagged this region. One signal among several, not a tally to defer to.", len(in.Cluster.Supporters))

	resp, err := client.Complete(ctx, model, system, sb.String(), 4096)
	if err != nil {
		return JudgeOutput{}, 0, err
	}
	var out JudgeOutput
	if err := json.Unmarshal([]byte(finding.ExtractJSON(resp.Text)), &out); err != nil {
		return JudgeOutput{Verdict: VerdictDrop, Reason: "judge output unparseable: " + err.Error()}, resp.Cost, nil
	}
	switch out.Verdict {
	case VerdictPublish, VerdictNeedsHuman:
		if out.Final == nil || out.Final.Body == "" {
			return JudgeOutput{Verdict: VerdictDrop, Reason: "judge published without a final comment body"}, resp.Cost, nil
		}
		if out.Final.Severity.Rank() == 0 {
			out.Final.Severity = in.Cluster.MaxSeverity()
		}
		if out.Final.Path == "" {
			out.Final.Path = in.Cluster.Path
		}
		if out.Final.Line == 0 {
			out.Final.Line = in.Cluster.Line
		}
	case VerdictDrop:
	default:
		return JudgeOutput{Verdict: VerdictDrop, Reason: fmt.Sprintf("judge emitted unknown verdict %q", out.Verdict)}, resp.Cost, nil
	}
	return out, resp.Cost, nil
}
