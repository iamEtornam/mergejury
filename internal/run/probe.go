package run

import (
	"context"

	"revu/internal/adapter"
)

// ProbeAdapters checks every configured adapter: installed, authenticated,
// expected flags present.
func (o *Orchestrator) ProbeAdapters(ctx context.Context) []adapter.ProbeResult {
	out := make([]adapter.ProbeResult, 0, len(o.Cfg.Adapters))
	for _, ac := range o.Cfg.Adapters {
		rev, err := adapter.New(ac, o.Prompts, o.Runner, o.Client)
		if err != nil {
			out = append(out, adapter.ProbeResult{AdapterID: ac.ID, OK: false, Detail: err.Error()})
			continue
		}
		out = append(out, rev.Probe(ctx))
	}
	return out
}
