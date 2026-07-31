package adapter

import (
	"context"
	"strings"
	"testing"
	"time"

	"mergejury/internal/config"
	"mergejury/internal/packet"
	"mergejury/prompts"
)

const helpAll = `Usage:
  --print, -p
  --output-format <format>
  --allowedTools, --allowed-tools <tools...>
  --disallowedTools, --disallowed-tools <tools...>
  --append-system-prompt <prompt>
  --max-turns <n>
  --json-schema <schema>
  --max-budget-usd <amount>
  --model <model>
  --mode <mode>
  --force
  --trust
  --print-timeout <d>
`

const validFindings = `{"findings":[{"path":"main.go","line":12,"start_line":null,"category":"bug","severity":"major","title":"t","body":"b","suggested_patch":null,"evidence":["main.go:12"],"confidence":"high"}],"omissions":[]}`

// fakeRunner scripts subprocess behaviour per invocation. --help calls get
// the canned help; everything else pops the next scripted result.
type fakeRunner struct {
	results []ExecResult
	calls   [][]string
	help    string
	block   bool // review calls block until ctx is done (timeout case)
}

func (f *fakeRunner) run(ctx context.Context, dir string, env []string, name string, args ...string) ExecResult {
	f.calls = append(f.calls, append([]string{name}, args...))
	if len(args) == 1 && args[0] == "--help" {
		return ExecResult{Stdout: f.help}
	}
	if f.block {
		<-ctx.Done()
		return ExecResult{TimedOut: true}
	}
	if len(f.results) == 0 {
		return ExecResult{}
	}
	r := f.results[0]
	f.results = f.results[1:]
	return r
}

func testAdapter(t *testing.T, kind string, fr *fakeRunner) Reviewer {
	t.Helper()
	if fr.help == "" {
		fr.help = helpAll
	}
	cfg := config.Adapter{ID: "test-" + kind, Kind: kind, Lens: "correctness", Model: "m", Timeout: time.Minute}
	r, err := New(cfg, prompts.New(""), fr.run, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func pkt() *packet.Packet {
	p := &packet.Packet{Title: "t", Body: "b"}
	p.Build()
	return p
}

func TestValidJSON(t *testing.T) {
	fr := &fakeRunner{results: []ExecResult{{Stdout: validFindings}}}
	res := testAdapter(t, "claude-code", fr).Review(context.Background(), pkt(), "")
	if res.Status != StatusOK || len(res.Findings) != 1 {
		t.Fatalf("status=%s findings=%d err=%s", res.Status, len(res.Findings), res.Err)
	}
	if res.Findings[0].ReviewerID != "test-claude-code" {
		t.Errorf("reviewer id not stamped: %q", res.Findings[0].ReviewerID)
	}
}

func TestJSONInMarkdownFences(t *testing.T) {
	fenced := "Here is my review:\n```json\n" + validFindings + "\n```\n"
	fr := &fakeRunner{results: []ExecResult{{Stdout: fenced}}}
	res := testAdapter(t, "antigravity", fr).Review(context.Background(), pkt(), "")
	if res.Status != StatusOK || len(res.Findings) != 1 {
		t.Fatalf("status=%s findings=%d err=%s", res.Status, len(res.Findings), res.Err)
	}
}

func TestEnvelopeWithResultString(t *testing.T) {
	env := `{"type":"result","is_error":false,"result":"` + strings.ReplaceAll(validFindings, `"`, `\"`) + `","total_cost_usd":0.42,"usage":{"input_tokens":100,"output_tokens":50}}`
	fr := &fakeRunner{results: []ExecResult{{Stdout: env}}}
	res := testAdapter(t, "claude-code", fr).Review(context.Background(), pkt(), "")
	if res.Status != StatusOK || len(res.Findings) != 1 {
		t.Fatalf("status=%s findings=%d err=%s", res.Status, len(res.Findings), res.Err)
	}
	if res.CostUSD != 0.42 || res.Tokens.Input != 100 {
		t.Errorf("cost/tokens not extracted: %+v", res)
	}
}

func TestStructuredOutputPreferred(t *testing.T) {
	env := `{"result":"ignore this prose","structured_output":` + validFindings + `,"total_cost_usd":0.1}`
	fr := &fakeRunner{results: []ExecResult{{Stdout: env}}}
	res := testAdapter(t, "claude-code", fr).Review(context.Background(), pkt(), "")
	if res.Status != StatusOK || len(res.Findings) != 1 {
		t.Fatalf("status=%s findings=%d err=%s", res.Status, len(res.Findings), res.Err)
	}
}

func TestTruncatedJSON(t *testing.T) {
	fr := &fakeRunner{results: []ExecResult{{Stdout: validFindings[:40]}}}
	res := testAdapter(t, "claude-code", fr).Review(context.Background(), pkt(), "")
	if res.Status != StatusParseError {
		t.Fatalf("want parse_error, got %s", res.Status)
	}
	if res.Raw == "" {
		t.Error("raw output must be persisted even on failure")
	}
}

func TestEmptyFindingsIsFirstClass(t *testing.T) {
	fr := &fakeRunner{results: []ExecResult{{Stdout: `{"findings":[],"omissions":["could not assess generated code"]}`}}}
	res := testAdapter(t, "claude-code", fr).Review(context.Background(), pkt(), "")
	if res.Status != StatusOK {
		t.Fatalf("empty findings must be OK, got %s (%s)", res.Status, res.Err)
	}
	if len(res.Findings) != 0 || len(res.Omissions) != 1 {
		t.Errorf("findings=%d omissions=%d", len(res.Findings), len(res.Omissions))
	}
}

func TestAuthErrorOnStderrWithExitZero(t *testing.T) {
	fr := &fakeRunner{results: []ExecResult{{Stdout: "", Stderr: "Error: Not logged in. Please run login.", ExitCode: 0}}}
	res := testAdapter(t, "cursor", fr).Review(context.Background(), pkt(), "")
	if res.Status != StatusAuthError {
		t.Fatalf("want auth_error, got %s", res.Status)
	}
	if StatusAuthError.Retryable() {
		t.Error("auth_error must never be retryable")
	}
}

func TestAntigravitySoftDenyOnStderrWithExitZero(t *testing.T) {
	fr := &fakeRunner{results: []ExecResult{{
		Stdout:   `{"findings":[]}`,
		Stderr:   "notice: shell command 'go test ./...' was denied in headless mode",
		ExitCode: 0,
	}}}
	res := testAdapter(t, "antigravity", fr).Review(context.Background(), pkt(), "")
	if res.Status != StatusDenied {
		t.Fatalf("want denied, got %s", res.Status)
	}
	if StatusDenied.Retryable() {
		t.Error("denied must never be retryable")
	}
}

func TestTimeout(t *testing.T) {
	fr := &fakeRunner{block: true}
	cfg := config.Adapter{ID: "a", Kind: "claude-code", Lens: "correctness", Timeout: 20 * time.Millisecond}
	r, err := New(cfg, prompts.New(""), fr.run, nil)
	if err != nil {
		t.Fatal(err)
	}
	res := r.Review(context.Background(), pkt(), "")
	if res.Status != StatusTimeout {
		t.Fatalf("want timeout, got %s", res.Status)
	}
}

func TestFlagGating(t *testing.T) {
	// agy's real surface: no --output-format, no --append-system-prompt.
	fr := &fakeRunner{
		help:    "--print\n--model <m>\n--print-timeout <d>\n",
		results: []ExecResult{{Stdout: validFindings}},
	}
	r := testAdapter(t, "antigravity", fr)
	res := r.Review(context.Background(), pkt(), "")
	if res.Status != StatusOK {
		t.Fatalf("status=%s err=%s", res.Status, res.Err)
	}
	last := fr.calls[len(fr.calls)-1]
	joined := strings.Join(last, " ")
	if strings.Contains(joined, "--output-format") {
		t.Errorf("unsupported flag passed to agy: %v", last)
	}
	if !strings.Contains(joined, "--print-timeout") {
		t.Errorf("supported flag not passed: %v", last)
	}
	// Lens must ride inside the prompt when no system-prompt flag exists.
	if !strings.Contains(joined, "correctness") {
		t.Errorf("lens prompt missing from args")
	}
}

func TestCrashedNonzeroExit(t *testing.T) {
	fr := &fakeRunner{results: []ExecResult{{Stderr: "segfault", ExitCode: 139}}}
	res := testAdapter(t, "claude-code", fr).Review(context.Background(), pkt(), "")
	if res.Status != StatusCrashed {
		t.Fatalf("want crashed, got %s", res.Status)
	}
	if !StatusCrashed.Retryable() {
		t.Error("crashed should be retryable once")
	}
}
