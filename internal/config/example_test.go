package config

import (
	"os"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// The example config is documentation users copy verbatim. It must parse.
func TestExampleConfigParses(t *testing.T) {
	b, err := os.ReadFile("../../mergejury.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		t.Fatalf("mergejury.example.yaml does not parse: %v", err)
	}
	if len(c.Adapters) == 0 {
		t.Fatal("no adapters parsed")
	}
	if c.Adapters[0].Timeout != 5*time.Minute {
		t.Errorf("timeout = %v, want 5m", c.Adapters[0].Timeout)
	}
}
