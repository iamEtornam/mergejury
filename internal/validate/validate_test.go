package validate

import (
	"context"
	"testing"

	"github.com/iamEtornam/mergejury/internal/finding"
	"github.com/iamEtornam/mergejury/internal/packet"
)

func testPacket() *packet.Packet {
	// commentable: main.go lines 10-15, util.go line 3
	return &packet.Packet{Commentable: map[string]map[int]bool{
		"main.go": {10: true, 11: true, 12: true, 13: true, 14: true, 15: true},
		"util.go": {3: true},
	}}
}

func testCounter() MapLineCounter {
	return MapLineCounter{Lines: map[string]int{"main.go": 210, "util.go": 40}}
}

func good(line int) finding.Finding {
	return finding.Finding{
		ReviewerID: "a1", Lens: "correctness", Path: "main.go", Line: line,
		Category: finding.CatBug, Severity: finding.SevMinor, Title: "t",
		Evidence: []string{"main.go:12"}, Confidence: finding.ConfHigh,
	}
}

func TestValidationTable(t *testing.T) {
	ctx := context.Background()
	intp := func(n int) *int { return &n }

	tests := []struct {
		name       string
		f          finding.Finding
		wantKept   bool
		wantDemote bool
		wantReason string
	}{
		{"valid", good(12), true, false, ""},
		{
			"schema: unknown category",
			func() finding.Finding { f := good(12); f.Category = "vibes"; return f }(),
			false, false, ReasonSchema,
		},
		{
			"schema: unknown severity",
			func() finding.Finding { f := good(12); f.Severity = "catastrophic"; return f }(),
			false, false, ReasonSchema,
		},
		{
			"schema: empty evidence",
			func() finding.Finding { f := good(12); f.Evidence = nil; return f }(),
			false, false, ReasonSchema,
		},
		{
			"schema: start_line == line",
			func() finding.Finding { f := good(12); f.StartLine = intp(12); return f }(),
			false, false, ReasonSchema,
		},
		{
			"anchor one line outside commentable set, minor: drop",
			good(16), // 15 is the last commentable line
			false, false, ReasonUnanchored,
		},
		{
			"anchor outside commentable set, major: demote to body note",
			func() finding.Finding { f := good(16); f.Severity = finding.SevMajor; return f }(),
			true, true, ReasonUnanchored,
		},
		{
			"evidence past end of file",
			func() finding.Finding { f := good(12); f.Evidence = []string{"util.go:400"}; return f }(),
			false, false, ReasonBadEvidence,
		},
		{
			"evidence file does not exist",
			func() finding.Finding { f := good(12); f.Evidence = []string{"ghost.go:1"}; return f }(),
			false, false, ReasonBadEvidence,
		},
		{
			"multiline: start_line not commentable",
			func() finding.Finding { f := good(12); f.StartLine = intp(5); return f }(),
			false, false, ReasonMultiline,
		},
		{
			"multiline: valid range",
			func() finding.Finding { f := good(12); f.StartLine = intp(10); return f }(),
			true, false, "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Run(ctx, testPacket(), testCounter(), []finding.Finding{tc.f})
			if len(got) != 1 {
				t.Fatalf("got %d outcomes", len(got))
			}
			o := got[0]
			if o.Kept != tc.wantKept || o.Demoted != tc.wantDemote || o.DropReason != tc.wantReason {
				t.Errorf("kept=%v demoted=%v reason=%q; want kept=%v demoted=%v reason=%q",
					o.Kept, o.Demoted, o.DropReason, tc.wantKept, tc.wantDemote, tc.wantReason)
			}
		})
	}
}

func TestDedupeKeepsHighestSeverity(t *testing.T) {
	ctx := context.Background()
	lo := good(12)
	hi := good(12)
	hi.Severity = finding.SevBlocker

	// lower first, higher second: higher wins, first is retro-dropped
	got := Run(ctx, testPacket(), testCounter(), []finding.Finding{lo, hi})
	if got[0].Kept || got[0].DropReason != ReasonDupe {
		t.Errorf("low-severity dupe should be dropped, got %+v", got[0])
	}
	if !got[1].Kept {
		t.Errorf("high-severity dupe should be kept")
	}

	// higher first, lower second: second is dropped
	got = Run(ctx, testPacket(), testCounter(), []finding.Finding{hi, lo})
	if !got[0].Kept {
		t.Errorf("high-severity original should be kept")
	}
	if got[1].Kept || got[1].DropReason != ReasonDupe {
		t.Errorf("low-severity dupe should be dropped, got %+v", got[1])
	}
}

func TestDifferentAdaptersNotDeduped(t *testing.T) {
	ctx := context.Background()
	a := good(12)
	b := good(12)
	b.ReviewerID = "a2"
	got := Run(ctx, testPacket(), testCounter(), []finding.Finding{a, b})
	if !got[0].Kept || !got[1].Kept {
		t.Errorf("cross-adapter agreement must survive validation (it becomes the agreement count)")
	}
}
