// Package adapter defines the Reviewer interface and its implementations:
// three agentic CLI adapters (claude-code, cursor, antigravity) and one
// direct model API adapter (modelapi).
package adapter

import (
	"context"
	"fmt"
	"time"

	"mergejury/internal/config"
	"mergejury/internal/finding"
	"mergejury/internal/packet"
	"mergejury/prompts"
)

type Status string

const (
	StatusOK         Status = "ok"
	StatusTimeout    Status = "timeout"
	StatusParseError Status = "parse_error"
	StatusAuthError  Status = "auth_error"
	StatusDenied     Status = "denied"
	StatusCrashed    Status = "crashed"
)

// Retryable statuses: retry once on parse_error and crashed, never on
// auth_error or denied.
func (s Status) Retryable() bool { return s == StatusParseError || s == StatusCrashed }

type TokenCounts struct {
	Input  int64 `json:"input"`
	Output int64 `json:"output"`
}

type ProbeResult struct {
	AdapterID   string `json:"adapter_id"`
	OK          bool   `json:"ok"`
	Detail      string `json:"detail"`
	Remediation string `json:"remediation"` // specific fix when not OK
}

type Result struct {
	Findings  []finding.Finding
	Omissions []string
	Status    Status
	Raw       string // always persisted, even on success
	CostUSD   float64
	Tokens    TokenCounts
	Duration  time.Duration
	Err       string
}

// Reviewer is one configured review agent. A reviewer failing degrades the
// run; it never aborts it, so failures are carried in Result.Status rather
// than as returned errors.
type Reviewer interface {
	ID() string
	Lens() string
	Model() string
	Probe(ctx context.Context) ProbeResult
	// Review runs the lens over the packet. worktree is a read-only checkout
	// at the head SHA for this adapter, or "" when no local repo exists.
	Review(ctx context.Context, p *packet.Packet, worktree string) Result
}

// New builds a Reviewer from config. Unknown kinds error at startup, not
// mid-run.
func New(c config.Adapter, ps *prompts.Set, runner Runner, apiClient *AnthropicClient) (Reviewer, error) {
	switch c.Kind {
	case "modelapi":
		return &ModelAPI{cfg: c, prompts: ps, client: apiClient}, nil
	case "claude-code":
		return newClaudeCode(c, ps, runner), nil
	case "cursor":
		return newCursor(c, ps, runner), nil
	case "antigravity":
		return newAntigravity(c, ps, runner), nil
	}
	return nil, fmt.Errorf("unknown adapter kind %q", c.Kind)
}

// userContent assembles the reviewer-facing user message: PR metadata and the
// rendered diff, both wrapped as untrusted data.
func userContent(p *packet.Packet) string {
	meta := fmt.Sprintf("PR title: %s\n\nPR body:\n%s", p.Title, p.Body)
	return "Review this pull request through your lens.\n\n" +
		prompts.UntrustedBlock("pr-metadata", meta) + "\n\n" +
		prompts.UntrustedBlock("diff", p.Rendered) +
		"\n\nRemember: respond with the single JSON object from your output contract, nothing else."
}

// stamp sets reviewer identity on findings the model isn't trusted to fill.
func stamp(fs []finding.Finding, id, lens string) []finding.Finding {
	for i := range fs {
		fs[i].ReviewerID = id
		fs[i].Lens = lens
	}
	return fs
}
