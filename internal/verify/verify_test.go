package verify

import (
	"context"
	"testing"

	"github.com/iamEtornam/mergejury/internal/cluster"
)

func TestRunCommandTreatsPathAsOneLiteralArgument(t *testing.T) {
	for _, path := range []string{"file with spaces.go", "safe; printf INJECTED", "$(printf INJECTED).go", "a'\"$file\nname.go"} {
		t.Run(path, func(t *testing.T) {
			got := runCommand(context.Background(), t.TempDir(), "lint", "printf '<%s>' {{.Paths}}", &cluster.Cluster{Path: path})
			if got.ExitCode != 0 || got.Output != "<"+path+">" {
				t.Fatalf("path interpreted by shell: exit=%d output=%q", got.ExitCode, got.Output)
			}
		})
	}
}

func TestRunCommandScrubsForgeTokens(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "github-test-token")
	t.Setenv("GH_TOKEN", "gh-test-token")
	got := runCommand(context.Background(), t.TempDir(), "lint", `printf '%s:%s' "${GITHUB_TOKEN-unset}" "${GH_TOKEN-unset}"`, &cluster.Cluster{Path: "file.go"})
	if got.Output != "unset:unset" {
		t.Fatalf("forge credentials inherited: %q", got.Output)
	}
}

func TestRunCommandReportsNonzeroExit(t *testing.T) {
	got := runCommand(context.Background(), t.TempDir(), "lint", "printf diagnostic >&2; exit 7", &cluster.Cluster{})
	if got.ExitCode != 7 || got.Output != "diagnostic" || got.Conclusion != ConcludeInconclusive {
		t.Fatalf("lost command failure: %+v", got)
	}
}
