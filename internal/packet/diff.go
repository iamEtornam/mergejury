// Package packet builds the input every adapter receives: parsed diff,
// commentable-line set, model-facing rendering, and per-adapter worktrees.
package packet

import (
	"fmt"
	"strconv"
	"strings"
)

type FileStatus string

const (
	StatusAdded    FileStatus = "added"
	StatusModified FileStatus = "modified"
	StatusDeleted  FileStatus = "deleted"
	StatusRenamed  FileStatus = "renamed"
)

type LineKind byte

const (
	LineContext LineKind = ' '
	LineAdded   LineKind = '+'
	LineDeleted LineKind = '-'
)

// Line is one diff line. NewNum is the 1-indexed line number in the head SHA
// (0 for deleted lines); OldNum is the base-side number (0 for added lines).
type Line struct {
	Kind    LineKind
	OldNum  int
	NewNum  int
	Content string
}

type Hunk struct {
	OldStart, OldCount, NewStart, NewCount int
	Header                                 string // trailing context after @@, if any
	Lines                                  []Line
}

type FileDiff struct {
	Path    string // head-side path
	OldPath string // set when renamed
	Status  FileStatus
	Binary  bool
	Hunks   []Hunk
}

// ParseUnifiedDiff parses `git diff` output into FileDiffs. It handles new,
// deleted, renamed and binary files, multiple hunks, CRLF content, and the
// "\ No newline at end of file" marker.
func ParseUnifiedDiff(text string) ([]FileDiff, error) {
	var files []FileDiff
	var cur *FileDiff
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		ln := lines[i]
		switch {
		case strings.HasPrefix(ln, "diff --git "):
			files = append(files, FileDiff{Status: StatusModified})
			cur = &files[len(files)-1]
			a, b := parseGitHeaderPaths(ln)
			cur.Path = b
			if a != b {
				cur.OldPath = a
			}
		case cur == nil:
			continue // preamble before first file header
		case strings.HasPrefix(ln, "new file mode"):
			cur.Status = StatusAdded
		case strings.HasPrefix(ln, "deleted file mode"):
			cur.Status = StatusDeleted
		case strings.HasPrefix(ln, "rename from "):
			cur.OldPath = strings.TrimPrefix(ln, "rename from ")
			cur.Status = StatusRenamed
		case strings.HasPrefix(ln, "rename to "):
			cur.Path = strings.TrimPrefix(ln, "rename to ")
			cur.Status = StatusRenamed
		case strings.HasPrefix(ln, "Binary files ") || strings.HasPrefix(ln, "GIT binary patch"):
			cur.Binary = true
		case strings.HasPrefix(ln, "--- "):
			// path lines; the +++ side is authoritative for head path
		case strings.HasPrefix(ln, "+++ "):
			p := strings.TrimPrefix(ln, "+++ ")
			p = strings.TrimSuffix(p, "\t")
			if p != "/dev/null" {
				cur.Path = strings.TrimPrefix(p, "b/")
			}
		case strings.HasPrefix(ln, "@@ "):
			h, err := parseHunkHeader(ln)
			if err != nil {
				return nil, fmt.Errorf("file %s: %w", cur.Path, err)
			}
			body, consumed := collectHunkBody(lines[i+1:], h)
			h.Lines = body
			cur.Hunks = append(cur.Hunks, h)
			i += consumed
		}
	}
	return files, nil
}

// parseGitHeaderPaths extracts a/b paths from a `diff --git a/x b/y` line.
// Paths with spaces are ambiguous in this header; the ---/+++ and rename
// lines correct them when present.
func parseGitHeaderPaths(ln string) (a, b string) {
	rest := strings.TrimPrefix(ln, "diff --git ")
	if i := strings.Index(rest, " b/"); i >= 0 {
		return strings.TrimPrefix(rest[:i], "a/"), rest[i+3:]
	}
	parts := strings.Fields(rest)
	if len(parts) == 2 {
		return strings.TrimPrefix(parts[0], "a/"), strings.TrimPrefix(parts[1], "b/")
	}
	return rest, rest
}

func parseHunkHeader(ln string) (Hunk, error) {
	// @@ -oldStart[,oldCount] +newStart[,newCount] @@ optional section
	var h Hunk
	rest := strings.TrimPrefix(ln, "@@ ")
	end := strings.Index(rest, " @@")
	if end < 0 {
		return h, fmt.Errorf("malformed hunk header %q", ln)
	}
	h.Header = strings.TrimPrefix(rest[end+3:], " ")
	ranges := strings.Fields(rest[:end])
	if len(ranges) != 2 {
		return h, fmt.Errorf("malformed hunk header %q", ln)
	}
	var err error
	h.OldStart, h.OldCount, err = parseRange(strings.TrimPrefix(ranges[0], "-"))
	if err != nil {
		return h, fmt.Errorf("hunk header %q: %w", ln, err)
	}
	h.NewStart, h.NewCount, err = parseRange(strings.TrimPrefix(ranges[1], "+"))
	if err != nil {
		return h, fmt.Errorf("hunk header %q: %w", ln, err)
	}
	return h, nil
}

func parseRange(s string) (start, count int, err error) {
	count = 1
	if i := strings.IndexByte(s, ','); i >= 0 {
		count, err = strconv.Atoi(s[i+1:])
		if err != nil {
			return 0, 0, err
		}
		s = s[:i]
	}
	start, err = strconv.Atoi(s)
	return start, count, err
}

// collectHunkBody reads hunk lines until both side counts are satisfied,
// assigning absolute line numbers. Returns the lines and how many raw lines
// were consumed (including any "\ No newline" markers).
func collectHunkBody(raw []string, h Hunk) ([]Line, int) {
	var out []Line
	oldN, newN := h.OldStart, h.NewStart
	oldLeft, newLeft := h.OldCount, h.NewCount
	consumed := 0
	for _, ln := range raw {
		if oldLeft <= 0 && newLeft <= 0 {
			// allow a trailing no-newline marker after the last line
			if strings.HasPrefix(ln, `\`) {
				consumed++
			}
			break
		}
		consumed++
		if strings.HasPrefix(ln, `\`) { // "\ No newline at end of file"
			continue
		}
		if ln == "" {
			// an empty context line (git emits a lone space, but be tolerant)
			out = append(out, Line{Kind: LineContext, OldNum: oldN, NewNum: newN})
			oldN, newN, oldLeft, newLeft = oldN+1, newN+1, oldLeft-1, newLeft-1
			continue
		}
		content := strings.TrimSuffix(ln[1:], "\r")
		switch ln[0] {
		case ' ':
			out = append(out, Line{Kind: LineContext, OldNum: oldN, NewNum: newN, Content: content})
			oldN, newN, oldLeft, newLeft = oldN+1, newN+1, oldLeft-1, newLeft-1
		case '+':
			out = append(out, Line{Kind: LineAdded, NewNum: newN, Content: content})
			newN, newLeft = newN+1, newLeft-1
		case '-':
			out = append(out, Line{Kind: LineDeleted, OldNum: oldN, Content: content})
			oldN, oldLeft = oldN+1, oldLeft-1
		default:
			// not a hunk line; back out
			consumed--
			return out, consumed
		}
	}
	return out, consumed
}

// ParsePatchHunks parses a bare patch body (GitHub's per-file `patch` field,
// which has hunk headers but no file headers).
func ParsePatchHunks(patch string) ([]Hunk, error) {
	if strings.TrimSpace(patch) == "" {
		return nil, nil
	}
	var hunks []Hunk
	lines := strings.Split(patch, "\n")
	for i := 0; i < len(lines); i++ {
		if !strings.HasPrefix(lines[i], "@@ ") {
			continue
		}
		h, err := parseHunkHeader(lines[i])
		if err != nil {
			return nil, err
		}
		body, consumed := collectHunkBody(lines[i+1:], h)
		h.Lines = body
		hunks = append(hunks, h)
		i += consumed
	}
	return hunks, nil
}
