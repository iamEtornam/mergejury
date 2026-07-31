package adapter

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
)

// ExecResult is what a subprocess produced.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	TimedOut bool
	Err      error // start/other failures; nil for a clean nonzero exit
}

// Runner is the process-spawn seam: tests supply canned stdout, stderr and
// exit codes; production uses ExecRunner.
type Runner func(ctx context.Context, dir string, extraEnv []string, name string, args ...string) ExecResult

// ExecRunner runs the real binary. Reviewers never hold the GitHub write
// token (section 10): forge credentials are scrubbed from the subprocess
// environment.
func ExecRunner(ctx context.Context, dir string, extraEnv []string, name string, args ...string) ExecResult {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	env := make([]string, 0, len(os.Environ())+len(extraEnv))
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GITHUB_TOKEN=") || strings.HasPrefix(kv, "GH_TOKEN=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env, extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		return res
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		res.Err = err
	}
	return res
}
