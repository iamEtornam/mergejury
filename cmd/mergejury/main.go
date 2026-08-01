// Command mergejury is the CLI front end: reviews, adapter checks, run
// inspection, replay, stats, and the local web console.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamEtornam/mergejury/internal/adapter"
	"github.com/iamEtornam/mergejury/internal/anthropic"
	"github.com/iamEtornam/mergejury/internal/config"
	"github.com/iamEtornam/mergejury/internal/forge"
	"github.com/iamEtornam/mergejury/internal/httpapi"
	"github.com/iamEtornam/mergejury/internal/packet"
	"github.com/iamEtornam/mergejury/internal/run"
	"github.com/iamEtornam/mergejury/internal/store"
	"github.com/iamEtornam/mergejury/prompts"
)

// Exit codes per section 12.
const (
	exitOK          = 0
	exitCouldNotRun = 1
	exitDegraded    = 2
	exitConfigAuth  = 3
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	root := newRoot()
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		if ec, ok := err.(exitCoder); ok {
			os.Exit(ec.code)
		}
		os.Exit(exitCouldNotRun)
	}
}

type exitCoder struct {
	error
	code int
}

func withCode(err error, code int) error {
	if err == nil {
		return nil
	}
	return exitCoder{err, code}
}

type app struct {
	cfg config.Config
	st  *store.Store
	ps  *prompts.Set
}

func setup() (*app, error) {
	wd, _ := os.Getwd()
	cfg, err := config.Load(wd)
	if err != nil {
		return nil, withCode(err, exitConfigAuth)
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return nil, withCode(fmt.Errorf("open store %s: %w", cfg.DBPath, err), exitConfigAuth)
	}
	return &app{cfg: cfg, st: st, ps: prompts.New(cfg.PromptsDir)}, nil
}

func (a *app) orchestrator(withForge bool) (*run.Orchestrator, error) {
	o := &run.Orchestrator{
		Cfg:     a.cfg,
		Store:   a.st,
		Prompts: a.ps,
		Client:  anthropic.NewFromEnv(),
		Runner:  adapter.ExecRunner,
		Events:  run.NewBroker(),
	}
	if withForge {
		gh, err := forge.NewGitHubFromEnv()
		if err != nil {
			return nil, withCode(err, exitConfigAuth)
		}
		o.Forge = gh
	}
	return o, nil
}

// version is stamped by the release build via
// -ldflags "-X main.version=v1.2.3". For `go install` builds it stays
// "dev" and the module version from the build info is used instead.
var version = "dev"

func versionString() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return version
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "mergejury",
		Short:         "multi-reviewer code review harness",
		Version:       versionString(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newInitCmd(), newReviewCmd(), newAdaptersCmd(), newRunsCmd(), newStatsCmd(), newServeCmd())
	return root
}

// ---- mergejury review ----

var prURLRe = regexp.MustCompile(`github\.com/([^/]+/[^/]+)/pull/(\d+)`)

func parsePRRef(ctx context.Context, arg string) (repo string, number int, err error) {
	if m := prURLRe.FindStringSubmatch(arg); m != nil {
		n, _ := strconv.Atoi(m[2])
		return m[1], n, nil
	}
	n, aerr := strconv.Atoi(arg)
	if aerr != nil {
		return "", 0, fmt.Errorf("%q is neither a PR URL nor a number", arg)
	}
	// Bare number: derive owner/repo from origin.
	wd, _ := os.Getwd()
	out, gerr := packet.Git(ctx, wd, "remote", "get-url", "origin")
	if gerr != nil {
		return "", 0, fmt.Errorf("bare PR number needs a git repo with an origin remote: %w", gerr)
	}
	repo = parseRepoFromRemote(strings.TrimSpace(out))
	if repo == "" {
		return "", 0, fmt.Errorf("cannot derive owner/repo from origin %q", strings.TrimSpace(out))
	}
	return repo, n, nil
}

func parseRepoFromRemote(url string) string {
	url = strings.TrimSuffix(url, ".git")
	if i := strings.Index(url, "github.com/"); i >= 0 {
		return url[i+len("github.com/"):]
	}
	if i := strings.Index(url, "github.com:"); i >= 0 {
		return url[i+len("github.com:"):]
	}
	return ""
}

func newReviewCmd() *cobra.Command {
	var (
		adaptersFlag string
		dryRun       bool
		noChallenger bool
		noVerdict    bool
		maxComments  int
		jsonOut      bool
		local        bool
		baseRef      string
	)
	cmd := &cobra.Command{
		Use:   "review [pr-url|number]",
		Short: "run a full review against a PR, or --local for the working tree",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := setup()
			if err != nil {
				return err
			}
			defer a.st.Close()
			opts := run.Options{
				DryRun:       dryRun,
				NoChallenger: noChallenger,
				NoVerdict:    noVerdict,
				MaxComments:  maxComments,
				Trigger:      "cli",
			}
			if adaptersFlag != "" {
				opts.Adapters = strings.Split(adaptersFlag, ",")
			}
			var sum *run.Summary
			if local {
				o, oerr := a.orchestrator(false)
				if oerr != nil {
					return oerr
				}
				wd, _ := os.Getwd()
				sum, err = o.ReviewLocal(cmd.Context(), wd, baseRef, opts)
			} else {
				if len(args) != 1 {
					return withCode(fmt.Errorf("need a PR URL or number (or --local)"), exitCouldNotRun)
				}
				repo, num, perr := parsePRRef(cmd.Context(), args[0])
				if perr != nil {
					return withCode(perr, exitCouldNotRun)
				}
				o, oerr := a.orchestrator(true)
				if oerr != nil {
					return oerr
				}
				sum, err = o.ReviewPR(cmd.Context(), repo, num, opts)
			}
			if err != nil {
				return withCode(err, exitCouldNotRun)
			}
			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				enc.Encode(sum)
			} else {
				printSummary(sum)
			}
			if sum.Status == "degraded" {
				return withCode(fmt.Errorf("run completed with adapter failures"), exitDegraded)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&adaptersFlag, "adapters", "", "comma-separated subset of adapter ids")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "render the review, do not post")
	cmd.Flags().BoolVar(&noChallenger, "no-challenger", false, "skip the adversarial pass")
	cmd.Flags().BoolVar(&noVerdict, "no-verdict", false, "force event COMMENT for this run")
	cmd.Flags().IntVar(&maxComments, "max-comments", 0, "inline comment cap override")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "machine-readable result for CI")
	cmd.Flags().BoolVar(&local, "local", false, "review the working tree against a base ref; no PR, no posting")
	cmd.Flags().StringVar(&baseRef, "base", "main", "base ref for --local")
	return cmd
}

func printSummary(s *run.Summary) {
	fmt.Printf("run %d: %s\n", s.RunID, s.Status)
	for id, st := range s.AdapterStatus {
		fmt.Printf("  %-20s %s\n", id, st)
	}
	fmt.Printf("findings: %d produced, %d kept, %d published, %d needs-human\n",
		s.Produced, s.Kept, s.Published, s.NeedsHuman)
	if s.Event != "" {
		fmt.Printf("event: %s (%s)\n", s.Event, s.EventReason)
	}
	fmt.Printf("cost: $%.4f\n", s.TotalCostUSD)
	if s.RenderedReview != "" {
		fmt.Println()
		fmt.Println(s.RenderedReview)
	}
	if s.Posted {
		fmt.Printf("posted review %d\n", s.ReviewID)
	}
}

// ---- mergejury adapters check ----

func newAdaptersCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "adapters", Short: "adapter tooling"}
	check := &cobra.Command{
		Use:   "check",
		Short: "probe every configured adapter for install, auth, and expected flags",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := setup()
			if err != nil {
				return err
			}
			defer a.st.Close()
			client := anthropic.NewFromEnv()
			w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ADAPTER\tKIND\tLENS\tSTATUS\tDETAIL")
			anyFail := false
			for _, ac := range a.cfg.Adapters {
				rev, aerr := adapter.New(ac, a.ps, adapter.ExecRunner, client)
				if aerr != nil {
					fmt.Fprintf(w, "%s\t%s\t%s\tFAIL\t%v\n", ac.ID, ac.Kind, ac.Lens, aerr)
					anyFail = true
					continue
				}
				pr := rev.Probe(cmd.Context())
				status := "ok"
				detail := pr.Detail
				if !pr.OK {
					status = "FAIL"
					anyFail = true
					if pr.Remediation != "" {
						detail += " → " + pr.Remediation
					}
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", ac.ID, ac.Kind, ac.Lens, status, detail)
			}
			w.Flush()
			if anyFail {
				return withCode(fmt.Errorf("one or more adapters failed the probe"), exitConfigAuth)
			}
			return nil
		},
	}
	cmd.AddCommand(check)
	return cmd
}

// ---- mergejury runs ----

func newRunsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "runs", Short: "inspect and replay stored runs"}

	var listRepo string
	var listLimit int
	list := &cobra.Command{
		Use:   "list",
		Short: "list stored runs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := setup()
			if err != nil {
				return err
			}
			defer a.st.Close()
			runs, err := a.st.ListRuns(listRepo, listLimit)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tREPO\tPR\tSTATUS\tEVENT\tPOSTED/PRODUCED\tCOST\tSTARTED")
			for _, r := range runs {
				fmt.Fprintf(w, "%d\t%s\t%d\t%s\t%s\t%d/%d\t$%.4f\t%s\n",
					r.ID, r.Repo, r.PRNumber, r.Status, r.ReviewEvent, r.CommentsPosted, r.FindingsProduced, r.TotalCostUSD, r.StartedAt)
			}
			return w.Flush()
		},
	}
	list.Flags().StringVar(&listRepo, "repo", "", "filter by repo")
	list.Flags().IntVar(&listLimit, "limit", 50, "max rows")

	var showAdapter string
	var showRaw bool
	show := &cobra.Command{
		Use:   "show <id>",
		Short: "full run detail, including dropped findings",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := setup()
			if err != nil {
				return err
			}
			defer a.st.Close()
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return err
			}
			return showRun(a.st, id, showAdapter, showRaw)
		},
	}
	show.Flags().StringVar(&showAdapter, "adapter", "", "filter to one adapter")
	show.Flags().BoolVar(&showRaw, "raw", false, "print raw adapter output")

	replay := &cobra.Command{
		Use:   "replay <id>",
		Short: "re-run cluster/challenge/judge on stored findings; no adapter invocations",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := setup()
			if err != nil {
				return err
			}
			defer a.st.Close()
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return err
			}
			o, oerr := a.orchestrator(false)
			if oerr != nil {
				return oerr
			}
			sum, err := o.Replay(cmd.Context(), id, run.Options{Trigger: "replay"})
			if err != nil {
				return err
			}
			printSummary(sum)
			return nil
		},
	}

	cmd.AddCommand(list, show, replay)
	return cmd
}

func showRun(st *store.Store, id int64, adapterFilter string, raw bool) error {
	r, err := st.GetRun(id)
	if err != nil {
		return fmt.Errorf("run %d: %w", id, err)
	}
	fmt.Printf("run %d  %s#%d  head %s  %s  event=%s\n", r.ID, r.Repo, r.PRNumber, short(r.HeadSHA), r.Status, r.ReviewEvent)
	if r.ReviewEventReason != "" {
		fmt.Printf("reason: %s\n", r.ReviewEventReason)
	}
	ars, err := st.AdapterRunsForRun(id)
	if err != nil {
		return err
	}
	fmt.Println("\nadapters:")
	for _, ar := range ars {
		if adapterFilter != "" && ar.AdapterID != adapterFilter {
			continue
		}
		fmt.Printf("  %-20s %-12s lens=%-14s %6dms  $%.4f", ar.AdapterID, ar.Status, ar.Lens, ar.DurationMS, ar.CostUSD)
		if ar.Error != "" {
			fmt.Printf("  err=%s", ar.Error)
		}
		fmt.Println()
		if raw {
			fmt.Println("  --- raw ---")
			fmt.Println(indent(ar.RawOutput, "  | "))
		}
	}
	findings, err := st.FindingsForRun(id, false)
	if err != nil {
		return err
	}
	fmt.Println("\nfindings:")
	for _, f := range findings {
		if adapterFilter != "" && f.AdapterID != adapterFilter {
			continue
		}
		fate := "kept"
		if !f.Kept {
			fate = "dropped:" + f.DropReason
		} else if f.DropReason != "" {
			fate = "demoted:" + f.DropReason
		}
		fmt.Printf("  [%d] %-10s %-8s %s:%d %s (%s, by %s)\n", f.ID, fate, f.Finding.Severity, f.Finding.Path, f.Finding.Line, f.Finding.Title, f.Finding.Category, f.AdapterID)
	}
	clusters, _ := st.ClustersForRun(id)
	verdicts, _ := st.VerdictsForRun(id)
	verdictByCluster := map[int64]store.StoredVerdict{}
	for _, v := range verdicts {
		verdictByCluster[v.ClusterID] = v
	}
	if len(clusters) > 0 {
		fmt.Println("\nclusters:")
		for _, c := range clusters {
			v, ok := verdictByCluster[c.ID]
			verdict := "(no verdict)"
			if ok {
				verdict = v.Verdict + ": " + v.Reason
			}
			fmt.Printf("  [%d] %s:%d %s supporters=%d → %s\n", c.ID, c.Path, c.Line, c.Category, c.SupporterCount, verdict)
		}
	}
	return nil
}

func short(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

// ---- mergejury stats ----

func newStatsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stats",
		Short: "precision, cost, latency per adapter and lens",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := setup()
			if err != nil {
				return err
			}
			defer a.st.Close()
			stats, err := a.st.Stats()
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ADAPTER\tLENS\tRUNS\tPRODUCED\tKEPT\tPUBLISHED\tRESOLVED\tDISMISSED\tMED LATENCY\tCOST\t$/PUBLISHED")
			for _, s := range stats {
				perPub := "-"
				if s.Published > 0 {
					perPub = fmt.Sprintf("$%.4f", s.CostPerPublished)
				}
				fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%dms\t$%.4f\t%s\n",
					s.AdapterID, s.Lens, s.Runs, s.FindingsProduced, s.FindingsKept, s.Published, s.Resolved, s.Dismissed, s.MedianLatencyMS, s.TotalCostUSD, perPub)
			}
			return w.Flush()
		},
	}
}

// ---- mergejury serve ----

func newServeCmd() *cobra.Command {
	var addr string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "start the local operator console",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := setup()
			if err != nil {
				return err
			}
			defer a.st.Close()
			o, _ := a.orchestrator(false) // forge attached lazily per run
			if gh, gerr := forge.NewGitHubFromEnv(); gerr == nil {
				o.Forge = gh
				// Outcomes feedback loop: poll recent PRs for what happened
				// to posted comments. Noisy proxy for correctness, good
				// enough for the scoreboard over a few hundred comments.
				go func() {
					ticker := time.NewTicker(30 * time.Minute)
					defer ticker.Stop()
					for {
						select {
						case <-cmd.Context().Done():
							return
						case <-ticker.C:
							runs, err := a.st.ListRuns("", 50)
							if err != nil {
								continue
							}
							for _, r := range runs {
								if r.CommentsPosted > 0 {
									_ = o.SyncOutcomes(cmd.Context(), r.ID)
								}
							}
						}
					}
				}()
			}
			srv := httpapi.New(o, a.st, a.ps)
			fmt.Printf("mergejury console on http://%s\n", addr)
			return srv.ListenAndServe(cmd.Context(), addr)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:7777", "listen address (localhost by default; this is an operator console)")
	return cmd
}
