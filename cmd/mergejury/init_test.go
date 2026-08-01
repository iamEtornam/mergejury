package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/iamEtornam/mergejury/internal/config"
)

func det(kind, lens string, ok bool, remediation string) detection {
	return detection{
		candidate:   candidate{kind: kind, lens: lens},
		available:   ok,
		remediation: remediation,
	}
}

func TestPlanUsesOnlyAvailableTools(t *testing.T) {
	cfg, gaps := plan(config.Default(), []detection{
		det("claude-code", "security", true, ""),
		det("cursor", "correctness", false, "authenticate cursor-agent"),
		det("antigravity", "api-contract", false, "install agy"),
		det("modelapi", "test-gap", true, ""),
	})
	if len(cfg.Adapters) != 2 {
		t.Fatalf("adapters = %d, want 2", len(cfg.Adapters))
	}
	if cfg.Adapters[0].Kind != "claude-code" || cfg.Adapters[0].Lens != "security" {
		t.Errorf("first adapter = %+v", cfg.Adapters[0])
	}
	if cfg.Adapters[1].Kind != "modelapi" || cfg.Adapters[1].Lens != "test-gap" {
		t.Errorf("second adapter = %+v", cfg.Adapters[1])
	}
	if len(gaps) != 2 {
		t.Errorf("gaps = %v, want one per unavailable tool with remediation", gaps)
	}
	for _, g := range gaps {
		if !strings.Contains(g, ":") {
			t.Errorf("gap %q should name the tool and the fix", g)
		}
	}
}

// A lone API reviewer should get the broadest lens, not the narrow one it
// would take in a full ensemble.
func TestPlanBroadensLensWhenAloneOnModelAPI(t *testing.T) {
	cfg, _ := plan(config.Default(), []detection{
		det("claude-code", "security", false, "install claude"),
		det("modelapi", "test-gap", true, ""),
	})
	if len(cfg.Adapters) != 1 {
		t.Fatalf("adapters = %d, want 1", len(cfg.Adapters))
	}
	if cfg.Adapters[0].Lens != "correctness" {
		t.Errorf("lone modelapi lens = %s, want correctness", cfg.Adapters[0].Lens)
	}
}

func TestPlanWithNothingAvailable(t *testing.T) {
	cfg, gaps := plan(config.Default(), []detection{
		det("claude-code", "security", false, "install claude"),
		det("modelapi", "test-gap", false, "set ANTHROPIC_API_KEY"),
	})
	if len(cfg.Adapters) != 0 {
		t.Errorf("adapters = %d, want none", len(cfg.Adapters))
	}
	if len(gaps) != 2 {
		t.Errorf("gaps = %v, want 2", gaps)
	}
}

// The generated file is the thing users actually run with: it must parse back
// into the same adapters, or init has produced a broken config.
func TestRenderedConfigRoundTrips(t *testing.T) {
	cfg, _ := plan(config.Default(), []detection{
		det("claude-code", "security", true, ""),
		det("cursor", "correctness", true, ""),
		det("modelapi", "test-gap", true, ""),
	})
	body := renderConfig(cfg)

	var parsed config.Config
	if err := yaml.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("generated config does not parse: %v\n%s", err, body)
	}
	if len(parsed.Adapters) != len(cfg.Adapters) {
		t.Fatalf("round trip: %d adapters, want %d", len(parsed.Adapters), len(cfg.Adapters))
	}
	for i, a := range parsed.Adapters {
		if a.Kind != cfg.Adapters[i].Kind || a.Lens != cfg.Adapters[i].Lens {
			t.Errorf("adapter %d = %+v, want kind=%s lens=%s", i, a, cfg.Adapters[i].Kind, cfg.Adapters[i].Lens)
		}
		if a.Timeout != 5*time.Minute {
			t.Errorf("adapter %d timeout = %v, want 5m", i, a.Timeout)
		}
	}
	// Verdicts must ship off: turning them on is a decision, not a default.
	if parsed.Verdict.Enabled {
		t.Error("generated config enables verdicts; it must ship them off")
	}
}

func TestRenderedConfigHasNoCredentials(t *testing.T) {
	cfg, _ := plan(config.Default(), []detection{det("modelapi", "test-gap", true, "")})
	body := renderConfig(cfg)
	for _, forbidden := range []string{"ANTHROPIC_API_KEY:", "GITHUB_TOKEN:", "sk-ant", "ghp_"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("generated config contains %q; credentials must stay in the environment", forbidden)
		}
	}
}

func TestConfirm(t *testing.T) {
	var out bytes.Buffer
	// Non-interactive (a pipe, CI): never prompt, never accept.
	if confirm(strings.NewReader("y\n"), &out, false, false) {
		t.Error("must not accept without a terminal unless --yes")
	}
	if !strings.Contains(out.String(), "not a terminal") {
		t.Errorf("should say why it declined: %q", out.String())
	}
	// --yes wins regardless.
	out.Reset()
	if !confirm(strings.NewReader(""), &out, true, false) {
		t.Error("--yes should confirm without prompting")
	}
	// Interactive: the answer is read.
	for answer, want := range map[string]bool{"y\n": true, "yes\n": true, "n\n": false, "\n": false, "": false} {
		out.Reset()
		if got := confirm(strings.NewReader(answer), &out, false, true); got != want {
			t.Errorf("answer %q -> %v, want %v", answer, got, want)
		}
	}
}

func TestIsTerminalRejectsNonFiles(t *testing.T) {
	if isTerminal(strings.NewReader("")) {
		t.Error("a plain reader is not a terminal")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if isTerminal(r) {
		t.Error("a pipe is not a terminal")
	}
}
