package packet

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

func loadFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("../../testdata/diffs", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(b)
}

func lineSet(nums ...int) map[int]bool {
	m := map[int]bool{}
	for _, n := range nums {
		m[n] = true
	}
	return m
}

func rangeSet(a, b int) map[int]bool {
	m := map[int]bool{}
	for n := a; n <= b; n++ {
		m[n] = true
	}
	return m
}

// TestGoldenCommentableSets is the anchoring test: every fixture case from
// section 15 with its expected commentable line set.
func TestGoldenCommentableSets(t *testing.T) {
	files, err := ParseUnifiedDiff(loadFixture(t, "kitchen_sink.diff"))
	if err != nil {
		t.Fatal(err)
	}
	p := &Packet{Files: files}
	p.Build()

	want := map[string]map[int]bool{
		"crlf.txt":           lineSet(1, 2, 3),
		"eof.txt":            lineSet(1, 2, 3, 4), // hunk at end of file, added last line
		"multi.txt":          rangeSet(1, 11),
		"newfile.txt":        lineSet(1, 2, 3), // new file
		"nonewline.txt":      lineSet(1, 2),    // no trailing newline
		"renamed_edited.txt": lineSet(1, 2, 3, 4),
		"top.txt":            lineSet(1, 2, 3), // hunk at line 1
	}
	// Not commentable at all: binary, deleted, pure rename.
	for _, path := range []string{"bin.dat", "delete_me.txt", "renamed.txt"} {
		if _, ok := p.Commentable[path]; ok {
			t.Errorf("%s must not be commentable", path)
		}
	}
	if len(p.Commentable) != len(want) {
		t.Errorf("commentable files = %d, want %d: %v", len(p.Commentable), len(want), keys(p.Commentable))
	}
	for path, ws := range want {
		got := p.Commentable[path]
		if got == nil {
			t.Errorf("%s: missing from commentable set", path)
			continue
		}
		for n := range ws {
			if !got[n] {
				t.Errorf("%s: line %d should be commentable", path, n)
			}
		}
		for n := range got {
			if !ws[n] {
				t.Errorf("%s: line %d should NOT be commentable", path, n)
			}
		}
	}
}

func TestRenameMetadata(t *testing.T) {
	files, err := ParseUnifiedDiff(loadFixture(t, "kitchen_sink.diff"))
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]FileDiff{}
	for _, f := range files {
		byPath[f.Path] = f
	}
	if f := byPath["renamed.txt"]; f.Status != StatusRenamed || f.OldPath != "rename_src.txt" {
		t.Errorf("pure rename: got status=%s old=%s", f.Status, f.OldPath)
	}
	if f := byPath["renamed_edited.txt"]; f.Status != StatusRenamed || f.OldPath != "rename_edit_src.txt" || len(f.Hunks) != 1 {
		t.Errorf("rename with edits: got status=%s old=%s hunks=%d", f.Status, f.OldPath, len(f.Hunks))
	}
	if f := byPath["delete_me.txt"]; f.Status != StatusDeleted {
		t.Errorf("deleted file: got status=%s", f.Status)
	}
	if f := byPath["newfile.txt"]; f.Status != StatusAdded {
		t.Errorf("new file: got status=%s", f.Status)
	}
	if f := byPath["bin.dat"]; !f.Binary {
		t.Errorf("binary file not detected")
	}
}

func TestTwoHunksOneFile(t *testing.T) {
	files, err := ParseUnifiedDiff(loadFixture(t, "two_hunks.diff"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || len(files[0].Hunks) != 2 {
		t.Fatalf("want 1 file with 2 hunks, got %d files, %d hunks", len(files), len(files[0].Hunks))
	}
	p := &Packet{Files: files}
	p.Build()
	got := p.Commentable["big.txt"]
	for n := 2; n <= 8; n++ {
		if !got[n] {
			t.Errorf("line %d should be commentable", n)
		}
	}
	for n := 27; n <= 33; n++ {
		if !got[n] {
			t.Errorf("line %d should be commentable", n)
		}
	}
	if got[9] || got[26] || got[1] || got[34] {
		t.Errorf("lines outside hunks leaked into commentable set")
	}
	if want := "2-8, 27-33"; CommentableRanges(got) != want {
		t.Errorf("ranges = %q, want %q", CommentableRanges(got), want)
	}
}

func TestCRLFContentStripped(t *testing.T) {
	files, err := ParseUnifiedDiff(loadFixture(t, "kitchen_sink.diff"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Path != "crlf.txt" {
			continue
		}
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				if strings.ContainsRune(l.Content, '\r') {
					t.Errorf("CRLF not stripped from rendered content: %q", l.Content)
				}
			}
		}
	}
}

// TestGoldenRendering pins the exact model-facing rendering from section 5.2.
func TestGoldenRendering(t *testing.T) {
	for _, name := range []string{"kitchen_sink", "two_hunks"} {
		files, err := ParseUnifiedDiff(loadFixture(t, name+".diff"))
		if err != nil {
			t.Fatal(err)
		}
		p := &Packet{Files: files}
		p.Build()
		goldenPath := filepath.Join("../../testdata/diffs", name+".rendered.golden")
		if *update {
			if err := os.WriteFile(goldenPath, []byte(p.Rendered), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(goldenPath)
		if err != nil {
			t.Fatalf("missing golden (run with -update): %v", err)
		}
		if p.Rendered != string(want) {
			t.Errorf("%s: rendered output diverged from golden.\ngot:\n%s", name, p.Rendered)
		}
	}
}

func TestParsePatchHunks(t *testing.T) {
	// GitHub's per-file patch field: hunks only, no file headers.
	patch := "@@ -1,3 +1,4 @@\n a\n+b\n c\n d"
	hunks, err := ParsePatchHunks(patch)
	if err != nil {
		t.Fatal(err)
	}
	if len(hunks) != 1 || len(hunks[0].Lines) != 4 {
		t.Fatalf("got %d hunks", len(hunks))
	}
	if hunks[0].Lines[1].Kind != LineAdded || hunks[0].Lines[1].NewNum != 2 {
		t.Errorf("added line misnumbered: %+v", hunks[0].Lines[1])
	}
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
