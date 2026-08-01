// Package config loads mergejury.yaml, resolved from repo root then
// ~/.config/mergejury/, with environment overrides.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/iamEtornam/mergejury/internal/finding"
)

type Adapter struct {
	ID         string        `yaml:"id" json:"id"`
	Kind       string        `yaml:"kind" json:"kind"` // claude-code | cursor | antigravity | modelapi
	Lens       string        `yaml:"lens" json:"lens"`
	Model      string        `yaml:"model" json:"model"`
	Timeout    time.Duration `yaml:"timeout" json:"timeout"`
	MaxCostUSD float64       `yaml:"max_cost_usd" json:"max_cost_usd"`
}

type Gates struct {
	MaxChangedLines int      `yaml:"max_changed_lines" json:"max_changed_lines"`
	MaxChangedFiles int      `yaml:"max_changed_files" json:"max_changed_files"`
	SkipPaths       []string `yaml:"skip_paths" json:"skip_paths"`
}

type Verification struct {
	Commands map[string]string `yaml:"commands" json:"commands"`
}

type Challenger struct {
	Enabled     bool             `yaml:"enabled" json:"enabled"`
	MinSeverity finding.Severity `yaml:"min_severity" json:"min_severity"`
	Model       string           `yaml:"model" json:"model"`
}

type Judge struct {
	Model string `yaml:"model" json:"model"`
}

type Adjudication struct {
	ClusterWindowLines int        `yaml:"cluster_window_lines" json:"cluster_window_lines"`
	Challenger         Challenger `yaml:"challenger" json:"challenger"`
	Judge              Judge      `yaml:"judge" json:"judge"`
}

type Posting struct {
	MaxInlineComments int              `yaml:"max_inline_comments" json:"max_inline_comments"`
	MinSeverity       finding.Severity `yaml:"min_severity" json:"min_severity"`
	DryRun            bool             `yaml:"dry_run" json:"dry_run"`
	FooterAttribution bool             `yaml:"footer_attribution" json:"footer_attribution"`
}

type Verdict struct {
	Enabled          bool             `yaml:"enabled" json:"enabled"`
	RequestChangesAt finding.Severity `yaml:"request_changes_at" json:"request_changes_at"`
	ApproveOnClean   bool             `yaml:"approve_on_clean" json:"approve_on_clean"`
	ApproveForks     bool             `yaml:"approve_forks" json:"approve_forks"`
	// Not configurable by design: a degraded run never approves.
}

type Config struct {
	Adapters     []Adapter    `yaml:"adapters" json:"adapters"`
	Gates        Gates        `yaml:"gates" json:"gates"`
	Verification Verification `yaml:"verification" json:"verification"`
	Adjudication Adjudication `yaml:"adjudication" json:"adjudication"`
	Posting      Posting      `yaml:"posting" json:"posting"`
	Verdict      Verdict      `yaml:"verdict" json:"verdict"`
	DBPath       string       `yaml:"db_path" json:"db_path"`
	PromptsDir   string       `yaml:"prompts_dir" json:"prompts_dir"` // overrides embedded prompts when set
}

func Default() Config {
	return Config{
		Adapters: []Adapter{
			{ID: "api-baseline", Kind: "modelapi", Lens: "correctness", Model: "claude-sonnet-5", Timeout: 5 * time.Minute},
		},
		Gates: Gates{
			MaxChangedLines: 1500,
			MaxChangedFiles: 60,
			SkipPaths:       []string{"**/*.lock", "**/go.sum", "**/generated/**", "**/*.pb.go", "**/vendor/**"},
		},
		Adjudication: Adjudication{
			ClusterWindowLines: 3,
			Challenger:         Challenger{Enabled: true, MinSeverity: finding.SevMajor, Model: "claude-opus-5"},
			Judge:              Judge{Model: "claude-opus-5"},
		},
		Posting: Posting{
			MaxInlineComments: 10,
			MinSeverity:       finding.SevMinor,
			FooterAttribution: true,
		},
		Verdict: Verdict{
			Enabled:          true,
			RequestChangesAt: finding.SevBlocker,
			ApproveOnClean:   true,
			ApproveForks:     false,
		},
	}
}

// Load resolves the config: defaults, then ~/.config/mergejury/mergejury.yaml, then
// <startDir>/mergejury.yaml, then environment overrides.
func Load(startDir string) (Config, error) {
	cfg := Default()
	if home, err := os.UserHomeDir(); err == nil {
		if err := mergeFile(&cfg, filepath.Join(home, ".config", "mergejury", "mergejury.yaml")); err != nil {
			return cfg, err
		}
	}
	if startDir != "" {
		if err := mergeFile(&cfg, filepath.Join(startDir, "mergejury.yaml")); err != nil {
			return cfg, err
		}
	}
	applyEnv(&cfg)
	if cfg.DBPath == "" {
		cfg.DBPath = defaultDBPath()
	}
	for i := range cfg.Adapters {
		if cfg.Adapters[i].Timeout == 0 {
			cfg.Adapters[i].Timeout = 5 * time.Minute
		}
	}
	return cfg, nil
}

func mergeFile(cfg *Config, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := yaml.Unmarshal(b, cfg); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("MERGEJURY_DB"); v != "" {
		cfg.DBPath = v
	}
	if v := os.Getenv("MERGEJURY_PROMPTS_DIR"); v != "" {
		cfg.PromptsDir = v
	}
	if v := os.Getenv("MERGEJURY_DRY_RUN"); v == "1" || v == "true" {
		cfg.Posting.DryRun = true
	}
}

func defaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "mergejury.db"
	}
	dir := filepath.Join(home, ".local", "share", "mergejury")
	_ = os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, "mergejury.db")
}

// Snapshot serializes the resolved config for runs.config_snapshot, so a
// quality change can be traced to a prompt edit versus a config change.
func (c Config) Snapshot() string {
	b, err := json.Marshal(c)
	if err != nil {
		return "{}"
	}
	return string(b)
}
