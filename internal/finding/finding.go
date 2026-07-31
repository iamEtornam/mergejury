// Package finding defines the Finding contract, the load-bearing type of the
// whole system. Adapters produce it, the store persists it, the judge consumes
// it, the poster renders it.
package finding

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type Category string

const (
	CatBug         Category = "bug"
	CatSecurity    Category = "security"
	CatPerf        Category = "perf"
	CatCorrectness Category = "correctness"
	CatAPIBreak    Category = "api-break"
	CatTestGap     Category = "test-gap"
	CatStyle       Category = "style"
)

var Categories = []Category{CatBug, CatSecurity, CatPerf, CatCorrectness, CatAPIBreak, CatTestGap, CatStyle}

type Severity string

const (
	SevBlocker Severity = "blocker"
	SevMajor   Severity = "major"
	SevMinor   Severity = "minor"
	SevNit     Severity = "nit"
)

// Rank orders severities: higher is more severe. Unknown ranks below nit.
func (s Severity) Rank() int {
	switch s {
	case SevBlocker:
		return 4
	case SevMajor:
		return 3
	case SevMinor:
		return 2
	case SevNit:
		return 1
	}
	return 0
}

type Confidence string

const (
	ConfHigh   Confidence = "high"
	ConfMedium Confidence = "medium"
	ConfLow    Confidence = "low"
)

// Finding is one reviewer claim, anchored to the head-SHA version of a file.
// Side is always RIGHT and is therefore not part of the schema.
type Finding struct {
	ReviewerID     string     `json:"reviewer_id"`
	Lens           string     `json:"lens"`
	Path           string     `json:"path"`
	Line           int        `json:"line"`
	StartLine      *int       `json:"start_line"`
	Category       Category   `json:"category"`
	Severity       Severity   `json:"severity"`
	Title          string     `json:"title"`
	Body           string     `json:"body"`
	SuggestedPatch *string    `json:"suggested_patch"`
	Evidence       []string   `json:"evidence"`
	Confidence     Confidence `json:"confidence"`
}

// SchemaErr describes why a finding failed schema validation.
type SchemaErr struct{ Field, Reason string }

func (e SchemaErr) Error() string { return fmt.Sprintf("finding schema: %s: %s", e.Field, e.Reason) }

// ValidateSchema checks structural validity only (enums, required fields).
// Anchor and evidence checks live in the validate package because they need
// the packet.
func (f *Finding) ValidateSchema() error {
	if f.Path == "" {
		return SchemaErr{"path", "empty"}
	}
	if f.Line < 1 {
		return SchemaErr{"line", "must be >= 1"}
	}
	if f.StartLine != nil && *f.StartLine >= f.Line {
		return SchemaErr{"start_line", "must be < line"}
	}
	if !validCategory(f.Category) {
		return SchemaErr{"category", "unknown value " + strconv.Quote(string(f.Category))}
	}
	if f.Severity.Rank() == 0 {
		return SchemaErr{"severity", "unknown value " + strconv.Quote(string(f.Severity))}
	}
	switch f.Confidence {
	case ConfHigh, ConfMedium, ConfLow:
	default:
		return SchemaErr{"confidence", "unknown value " + strconv.Quote(string(f.Confidence))}
	}
	if f.Title == "" {
		return SchemaErr{"title", "empty"}
	}
	if len(f.Evidence) == 0 {
		return SchemaErr{"evidence", "required and must be non-empty"}
	}
	for _, ev := range f.Evidence {
		if _, _, err := ParseEvidence(ev); err != nil {
			return SchemaErr{"evidence", err.Error()}
		}
	}
	return nil
}

func validCategory(c Category) bool {
	for _, k := range Categories {
		if c == k {
			return true
		}
	}
	return false
}

// ParseEvidence splits "path:line" into its parts.
func ParseEvidence(ev string) (path string, line int, err error) {
	i := strings.LastIndexByte(ev, ':')
	if i <= 0 || i == len(ev)-1 {
		return "", 0, fmt.Errorf("evidence %q is not path:line", ev)
	}
	n, err := strconv.Atoi(ev[i+1:])
	if err != nil || n < 1 {
		return "", 0, fmt.Errorf("evidence %q has invalid line number", ev)
	}
	return ev[:i], n, nil
}

// ParseFindings parses an adapter's JSON output into findings. It accepts
// either a bare array or an object with a "findings" key (plus optional
// "omissions"), and tolerates markdown code fences around the JSON. An empty
// findings array is a valid, first-class result.
func ParseFindings(raw string) ([]Finding, []string, error) {
	s := ExtractJSON(raw)
	if s == "" {
		return nil, nil, fmt.Errorf("no JSON found in output")
	}
	var wrapper struct {
		Findings  []Finding `json:"findings"`
		Omissions []string  `json:"omissions"`
	}
	if strings.HasPrefix(strings.TrimSpace(s), "[") {
		var arr []Finding
		if err := json.Unmarshal([]byte(s), &arr); err != nil {
			return nil, nil, fmt.Errorf("parse findings array: %w", err)
		}
		return arr, nil, nil
	}
	if err := json.Unmarshal([]byte(s), &wrapper); err != nil {
		return nil, nil, fmt.Errorf("parse findings object: %w", err)
	}
	return wrapper.Findings, wrapper.Omissions, nil
}

// ExtractJSON pulls the first JSON object or array out of text that may wrap
// it in markdown fences or prose. Returns "" if none found.
func ExtractJSON(raw string) string {
	s := strings.TrimSpace(raw)
	// Strip a ```json ... ``` fence if the whole thing is fenced.
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			body := s[i+1:]
			if j := strings.LastIndex(body, "```"); j >= 0 {
				s = strings.TrimSpace(body[:j])
			}
		}
	}
	start := strings.IndexAny(s, "{[")
	if start < 0 {
		return ""
	}
	open := s[start]
	var close byte = '}'
	if open == '[' {
		close = ']'
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}
