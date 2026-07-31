package cluster

import (
	"reflect"
	"testing"

	"mergejury/internal/finding"
)

func item(id int64, adapter, path string, line int, cat finding.Category, sev finding.Severity) Item {
	return Item{FindingID: id, Finding: finding.Finding{
		ReviewerID: adapter, Path: path, Line: line, Category: cat, Severity: sev,
	}}
}

func TestGroupWindowAndAgreement(t *testing.T) {
	items := []Item{
		item(1, "a1", "x.go", 10, finding.CatBug, finding.SevMinor),
		item(2, "a2", "x.go", 12, finding.CatBug, finding.SevBlocker), // within window 3 of line 10
		item(3, "a1", "x.go", 40, finding.CatBug, finding.SevMinor),   // far away: own cluster
		item(4, "a1", "x.go", 11, finding.CatPerf, finding.SevMinor),  // different category: own cluster
		item(5, "a3", "y.go", 10, finding.CatBug, finding.SevMinor),   // different file: own cluster
	}
	got := Group(items, 3)
	if len(got) != 4 {
		t.Fatalf("clusters = %d, want 4", len(got))
	}
	first := got[0]
	if len(first.Items) != 2 || !reflect.DeepEqual(first.Supporters, []string{"a1", "a2"}) {
		t.Errorf("agreement cluster wrong: %+v", first)
	}
	// Anchor follows the most severe finding.
	if first.Line != 12 || first.MaxSeverity() != finding.SevBlocker {
		t.Errorf("anchor should follow the blocker: line=%d sev=%s", first.Line, first.MaxSeverity())
	}
}

func TestGroupDeterministicAcrossInputOrder(t *testing.T) {
	items := []Item{
		item(1, "a1", "x.go", 10, finding.CatBug, finding.SevMinor),
		item(2, "a2", "x.go", 12, finding.CatBug, finding.SevMajor),
		item(3, "a3", "y.go", 5, finding.CatPerf, finding.SevNit),
	}
	reversed := []Item{items[2], items[1], items[0]}
	a := Group(items, 3)
	b := Group(reversed, 3)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("clustering must be identical across input orders:\n%+v\n%+v", a, b)
	}
}
