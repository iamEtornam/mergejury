// Package verify runs mechanical checks against a cluster before the judge.
// Ground truth beats opinion and is much cheaper. A check that errors out is
// inconclusive, not a pass.
package verify

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"revu/internal/cluster"
	"revu/internal/finding"
)

const (
	ConcludeSupports     = "supports"
	ConcludeRefutes      = "refutes"
	ConcludeInconclusive = "inconclusive"
)

type Result struct {
	Kind       string
	Command    string
	ExitCode   int
	Output     string
	Conclusion string
}

// Run applies the checks relevant to the cluster's category, in the
// worktree. commands come from verification.commands in config.
func Run(ctx context.Context, workDir string, commands map[string]string, c *cluster.Cluster) []Result {
	var out []Result
	if workDir == "" {
		return nil // no local checkout: nothing mechanical to check
	}
	switch c.Category {
	case finding.CatTestGap:
		out = append(out, checkTestGap(workDir, c))
	case finding.CatAPIBreak:
		out = append(out, checkCallSites(ctx, workDir, c))
	case finding.CatStyle, finding.CatBug:
		if cmd, ok := commands["lint"]; ok {
			out = append(out, runCommand(ctx, workDir, "lint", cmd, c))
		}
	}
	if cmd, ok := commands["typecheck"]; ok && c.Category != finding.CatTestGap {
		out = append(out, runCommand(ctx, workDir, "typecheck", cmd, c))
	}
	out = append(out, checkAnchorContent(workDir, c))
	return out
}

// checkTestGap: does a test file matching the claimed gap actually not
// exist? Existence of a plausible sibling test file is evidence against.
func checkTestGap(workDir string, c *cluster.Cluster) Result {
	r := Result{Kind: "test-gap-file"}
	dir := filepath.Dir(c.Path)
	base := strings.TrimSuffix(filepath.Base(c.Path), filepath.Ext(c.Path))
	candidates := []string{
		filepath.Join(dir, base+"_test.go"),
		filepath.Join(dir, base+".test.ts"),
		filepath.Join(dir, base+".spec.ts"),
		filepath.Join(dir, base+".test.js"),
		filepath.Join(dir, "test_"+base+".py"),
		filepath.Join(dir, base+"_test.py"),
	}
	var found []string
	for _, cand := range candidates {
		if _, err := os.Stat(filepath.Join(workDir, cand)); err == nil {
			found = append(found, cand)
		}
	}
	if len(found) > 0 {
		r.Output = "sibling test file(s) exist: " + strings.Join(found, ", ")
		r.Conclusion = ConcludeInconclusive // a file existing does not prove the branch is covered
		return r
	}
	r.Output = "no sibling test file found for " + c.Path
	r.Conclusion = ConcludeSupports
	return r
}

var identRe = regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_]{2,}\b`)

// checkCallSites: for a claimed breaking change, count references to
// identifiers named in the finding titles elsewhere in the repo.
func checkCallSites(ctx context.Context, workDir string, c *cluster.Cluster) Result {
	r := Result{Kind: "api-break-callsites"}
	idents := map[string]bool{}
	for _, it := range c.Items {
		for _, m := range identRe.FindAllString(it.Finding.Title, -1) {
			// Heuristic: exported-looking or snake identifiers are worth
			// grepping; short common words are not.
			if len(m) >= 4 && strings.ToLower(m) != m || strings.Contains(m, "_") {
				idents[m] = true
			}
		}
	}
	if len(idents) == 0 {
		r.Output = "no greppable identifier in finding titles"
		r.Conclusion = ConcludeInconclusive
		return r
	}
	total := 0
	var lines []string
	for ident := range idents {
		cmd := exec.CommandContext(ctx, "git", "grep", "-c", "--", ident)
		cmd.Dir = workDir
		out, _ := cmd.Output() // exit 1 = no matches, fine
		n := 0
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if i := strings.LastIndexByte(l, ':'); i > 0 {
				var c int
				fmt.Sscanf(l[i+1:], "%d", &c)
				n += c
			}
		}
		lines = append(lines, fmt.Sprintf("%s: %d reference(s)", ident, n))
		total += n
	}
	r.Command = "git grep -c <identifiers>"
	r.Output = strings.Join(lines, "\n")
	if total > len(idents) { // more references than definitions
		r.Conclusion = ConcludeSupports
	} else {
		r.Conclusion = ConcludeRefutes // claimed break with no callers in-repo
	}
	return r
}

// checkAnchorContent: does the anchored line exist in the worktree file at
// all? Catches clusters pointing at code that is not there.
func checkAnchorContent(workDir string, c *cluster.Cluster) Result {
	r := Result{Kind: "anchor-content"}
	b, err := os.ReadFile(filepath.Join(workDir, c.Path))
	if err != nil {
		r.Output = "cannot read " + c.Path + ": " + err.Error()
		r.Conclusion = ConcludeInconclusive
		return r
	}
	lines := strings.Split(string(b), "\n")
	if c.Line > len(lines) {
		r.Output = fmt.Sprintf("anchor line %d past end of file (%d lines)", c.Line, len(lines))
		r.Conclusion = ConcludeRefutes
		return r
	}
	r.Output = fmt.Sprintf("line %d: %s", c.Line, strings.TrimSpace(lines[c.Line-1]))
	r.Conclusion = ConcludeSupports
	return r
}

// runCommand executes a configured verification command in the worktree.
// {{.Paths}} expands to the cluster's file.
func runCommand(ctx context.Context, workDir, kind, command string, c *cluster.Cluster) Result {
	cmdStr := strings.ReplaceAll(command, "{{.Paths}}", c.Path)
	r := Result{Kind: kind, Command: cmdStr}
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	if len(out) > 16<<10 {
		out = out[:16<<10]
	}
	r.Output = string(out)
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			r.ExitCode = ee.ExitCode()
			// Nonzero exit from a linter/typechecker means it found
			// something; whether that supports the finding is the judge's
			// call. Report as-is.
			r.Conclusion = ConcludeInconclusive
			return r
		}
		r.Output += "\n(command failed to run: " + err.Error() + ")"
		r.Conclusion = ConcludeInconclusive // an erroring check is not a pass
		return r
	}
	r.Conclusion = ConcludeInconclusive
	return r
}
