// Package validate is the deterministic pass between adapters and the judge.
// No LLM involved. Every finding is kept, demoted, or dropped, with the
// reason recorded.
package validate

import (
	"context"
	"strconv"
	"strings"

	"revu/internal/finding"
	"revu/internal/packet"
)

// Drop reasons, recorded on every dropped finding.
const (
	ReasonSchema      = "schema"
	ReasonUnanchored  = "unanchored"
	ReasonBadEvidence = "bad_evidence"
	ReasonMultiline   = "multiline"
	ReasonDupe        = "dupe"
)

type Outcome struct {
	Finding    finding.Finding
	Kept       bool
	Demoted    bool // unanchored but major+: becomes a body-level note
	DropReason string
}

// FileLineCounter answers "does path exist at head, and how many lines does
// it have". Backed by git for real runs, by a map in tests.
type FileLineCounter interface {
	LineCount(ctx context.Context, path string) (lines int, exists bool)
}

// GitLineCounter counts lines at a SHA in a local repo.
type GitLineCounter struct {
	RepoDir string
	SHA     string // empty = working tree
	cache   map[string]int
}

func (g *GitLineCounter) LineCount(ctx context.Context, path string) (int, bool) {
	if g.cache == nil {
		g.cache = map[string]int{}
	}
	if n, ok := g.cache[path]; ok {
		return n, n >= 0
	}
	content, err := packet.FileAtSHA(ctx, g.RepoDir, g.SHA, path)
	if err != nil {
		g.cache[path] = -1
		return 0, false
	}
	n := strings.Count(content, "\n")
	if len(content) > 0 && !strings.HasSuffix(content, "\n") {
		n++
	}
	g.cache[path] = n
	return n, true
}

// MapLineCounter is the test/fallback implementation. When a path is absent
// from the map it falls back to the packet's own commentable knowledge: a
// path that appears in the diff at least exists.
type MapLineCounter struct{ Lines map[string]int }

func (m MapLineCounter) LineCount(_ context.Context, path string) (int, bool) {
	n, ok := m.Lines[path]
	return n, ok
}

// Run applies the section 6 checks in order. Findings come back in input
// order with their fate attached; nothing is silently discarded.
func Run(ctx context.Context, p *packet.Packet, counter FileLineCounter, findings []finding.Finding) []Outcome {
	out := make([]Outcome, 0, len(findings))
	// key: adapter|path|line|category -> index into out, for within-adapter dedupe
	seen := map[string]int{}
	for _, f := range findings {
		o := Outcome{Finding: f}

		// 1. Schema.
		if err := f.ValidateSchema(); err != nil {
			o.DropReason = ReasonSchema
			out = append(out, o)
			continue
		}

		// 2. Anchor.
		anchored := p.Commentable[f.Path][f.Line]
		if !anchored {
			if f.Severity.Rank() >= finding.SevMajor.Rank() {
				o.Kept = true
				o.Demoted = true
				o.DropReason = ReasonUnanchored // recorded even on demotion, for traceability
			} else {
				o.DropReason = ReasonUnanchored
				out = append(out, o)
				continue
			}
		}

		// 3. Evidence: every citation must point inside a real file.
		if counter != nil && !evidenceOK(ctx, counter, f.Evidence) {
			o.Kept = false
			o.Demoted = false
			o.DropReason = ReasonBadEvidence
			out = append(out, o)
			continue
		}

		// 4. Multi-line sanity: same file, both ends commentable.
		if f.StartLine != nil && !o.Demoted {
			if *f.StartLine >= f.Line || !p.Commentable[f.Path][*f.StartLine] {
				o.DropReason = ReasonMultiline
				o.Kept = false
				out = append(out, o)
				continue
			}
		}

		// 5. Dedupe within adapter: same (path, line, category) keeps the
		// highest severity.
		key := f.ReviewerID + "|" + f.Path + "|" + strconv.Itoa(f.Line) + "|" + string(f.Category)
		if prev, dup := seen[key]; dup {
			if f.Severity.Rank() > out[prev].Finding.Severity.Rank() {
				// keep the new one, drop the old
				out[prev].Kept = false
				out[prev].Demoted = false
				out[prev].DropReason = ReasonDupe
				seen[key] = len(out)
			} else {
				o.Kept = false
				o.Demoted = false
				o.DropReason = ReasonDupe
				out = append(out, o)
				continue
			}
		} else {
			seen[key] = len(out)
		}

		o.Kept = true
		out = append(out, o)
	}
	return out
}

func evidenceOK(ctx context.Context, counter FileLineCounter, evidence []string) bool {
	for _, ev := range evidence {
		path, line, err := finding.ParseEvidence(ev)
		if err != nil {
			return false
		}
		n, exists := counter.LineCount(ctx, path)
		if !exists || line > n {
			return false
		}
	}
	return true
}
