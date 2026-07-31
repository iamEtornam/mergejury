package adapter

import (
	"context"
	"errors"
	"time"

	"mergejury/internal/anthropic"
	"mergejury/internal/config"
	"mergejury/internal/finding"
	"mergejury/internal/packet"
	"mergejury/prompts"
)

// AnthropicClient aliases the shared client so callers construct one and
// hand it to every model-backed component.
type AnthropicClient = anthropic.Client

// ModelAPI sends the rendered diff straight to a model API with no repo
// access. Cheapest adapter and the precision baseline the agentic adapters
// have to beat.
type ModelAPI struct {
	cfg     config.Adapter
	prompts *prompts.Set
	client  *anthropic.Client
}

func (m *ModelAPI) ID() string    { return m.cfg.ID }
func (m *ModelAPI) Lens() string  { return m.cfg.Lens }
func (m *ModelAPI) Model() string { return m.cfg.Model }

func (m *ModelAPI) Probe(ctx context.Context) ProbeResult {
	if m.client == nil || m.client.APIKey == "" {
		return ProbeResult{AdapterID: m.cfg.ID, OK: false, Detail: "no API key",
			Remediation: "set ANTHROPIC_API_KEY in the environment"}
	}
	if _, err := m.prompts.Lens(m.cfg.Lens); err != nil {
		return ProbeResult{AdapterID: m.cfg.ID, OK: false, Detail: err.Error(),
			Remediation: "configure a lens with a matching prompts/lens_<name>.md"}
	}
	return ProbeResult{AdapterID: m.cfg.ID, OK: true, Detail: "API key present, lens prompt found"}
}

func (m *ModelAPI) Review(ctx context.Context, p *packet.Packet, _ string) Result {
	start := time.Now()
	res := Result{Status: StatusOK}
	system, err := m.prompts.Lens(m.cfg.Lens)
	if err != nil {
		res.Status = StatusCrashed
		res.Err = err.Error()
		return res
	}
	if m.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, m.cfg.Timeout)
		defer cancel()
	}
	resp, err := m.client.Complete(ctx, m.cfg.Model, system, userContent(p), 0)
	res.Duration = time.Since(start)
	if err != nil {
		var authErr anthropic.AuthError
		switch {
		case errors.As(err, &authErr):
			res.Status = StatusAuthError
		case ctx.Err() == context.DeadlineExceeded:
			res.Status = StatusTimeout
		default:
			res.Status = StatusCrashed
		}
		res.Err = err.Error()
		return res
	}
	res.Raw = resp.Text
	res.CostUSD = resp.Cost
	res.Tokens = TokenCounts{Input: resp.Usage.InputTokens, Output: resp.Usage.OutputTokens}
	fs, omissions, err := finding.ParseFindings(resp.Text)
	if err != nil {
		res.Status = StatusParseError
		res.Err = err.Error()
		return res
	}
	res.Findings = stamp(fs, m.cfg.ID, m.cfg.Lens)
	res.Omissions = omissions
	return res
}
