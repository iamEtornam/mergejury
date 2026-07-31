package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"revu/internal/config"
	"revu/internal/finding"
	"revu/internal/packet"
	"revu/prompts"
)

// findingSchemaJSON is the wrapper schema passed to CLIs that support
// schema-constrained output. Schema-constrained output beats any amount of
// prompt instruction.
const findingSchemaJSON = `{
  "type": "object",
  "properties": {
    "findings": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "path": {"type": "string"},
          "line": {"type": "integer"},
          "start_line": {"type": ["integer", "null"]},
          "category": {"type": "string", "enum": ["bug", "security", "perf", "correctness", "api-break", "test-gap", "style"]},
          "severity": {"type": "string", "enum": ["blocker", "major", "minor", "nit"]},
          "title": {"type": "string"},
          "body": {"type": "string"},
          "suggested_patch": {"type": ["string", "null"]},
          "evidence": {"type": "array", "items": {"type": "string"}},
          "confidence": {"type": "string", "enum": ["high", "medium", "low"]}
        },
        "required": ["path", "line", "category", "severity", "title", "body", "evidence", "confidence"]
      }
    },
    "omissions": {"type": "array", "items": {"type": "string"}}
  },
  "required": ["findings"]
}`

// cliAdapter is the shared machinery for subprocess-based reviewers. The
// three tools differ only in binary name, argument construction, auth
// probing, and stderr classification.
type cliAdapter struct {
	cfg     config.Adapter
	prompts *prompts.Set
	runner  Runner
	binary  string

	// wantFlags are probed against --help once; buildArgs receives the
	// subset that is actually supported, because flag surfaces drift.
	wantFlags []string
	buildArgs func(supported map[string]bool, system, user string, cfg config.Adapter) []string

	// authProbe is a cheap authenticated subcommand ("status", "models").
	// Empty means auth can only be confirmed by a real run.
	authProbe []string
	// authErrPatterns classify stderr/stdout as an auth failure.
	authErrPatterns []*regexp.Regexp
	// denyPatterns detect soft-denied tool use on stderr (exit 0, shallow
	// review): Status becomes denied so the run is degraded loudly.
	denyPatterns []*regexp.Regexp

	flagsOnce sync.Once
	flags     map[string]bool
	flagsErr  error
}

func (c *cliAdapter) ID() string    { return c.cfg.ID }
func (c *cliAdapter) Lens() string  { return c.cfg.Lens }
func (c *cliAdapter) Model() string { return c.cfg.Model }

func (c *cliAdapter) supportedFlags(ctx context.Context) (map[string]bool, error) {
	c.flagsOnce.Do(func() {
		res := c.runner(ctx, "", nil, c.binary, "--help")
		if res.Err != nil {
			c.flagsErr = res.Err
			return
		}
		help := res.Stdout + "\n" + res.Stderr
		c.flags = map[string]bool{}
		for _, f := range c.wantFlags {
			if strings.Contains(help, f) {
				c.flags[f] = true
			}
		}
	})
	return c.flags, c.flagsErr
}

func (c *cliAdapter) Probe(ctx context.Context) ProbeResult {
	pr := ProbeResult{AdapterID: c.cfg.ID}
	if _, err := exec.LookPath(c.binary); err != nil {
		pr.Detail = fmt.Sprintf("%s not found on PATH", c.binary)
		pr.Remediation = fmt.Sprintf("install %s and ensure it is on PATH", c.binary)
		return pr
	}
	flags, err := c.supportedFlags(ctx)
	if err != nil {
		pr.Detail = fmt.Sprintf("%s --help failed: %v", c.binary, err)
		pr.Remediation = fmt.Sprintf("run `%s --help` manually and fix the installation", c.binary)
		return pr
	}
	var missing []string
	for _, f := range c.wantFlags {
		if !flags[f] {
			missing = append(missing, f)
		}
	}
	if len(c.authProbe) > 0 {
		res := c.runner(ctx, "", nil, c.binary, c.authProbe...)
		combined := res.Stdout + res.Stderr
		if res.Err != nil || res.ExitCode != 0 || c.matchesAuthErr(combined) {
			pr.Detail = fmt.Sprintf("auth check `%s %s` failed: %s", c.binary, strings.Join(c.authProbe, " "), firstLine(combined))
			pr.Remediation = fmt.Sprintf("authenticate %s (run it interactively once, or set its API key env var)", c.binary)
			return pr
		}
	}
	pr.OK = true
	pr.Detail = "installed, authenticated"
	if len(c.authProbe) == 0 {
		pr.Detail = "installed (auth verified at run time)"
	}
	if len(missing) > 0 {
		pr.Detail += "; missing optional flags: " + strings.Join(missing, ", ")
	}
	return pr
}

func (c *cliAdapter) matchesAuthErr(s string) bool {
	for _, re := range c.authErrPatterns {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

func (c *cliAdapter) Review(ctx context.Context, p *packet.Packet, worktree string) Result {
	start := time.Now()
	res := Result{Status: StatusOK}
	system, err := c.prompts.Lens(c.cfg.Lens)
	if err != nil {
		res.Status = StatusCrashed
		res.Err = err.Error()
		return res
	}
	flags, err := c.supportedFlags(ctx)
	if err != nil {
		res.Status = StatusCrashed
		res.Err = fmt.Sprintf("probe %s: %v", c.binary, err)
		return res
	}
	if c.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.cfg.Timeout)
		defer cancel()
	}
	args := c.buildArgs(flags, system, userContent(p), c.cfg)
	er := c.runner(ctx, worktree, nil, c.binary, args...)
	res.Duration = time.Since(start)
	res.Raw = er.Stdout
	if er.Stderr != "" {
		res.Raw += "\n--- stderr ---\n" + er.Stderr
	}

	combined := er.Stdout + "\n" + er.Stderr
	switch {
	case er.TimedOut:
		res.Status = StatusTimeout
		res.Err = fmt.Sprintf("timed out after %s", c.cfg.Timeout)
		return res
	case er.Err != nil:
		res.Status = StatusCrashed
		res.Err = er.Err.Error()
		return res
	case c.matchesAuthErr(combined):
		// Auth errors can arrive with exit 0 in headless modes; check
		// before the exit code.
		res.Status = StatusAuthError
		res.Err = firstLine(combined)
		return res
	case er.ExitCode != 0:
		res.Status = StatusCrashed
		res.Err = fmt.Sprintf("exit %d: %s", er.ExitCode, firstLine(er.Stderr))
		return res
	}

	// Soft-denied tool use: run "succeeded" but the reviewer was blind.
	for _, re := range c.denyPatterns {
		if re.MatchString(er.Stderr) {
			res.Status = StatusDenied
			res.Err = "tool use soft-denied in headless mode: " + firstLine(er.Stderr)
			return res
		}
	}

	fs, omissions, cost, tokens, perr := parseCLIEnvelope(er.Stdout)
	res.CostUSD = cost
	res.Tokens = tokens
	if perr != nil {
		res.Status = StatusParseError
		res.Err = perr.Error()
		return res
	}
	res.Findings = stamp(fs, c.cfg.ID, c.cfg.Lens)
	res.Omissions = omissions
	return res
}

// cliEnvelope extends the generic envelope with claude's structured_output.
type cliEnvelope struct {
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	IsError          bool            `json:"is_error"`
	TotalCostUSD     float64         `json:"total_cost_usd"`
	Usage            struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

func parseCLIEnvelope(stdout string) ([]finding.Finding, []string, float64, TokenCounts, error) {
	inner := stdout
	var cost float64
	var tokens TokenCounts
	var env cliEnvelope
	if err := json.Unmarshal([]byte(finding.ExtractJSON(stdout)), &env); err == nil {
		cost = env.TotalCostUSD
		tokens = TokenCounts{Input: env.Usage.InputTokens, Output: env.Usage.OutputTokens}
		switch {
		case len(env.StructuredOutput) > 0 && string(env.StructuredOutput) != "null":
			inner = string(env.StructuredOutput)
		case env.Result != "":
			inner = env.Result
		}
	}
	fs, omissions, err := finding.ParseFindings(inner)
	if err != nil {
		return nil, nil, cost, tokens, err
	}
	return fs, omissions, cost, tokens, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// ---- claude-code ----

func newClaudeCode(cfg config.Adapter, ps *prompts.Set, runner Runner) Reviewer {
	return &cliAdapter{
		cfg: cfg, prompts: ps, runner: runner,
		binary: "claude",
		wantFlags: []string{
			"--print", "--output-format", "--allowedTools", "--disallowedTools",
			"--append-system-prompt", "--max-turns", "--json-schema", "--max-budget-usd", "--model",
		},
		buildArgs: func(sup map[string]bool, system, user string, cfg config.Adapter) []string {
			args := []string{"-p", user, "--output-format", "json"}
			if cfg.Model != "" && sup["--model"] {
				args = append(args, "--model", cfg.Model)
			}
			if sup["--allowedTools"] {
				args = append(args, "--allowedTools", "Read,Grep,Glob")
			}
			if sup["--disallowedTools"] {
				args = append(args, "--disallowedTools", "Write,Edit,Bash")
			}
			if sup["--append-system-prompt"] {
				args = append(args, "--append-system-prompt", system)
			} else {
				args[1] = system + "\n\n" + user
			}
			if sup["--max-turns"] {
				args = append(args, "--max-turns", "30")
			}
			if sup["--json-schema"] {
				args = append(args, "--json-schema", findingSchemaJSON)
			}
			if sup["--max-budget-usd"] && cfg.MaxCostUSD > 0 {
				args = append(args, "--max-budget-usd", fmt.Sprintf("%.2f", cfg.MaxCostUSD))
			}
			return args
		},
		authErrPatterns: []*regexp.Regexp{
			regexp.MustCompile(`(?i)(not logged in|please log ?in|invalid api key|authentication_error|OAuth token has expired)`),
		},
		denyPatterns: []*regexp.Regexp{
			regexp.MustCompile(`(?i)permission denied and no permission prompt available`),
		},
	}
}

// ---- cursor ----

func newCursor(cfg config.Adapter, ps *prompts.Set, runner Runner) Reviewer {
	return &cliAdapter{
		cfg: cfg, prompts: ps, runner: runner,
		binary:    "cursor-agent",
		wantFlags: []string{"--print", "--output-format", "--mode", "--force", "--trust", "--model"},
		buildArgs: func(sup map[string]bool, system, user string, cfg config.Adapter) []string {
			// No system-prompt flag on this surface: the lens rides in the
			// prompt text.
			args := []string{"-p", system + "\n\n" + user}
			if sup["--output-format"] {
				args = append(args, "--output-format", "json")
			}
			if cfg.Model != "" && sup["--model"] {
				args = append(args, "--model", cfg.Model)
			}
			if sup["--mode"] {
				args = append(args, "--mode", "ask") // read-only
			}
			// Headless runs stall on approval prompts without these.
			if sup["--force"] {
				args = append(args, "--force")
			}
			if sup["--trust"] {
				args = append(args, "--trust")
			}
			return args
		},
		authProbe: []string{"status"},
		authErrPatterns: []*regexp.Regexp{
			regexp.MustCompile(`(?i)(not logged in|unauthorized|invalid api key|please run.*login|authentication failed)`),
		},
	}
}

// ---- antigravity ----

func newAntigravity(cfg config.Adapter, ps *prompts.Set, runner Runner) Reviewer {
	return &cliAdapter{
		cfg: cfg, prompts: ps, runner: runner,
		binary: "agy",
		// Verified against the current agy surface: there is no
		// --output-format flag; findings are extracted from the text
		// response. Probe reports whatever is missing.
		wantFlags: []string{"--print", "--model", "--print-timeout"},
		buildArgs: func(sup map[string]bool, system, user string, cfg config.Adapter) []string {
			args := []string{"-p", system + "\n\n" + user}
			if cfg.Model != "" && sup["--model"] {
				args = append(args, "--model", cfg.Model)
			}
			if sup["--print-timeout"] && cfg.Timeout > 0 {
				args = append(args, "--print-timeout", cfg.Timeout.String())
			}
			return args
		},
		// Headless auth uses cached credentials; an unauthenticated run
		// exits with an auth error, and `agy models` needs auth, so it
		// doubles as the cheap probe.
		authProbe: []string{"models"},
		authErrPatterns: []*regexp.Regexp{
			regexp.MustCompile(`(?i)(not authenticated|authentication (error|required|failed)|please (sign|log) ?in|no cached credential)`),
		},
		// Shell commands default to Ask and are soft-denied headless: run
		// continues, exits 0, notice on stderr. That is a denied review,
		// not a successful one.
		denyPatterns: []*regexp.Regexp{
			regexp.MustCompile(`(?i)(soft[- ]den(y|ied)|command .*was denied|denied in (headless|non-interactive)|requires approval.*skipp?ed|permission request.*denied)`),
		},
	}
}
