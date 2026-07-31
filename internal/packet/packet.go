package packet

import (
	"fmt"
	"sort"
	"strings"
)

// Packet is the input every adapter receives for one run.
type Packet struct {
	Repo     string // owner/name, empty for local runs
	PRNumber int    // 0 for local runs
	Title    string
	Body     string
	BaseSHA  string
	HeadSHA  string
	IsFork   bool
	Files    []FileDiff

	// Commentable is path -> set of right-side (head) line numbers that a
	// finding may anchor to. This is the validation gate.
	Commentable map[string]map[int]bool

	// Rendered is the model-facing rendering from section 5.2.
	Rendered string

	// RepoDir is a local checkout used for worktrees and evidence checks.
	// Empty when no local repo is available (modelapi-only runs).
	RepoDir string

	ChangedLines int
	ChangedFiles int
}

// Build computes the commentable set, rendering, and change stats from Files.
func (p *Packet) Build() {
	p.Commentable = map[string]map[int]bool{}
	p.ChangedFiles = len(p.Files)
	p.ChangedLines = 0
	for _, f := range p.Files {
		if f.Binary || f.Status == StatusDeleted {
			continue
		}
		set := map[int]bool{}
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				switch l.Kind {
				case LineAdded:
					set[l.NewNum] = true
					p.ChangedLines++
				case LineContext:
					set[l.NewNum] = true
				case LineDeleted:
					p.ChangedLines++
				}
			}
		}
		if len(set) > 0 {
			p.Commentable[f.Path] = set
		}
	}
	p.Rendered = renderFiles(p.Files, p.Commentable)
}

// CommentableRanges compresses a file's commentable set into "a-b, c" form,
// sorted ascending.
func CommentableRanges(set map[int]bool) string {
	if len(set) == 0 {
		return ""
	}
	nums := make([]int, 0, len(set))
	for n := range set {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	var parts []string
	start, prev := nums[0], nums[0]
	flush := func() {
		if start == prev {
			parts = append(parts, fmt.Sprintf("%d", start))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", start, prev))
		}
	}
	for _, n := range nums[1:] {
		if n == prev+1 {
			prev = n
			continue
		}
		flush()
		start, prev = n, n
	}
	flush()
	return strings.Join(parts, ", ")
}

// renderFiles produces the section 5.2 rendering: absolute head line numbers,
// '+' marking added lines, deleted lines listed separately per hunk.
func renderFiles(files []FileDiff, commentable map[string]map[int]bool) string {
	var b strings.Builder
	for _, f := range files {
		fmt.Fprintf(&b, "=== %s ===\n", f.Path)
		if f.OldPath != "" && f.OldPath != f.Path {
			fmt.Fprintf(&b, "Renamed from %s\n", f.OldPath)
		}
		switch {
		case f.Binary:
			b.WriteString("Binary file, not commentable.\n\n")
			continue
		case f.Status == StatusDeleted:
			b.WriteString("File deleted in this PR, not commentable.\n\n")
			continue
		}
		ranges := CommentableRanges(commentable[f.Path])
		if ranges == "" {
			b.WriteString("No commentable lines in this file.\n\n")
			continue
		}
		fmt.Fprintf(&b, "You may only comment on these line numbers: %s\n\n", ranges)
		for hi, h := range f.Hunks {
			if hi > 0 {
				b.WriteString("     ... |\n")
			}
			var deleted []Line
			for _, l := range h.Lines {
				switch l.Kind {
				case LineDeleted:
					deleted = append(deleted, l)
				case LineAdded:
					fmt.Fprintf(&b, "%8d | + %s\n", l.NewNum, l.Content)
				case LineContext:
					fmt.Fprintf(&b, "%8d |   %s\n", l.NewNum, l.Content)
				}
			}
			if len(deleted) > 0 {
				b.WriteString("  Removed in this PR (old line numbers, do not anchor to these):\n")
				for _, l := range deleted {
					fmt.Fprintf(&b, "  - %d: %s\n", l.OldNum, l.Content)
				}
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}
