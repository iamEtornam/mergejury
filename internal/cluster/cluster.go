// Package cluster groups validated findings deterministically: plain code,
// no model, identical output across runs so replays reproduce clusters.
package cluster

import (
	"sort"

	"revu/internal/finding"
)

// Item is a kept finding with its store ID, ready to cluster.
type Item struct {
	FindingID int64
	Finding   finding.Finding
	Demoted   bool // body-level note; still clustered so it reaches the judge
}

// Cluster is a group of findings at the same (path, category) whose lines
// fall within the window.
type Cluster struct {
	Path       string
	Line       int // anchor: the line of the most severe finding
	Category   finding.Category
	Items      []Item
	Supporters []string // distinct adapter IDs, sorted
}

// MaxSeverity of any finding in the cluster.
func (c *Cluster) MaxSeverity() finding.Severity {
	best := finding.Severity("")
	for _, it := range c.Items {
		if it.Finding.Severity.Rank() > best.Rank() {
			best = it.Finding.Severity
		}
	}
	return best
}

// Group clusters items by (path, category) with line numbers within
// windowLines of the cluster's running span. Deterministic: items are sorted
// first, grouping is greedy in line order.
func Group(items []Item, windowLines int) []Cluster {
	if windowLines < 0 {
		windowLines = 0
	}
	sorted := make([]Item, len(items))
	copy(sorted, items)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i].Finding, sorted[j].Finding
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Category != b.Category {
			return a.Category < b.Category
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return sorted[i].FindingID < sorted[j].FindingID
	})

	var out []Cluster
	for _, it := range sorted {
		f := it.Finding
		if n := len(out); n > 0 {
			last := &out[n-1]
			if last.Path == f.Path && last.Category == f.Category &&
				f.Line-last.Items[len(last.Items)-1].Finding.Line <= windowLines {
				last.Items = append(last.Items, it)
				continue
			}
		}
		out = append(out, Cluster{Path: f.Path, Line: f.Line, Category: f.Category, Items: []Item{it}})
	}

	for i := range out {
		c := &out[i]
		// Anchor on the most severe finding; ties go to the earliest line.
		best := c.Items[0]
		for _, it := range c.Items[1:] {
			if it.Finding.Severity.Rank() > best.Finding.Severity.Rank() {
				best = it
			}
		}
		c.Line = best.Finding.Line
		seen := map[string]bool{}
		for _, it := range c.Items {
			if !seen[it.Finding.ReviewerID] {
				seen[it.Finding.ReviewerID] = true
				c.Supporters = append(c.Supporters, it.Finding.ReviewerID)
			}
		}
		sort.Strings(c.Supporters)
	}
	return out
}
